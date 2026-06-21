package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"time"

	block "github.com/scaleway/scaleway-sdk-go/api/block/v1alpha1"
	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// BigFleet server-tag keys. The bigfleet:managed tag marks our servers so
// DescribeManaged never touches anything else; the rest let inventory and
// bindings be recovered from Scaleway alone after a restart. Scaleway tags are
// free-form strings, so we use "key=value" tags and parse them back.
const (
	tagManaged   = "bigfleet:managed"
	tagMachineID = "bigfleet:machine-id="
	tagCluster   = "bigfleet:cluster="
)

// scwCredentials carries the Scaleway API-key auth surface (access key + secret
// key + project), read from flags or the SCW_* environment. The region/zone is
// the substrate this process serves.
type scwCredentials struct {
	accessKey string
	secretKey string
	projectID string
	region    string // a zone like fr-par-1; the region is derived from it
}

// complete reports whether the full Scaleway credential set (access key, secret
// key, and project id) is present to talk to the real API. Used by
// `--scaleway-backend=auto` to fall back to the fake when credentials are not
// fully configured (the credential-free certification path). The project id is
// required up front so a missing one fails fast at startup rather than as a
// confusing runtime CreateServer error.
func (c scwCredentials) complete() bool {
	return c.accessKey != "" && c.secretKey != "" && c.projectID != ""
}

// scwRealConfig is the launch configuration for the production Scaleway client.
type scwRealConfig struct {
	Creds scwCredentials
	// CommercialKind selects the substrate: CapacityOnDemand → Instances,
	// CapacityBareMetal → Elastic Metal. This client implements the Instances
	// path; an Elastic Metal client is selected by main when CommercialKind is
	// BareMetal (see newSCWReal).
	CommercialKind providerkit.CapacityType
	Image          string
	Zone           string
	EURtoUSD       float64

	// Vault is the on-host agent control channel used by ApplyBootstrap /
	// DrainNode (Scaleway has no in-guest command API). Required.
	Vault *bootstrapVault
	// BootstrapEndpoint is the externally-reachable URL of the provider's
	// bootstrap channel (e.g. https://scaleway-provider.example:9443). It is
	// injected into the server's generic user_data so the agent knows where to
	// fetch.
	BootstrapEndpoint string
	// BootstrapCAPEM is the PEM the agent pins to verify the provider's server
	// certificate — the agent side of the mutual authentication.
	BootstrapCAPEM string

	// CreateWaitTimeout caps how long CreateServer waits for the server to reach
	// 'running' (the kit's Create timeout, carried on ctx, usually fires first).
	CreateWaitTimeout time.Duration
	// PollInterval is how often CreateServer polls the server status.
	PollInterval time.Duration
}

func (c *scwRealConfig) withDefaults() {
	if c.EURtoUSD <= 0 {
		c.EURtoUSD = defaultEURtoUSD
	}
	if c.CreateWaitTimeout <= 0 {
		c.CreateWaitTimeout = 10 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 3 * time.Second
	}
}

// scwReal is the production Instances scwClient, backed by scaleway-sdk-go.
// Inventory and bindings are recovered from server tags; the cluster-specific
// bootstrap and the drain are delivered over the on-host agent's
// mutually-authenticated TLS channel (Scaleway user-data is consumed only at
// first boot), so the base image must ship the agent that the generic
// Create-time user_data configures.
type scwReal struct {
	cfg    scwRealConfig
	zone   scw.Zone
	api    *instance.API
	block  *block.API
	vault  *bootstrapVault
	logger *slog.Logger

	// typesCache memoises the (single-zone) server-type catalogue for a short TTL.
	// pricing.refresh calls PriceUSD once per (type, zone) pair, so without this a
	// refresh of N offerings would trigger N full-catalogue scans; the cache
	// collapses them to one per refresh run.
	typesMu     sync.Mutex
	typesCache  map[string]*instance.ServerType
	typesExpiry time.Time
}

// serverTypesTTL bounds how long a fetched server-type catalogue is reused.
// Specs/prices change rarely, and the price refresher already runs on its own
// interval, so a minute is plenty to collapse a burst of per-pair lookups.
const serverTypesTTL = time.Minute

// newSCWReal builds the production client for the configured substrate. The
// Elastic Metal path is reported as unsupported-in-binary here so that a misbuilt
// deployment fails loudly rather than silently provisioning Instances for a
// bare-metal offering; the Instances path is fully implemented.
func newSCWReal(cfg scwRealConfig, logger *slog.Logger) (scwClient, error) {
	if !cfg.Creds.complete() {
		return nil, fmt.Errorf("scaleway: access key, secret key, and project id (SCW_ACCESS_KEY / SCW_SECRET_KEY / SCW_DEFAULT_PROJECT_ID) are all required for the scaleway backend")
	}
	if cfg.Image == "" {
		return nil, fmt.Errorf("scaleway: --image is required for the scaleway backend")
	}
	if cfg.Vault == nil {
		return nil, fmt.Errorf("scaleway: the bootstrap agent channel is required (configure --bootstrap-addr + --bootstrap-tls-cert/key)")
	}
	if cfg.BootstrapEndpoint == "" {
		return nil, fmt.Errorf("scaleway: --bootstrap-endpoint is required so the on-host agent can reach the provider")
	}
	cfg.withDefaults()

	zone, err := scw.ParseZone(cfg.Zone)
	if err != nil {
		return nil, fmt.Errorf("scaleway: parse zone %q: %w", cfg.Zone, err)
	}

	opts := []scw.ClientOption{
		scw.WithAuth(cfg.Creds.accessKey, cfg.Creds.secretKey),
		scw.WithDefaultZone(zone),
	}
	if cfg.Creds.projectID != "" {
		opts = append(opts, scw.WithDefaultProjectID(cfg.Creds.projectID))
	}
	client, err := scw.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("scaleway: build client: %w", err)
	}

	if cfg.CommercialKind == providerkit.CapacityBareMetal {
		// Elastic Metal uses a distinct two-step provisioning flow (CreateServer +
		// InstallServer + WaitForServerInstall) on the baremetal/v1 API. It shares
		// this client's auth and the same bootstrap-delivery model, but is a
		// separate adapter; see docs/configuration.md. Until that adapter ships,
		// refuse rather than mis-provisioning Instances under a bare-metal offering.
		return nil, fmt.Errorf("scaleway: the real Elastic Metal backend is not built into this binary; run --substrate=elastic-metal against the fake, or use the Instances substrate")
	}

	return &scwReal{
		cfg:    cfg,
		zone:   zone,
		api:    instance.NewAPI(client),
		block:  block.NewAPI(client),
		vault:  cfg.Vault,
		logger: logger,
	}, nil
}

func (r *scwReal) CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error) {
	name := serverName(spec)
	// Bake the generic, pre-binding agent bootstrap into user_data: the operator's
	// base user-data (installs/starts the agent) plus the agent config carrying
	// this server's per-machine token and the provider's pinned endpoint/CA. The
	// cluster-specific blob is delivered later over the agent channel, because
	// user_data is consumed only at first boot. Computed before the recovery
	// pre-check so a recovered, not-yet-booted server can have it (re-)applied.
	token := r.vault.Token(spec.MachineID)
	agentCfg := agentCloudConfig(r.cfg.BootstrapEndpoint, r.cfg.BootstrapCAPEM, spec.MachineID, token)
	userData, err := combineUserData(spec.BaseUserData, agentCfg)
	if err != nil {
		return serverInstance{}, err
	}

	// Idempotent create: a retried Create whose name already exists recovers the
	// existing server instead of launching a second one. Recovery is
	// power-state-aware (ensureRunning), because a Create that died between
	// CreateServer and Poweron leaves a STOPPED server that would otherwise be
	// polled forever; ensureRunning also (re-)applies user-data before first boot
	// so a server created before SetServerUserData ran still gets its agent config.
	if existing := r.serverByName(ctx, name); existing != nil {
		return r.ensureRunning(ctx, existing.ID, spec.MachineID, userData)
	}

	res, err := r.api.CreateServer(&instance.CreateServerRequest{
		Zone:           r.zone,
		Name:           name,
		CommercialType: spec.CommercialType,
		Image:          scw.StringPtr(spec.Image),
		Tags:           createTags(spec.MachineID),
		Project:        optStr(r.cfg.Creds.projectID),
	}, scw.WithContext(ctx))
	if err != nil {
		if existing := r.serverByName(ctx, name); existing != nil {
			return r.ensureRunning(ctx, existing.ID, spec.MachineID, userData)
		}
		return serverInstance{}, fmt.Errorf("create server %s: %w", spec.CommercialType, err)
	}
	if res == nil || res.Server == nil {
		return serverInstance{}, fmt.Errorf("create server %s: empty result", spec.CommercialType)
	}
	srv := res.Server

	// Stamp the managed + machine-id tags on the implicitly-created boot volume(s)
	// so ReapOrphanVolumes can later identify and delete a volume left orphaned by
	// an out-of-band server deletion. Best-effort (a tag failure is logged, not
	// fatal): a normal Delete still removes the volume inline; the reaper is only
	// the backstop.
	r.tagVolumes(ctx, spec.MachineID, srv.Volumes)

	// Set the cloud-init user-data (consumed at first boot only), then power on.
	if len(userData) > 0 {
		if err := r.api.SetServerUserData(&instance.SetServerUserDataRequest{
			Zone:     r.zone,
			ServerID: srv.ID,
			Key:      "cloud-init",
			Content:  strings.NewReader(userData),
		}, scw.WithContext(ctx)); err != nil {
			return serverInstance{}, fmt.Errorf("set user-data on %s: %w", srv.ID, err)
		}
	}
	if _, err := r.api.ServerAction(&instance.ServerActionRequest{
		Zone:     r.zone,
		ServerID: srv.ID,
		Action:   instance.ServerActionPoweron,
	}, scw.WithContext(ctx)); err != nil {
		return serverInstance{}, fmt.Errorf("power on %s: %w", srv.ID, err)
	}
	return r.waitRunning(ctx, srv.ID)
}

// EnsureRunning powers a stopped server back on and waits for it, used by
// Configure/Drain before delivering the bootstrap so the on-host agent is alive
// to receive it. No user-data re-apply: an Idle host has already booted (its
// agent config is in place); we only need it running again. No-op when already
// running/starting.
func (r *scwReal) EnsureRunning(ctx context.Context, serverID string) error {
	// machineID "" → no volume re-tagging here: a Configure/Drain on an already-
	// created server keeps the tags stamped at create (or repaired by the create-
	// recovery path); only that path needs to repair them.
	_, err := r.ensureRunning(ctx, serverID, "", "")
	return err
}

// ensureRunning recovers an already-created server toward running. If it is
// stopped (a Create that died before/at Poweron leaves it stopped — Scaleway
// Instances boot stopped), it (re-)applies the agent user-data and issues an
// idempotent Poweron, then waits. Re-applying user-data before first boot also
// repairs a crash between CreateServer and SetServerUserData, which would
// otherwise boot an agentless server that never joins. Without the Poweron, a
// recovery branch would poll a stopped server forever (timeout → sticky Create
// failure + leaked server/volume on every retry).
func (r *scwReal) ensureRunning(ctx context.Context, id, machineID, userData string) (serverInstance, error) {
	res, err := r.api.GetServer(&instance.GetServerRequest{Zone: r.zone, ServerID: id}, scw.WithContext(ctx))
	if err != nil {
		return serverInstance{}, fmt.Errorf("recover server %s: %w", id, err)
	}
	if res == nil || res.Server == nil {
		return serverInstance{}, fmt.Errorf("server %s vanished during recovery", id)
	}
	// On the create-recovery path (machineID set), (re-)assert the boot volume tags:
	// a Create that crashed between CreateServer and tagVolumes would otherwise
	// leave the volume permanently untagged and thus invisible to ReapOrphanVolumes.
	// Idempotent, so repeating it on a later retry is harmless.
	if machineID != "" {
		r.tagVolumes(ctx, machineID, res.Server.Volumes)
	}
	switch res.Server.State {
	case instance.ServerStateRunning, instance.ServerStateStarting:
		// already booted / booting: user-data is consumed at first boot, so it is
		// too late (and unnecessary) to re-apply it here.
	default:
		// stopped / stopped-in-place: (re-)assert user-data, then power on. The
		// server has not booted yet, so a crash between CreateServer and the
		// original SetServerUserData is repaired here — cloud-init consumes the
		// freshly-set user-data on first boot, so the agent config is present.
		if len(userData) > 0 {
			if err := r.api.SetServerUserData(&instance.SetServerUserDataRequest{
				Zone:     r.zone,
				ServerID: id,
				Key:      "cloud-init",
				Content:  strings.NewReader(userData),
			}, scw.WithContext(ctx)); err != nil {
				return serverInstance{}, fmt.Errorf("recover: set user-data on %s: %w", id, err)
			}
		}
		if _, err := r.api.ServerAction(&instance.ServerActionRequest{
			Zone: r.zone, ServerID: id, Action: instance.ServerActionPoweron,
		}, scw.WithContext(ctx)); err != nil && !is404(err) {
			return serverInstance{}, fmt.Errorf("recover: power on %s: %w", id, err)
		}
	}
	return r.waitRunning(ctx, id)
}

// waitRunning polls until the server reaches the running state (so the kit's IDLE
// means "reachable host" and the immediately-following Configure does not race a
// still-booting server). ctx (the kit's Create timeout) cancels it.
func (r *scwReal) waitRunning(ctx context.Context, id string) (serverInstance, error) {
	deadline := time.Now().Add(r.cfg.CreateWaitTimeout)
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		res, err := r.api.GetServer(&instance.GetServerRequest{Zone: r.zone, ServerID: id}, scw.WithContext(ctx))
		if err != nil {
			return serverInstance{}, fmt.Errorf("poll server %s: %w", id, err)
		}
		if res == nil || res.Server == nil {
			return serverInstance{}, fmt.Errorf("server %s vanished while creating", id)
		}
		if res.Server.State == instance.ServerStateRunning {
			return r.toServerInstance(res.Server), nil
		}
		select {
		case <-ctx.Done():
			return serverInstance{}, fmt.Errorf("waiting for server %s to run: %w", id, ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return serverInstance{}, fmt.Errorf("server %s did not reach running within %s", id, r.cfg.CreateWaitTimeout)
			}
		}
	}
}

func (r *scwReal) DeleteServer(ctx context.Context, serverID string) error {
	res, err := r.api.GetServer(&instance.GetServerRequest{Zone: r.zone, ServerID: serverID}, scw.WithContext(ctx))
	if err != nil {
		if is404(err) {
			return nil // already gone — idempotent
		}
		return fmt.Errorf("delete: lookup server %s: %w", serverID, err)
	}
	if res == nil || res.Server == nil {
		return nil
	}
	// Capture the attached volumes BEFORE deleting the server: a plain DeleteServer
	// detaches but does NOT delete them, so the implicitly-created boot volume would
	// otherwise be orphaned and keep billing as the fleet churns.
	volumes := res.Server.Volumes

	// Power off and wait until the server is actually stopped (poweroff is async;
	// the server must be stopped before DeleteServer). Poll explicitly — a bare
	// WaitForServer can treat the still-'running' state as terminal and return
	// before the poweroff takes effect.
	if res.Server.State != instance.ServerStateStopped {
		if _, err := r.api.ServerAction(&instance.ServerActionRequest{
			Zone: r.zone, ServerID: serverID, Action: instance.ServerActionPoweroff,
		}, scw.WithContext(ctx)); err != nil && !is404(err) {
			return fmt.Errorf("delete: power off %s: %w", serverID, err)
		}
		if err := r.waitStopped(ctx, serverID); err != nil {
			r.logger.Warn("delete: wait for poweroff failed; proceeding to delete", "server", serverID, "err", err)
		}
	}
	if err := r.api.DeleteServer(&instance.DeleteServerRequest{Zone: r.zone, ServerID: serverID}, scw.WithContext(ctx)); err != nil && !is404(err) {
		return fmt.Errorf("delete server %s: %w", serverID, err)
	}
	// Delete the now-detached volumes so storage does not leak. Modern images boot
	// on a Block Storage (sbs_volume) volume managed by the block API; legacy
	// l_ssd/b_ssd volumes are managed by the instance API. 404 = already gone;
	// transient errors are retried inside deleteVolumes. A volume that STILL can't
	// be deleted is non-fatal here: the server (compute) is already gone, so the
	// machine must not wedge at FAILED — and the volume is tagged, so the orphan
	// reaper retries it on the reconcile cadence. Surfacing it would leave the
	// machine stuck FAILED forever while the reaper would have cleaned it up.
	if err := r.deleteVolumes(ctx, serverID, volumes); err != nil {
		r.logger.Warn("delete: volume cleanup incomplete; orphan reaper will retry", "server", serverID, "err", err)
	}
	return nil
}

// deleteVolumes removes the server's detached volumes via the correct API for
// each volume type, retrying a few times so a transient error (e.g. a volume
// still detaching) doesn't leak the boot volume. Idempotent (404 = already gone).
// Returns an error only if a volume still can't be deleted after the retries, so
// the caller can decide whether to surface it.
func (r *scwReal) deleteVolumes(ctx context.Context, serverID string, volumes map[string]*instance.VolumeServer) error {
	const attempts = 5
	var failed []string
	for _, vol := range volumes {
		if vol == nil || vol.ID == "" {
			continue
		}
		var err error
		for i := 0; i < attempts; i++ {
			if vol.VolumeType == instance.VolumeServerVolumeTypeSbsVolume {
				err = r.block.DeleteVolume(&block.DeleteVolumeRequest{Zone: r.zone, VolumeID: vol.ID}, scw.WithContext(ctx))
			} else {
				err = r.api.DeleteVolume(&instance.DeleteVolumeRequest{Zone: r.zone, VolumeID: vol.ID}, scw.WithContext(ctx))
			}
			if err == nil || is404(err) {
				err = nil
				break
			}
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-time.After(2 * time.Second):
				continue
			}
			break
		}
		if err != nil {
			failed = append(failed, vol.ID)
			r.logger.Warn("delete: could not delete volume after retries",
				"server", serverID, "volume", vol.ID, "type", vol.VolumeType, "err", err)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("delete: %d volume(s) of server %s could not be deleted: %v", len(failed), serverID, failed)
	}
	return nil
}

// tagVolumes stamps the managed + machine-id tags on a server's attached volumes
// so ReapOrphanVolumes can later identify (and only ever delete) volumes BigFleet
// owns. Routes each volume to its plane: sbs_volume → block API, l_ssd/b_ssd →
// instance API. Best-effort — a tag failure is logged, not fatal (a normal Delete
// still removes the volume inline; the reaper is the backstop for the out-of-band
// case, and only an untagged volume escapes it).
func (r *scwReal) tagVolumes(ctx context.Context, machineID string, volumes map[string]*instance.VolumeServer) {
	for _, vol := range volumes {
		if vol == nil || vol.ID == "" {
			continue
		}
		tags := []string{tagManaged, tagMachineID + machineID}
		var err error
		if vol.VolumeType == instance.VolumeServerVolumeTypeSbsVolume {
			_, err = r.block.UpdateVolume(&block.UpdateVolumeRequest{Zone: r.zone, VolumeID: vol.ID, Tags: &tags}, scw.WithContext(ctx))
		} else {
			_, err = r.api.UpdateVolume(&instance.UpdateVolumeRequest{Zone: r.zone, VolumeID: vol.ID, Tags: &tags}, scw.WithContext(ctx))
		}
		if err != nil {
			r.logger.Warn("tag volume failed; orphan reaper may not catch it if leaked", "machine_id", machineID, "volume", vol.ID, "err", err)
		}
	}
}

// orphanVolumeGrace is how long a detached managed volume must have existed
// before ReapOrphanVolumes will delete it. It guards against deleting a volume
// that is only momentarily detached during a live CreateServer (the boot volume
// is attached as part of create, but the grace makes the reaper robust to any
// brief create-time window and to clock skew).
const orphanVolumeGrace = 10 * time.Minute

// ReapOrphanVolumes deletes BigFleet-managed volumes no longer attached to any
// server, scanning both planes: the instance API (l_ssd/b_ssd), where an orphan
// has Server==nil, and the block API (sbs_volume), where an orphan has no
// References. Only volumes carrying the managed tag and older than
// orphanVolumeGrace are eligible, so a volume mid-create is never reaped out from
// under a live CreateServer and a volume BigFleet does not own is never touched.
// Best-effort: returns the count deleted; per-volume failures are logged and
// retried on the next reconcile.
func (r *scwReal) ReapOrphanVolumes(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-orphanVolumeGrace)
	reaped := 0

	// instance-plane volumes (l_ssd/b_ssd): orphan == no attached server.
	page := int32(1)
	per := uint32(100)
	for {
		res, err := r.api.ListVolumes(&instance.ListVolumesRequest{
			Zone: r.zone, Tags: []string{tagManaged}, Page: &page, PerPage: &per, Project: optStr(r.cfg.Creds.projectID),
		}, scw.WithContext(ctx))
		if err != nil {
			return reaped, fmt.Errorf("reap: list instance volumes: %w", err)
		}
		if res == nil || len(res.Volumes) == 0 {
			break
		}
		for _, vol := range res.Volumes {
			if vol == nil || vol.Server != nil {
				continue // still attached
			}
			if vol.VolumeType == instance.VolumeVolumeTypeSbsVolume {
				// An sbs_volume is owned by the block plane (it can surface on the
				// instance plane too); the block loop below reaps it via the block API.
				// Deleting it here would be a wrong-plane delete, breaking the plane
				// routing the inline cleanup relies on.
				continue
			}
			if vol.CreationDate != nil && vol.CreationDate.After(cutoff) {
				continue // too young — may belong to an in-flight create
			}
			if err := r.api.DeleteVolume(&instance.DeleteVolumeRequest{Zone: r.zone, VolumeID: vol.ID}, scw.WithContext(ctx)); err != nil && !is404(err) {
				r.logger.Warn("reap: delete orphan instance volume failed; will retry", "volume", vol.ID, "err", err)
				continue
			}
			r.logger.Info("reaped orphan volume", "plane", "instance", "volume", vol.ID)
			reaped++
		}
		if len(res.Volumes) < int(per) {
			break
		}
		page++
	}

	// block-plane volumes (sbs_volume): orphan == no references to any resource.
	bpage := int32(1)
	bsize := uint32(100)
	for {
		res, err := r.block.ListVolumes(&block.ListVolumesRequest{
			Zone: r.zone, Tags: []string{tagManaged}, Page: &bpage, PageSize: &bsize, ProjectID: optStr(r.cfg.Creds.projectID),
		}, scw.WithContext(ctx))
		if err != nil {
			return reaped, fmt.Errorf("reap: list block volumes: %w", err)
		}
		if res == nil || len(res.Volumes) == 0 {
			break
		}
		for _, vol := range res.Volumes {
			if vol == nil || len(vol.References) > 0 {
				continue // still referenced (attached)
			}
			if vol.CreatedAt != nil && vol.CreatedAt.After(cutoff) {
				continue // too young
			}
			if err := r.block.DeleteVolume(&block.DeleteVolumeRequest{Zone: r.zone, VolumeID: vol.ID}, scw.WithContext(ctx)); err != nil && !is404(err) {
				r.logger.Warn("reap: delete orphan block volume failed; will retry", "volume", vol.ID, "err", err)
				continue
			}
			r.logger.Info("reaped orphan volume", "plane", "block", "volume", vol.ID)
			reaped++
		}
		if uint32(len(res.Volumes)) < bsize {
			break
		}
		bpage++
	}
	return reaped, nil
}

// waitStopped polls until the server reaches the stopped state (poweroff is
// async), bounded by ctx.
func (r *scwReal) waitStopped(ctx context.Context, id string) error {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		res, err := r.api.GetServer(&instance.GetServerRequest{Zone: r.zone, ServerID: id}, scw.WithContext(ctx))
		if err != nil {
			if is404(err) {
				return nil
			}
			return err
		}
		if res != nil && res.Server != nil && res.Server.State == instance.ServerStateStopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *scwReal) DescribeManaged(ctx context.Context) ([]serverInstance, error) {
	servers, err := r.listManaged(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]serverInstance, 0, len(servers))
	for _, srv := range servers {
		out = append(out, r.toServerInstance(srv))
	}
	return out, nil
}

// listManaged returns the BigFleet-managed servers (optionally filtered by name),
// enumerating ALL power states. ListServers with a nil State applies the API's
// running-only default, so a stopped server (e.g. a Create that died before/at
// Poweron, or a powered-off machine) would otherwise be invisible — which would
// let idempotency recovery miss it (→ a duplicate billed server) and let
// DescribeManaged re-seed its slot Speculative. Results are de-duplicated by
// server id across states and pages.
func (r *scwReal) listManaged(ctx context.Context, name *string) ([]*instance.Server, error) {
	byID := make(map[string]*instance.Server)
	for _, state := range instance.ServerStateRunning.Values() {
		state := state
		page := int32(1)
		per := uint32(100)
		for {
			res, err := r.api.ListServers(&instance.ListServersRequest{
				Zone:    r.zone,
				Tags:    []string{tagManaged},
				Name:    name,
				State:   &state,
				Page:    &page,
				PerPage: &per,
				Project: optStr(r.cfg.Creds.projectID),
			}, scw.WithContext(ctx))
			if err != nil {
				return nil, err
			}
			if res == nil || len(res.Servers) == 0 {
				break
			}
			for _, srv := range res.Servers {
				byID[srv.ID] = srv
			}
			if len(res.Servers) < int(per) {
				break
			}
			page++
		}
	}
	out := make([]*instance.Server, 0, len(byID))
	for _, srv := range byID {
		out = append(out, srv)
	}
	return out, nil
}

func (r *scwReal) ApplyBootstrap(ctx context.Context, srv serverInstance, clusterID string, bootstrap []byte) error {
	if srv.MachineID == "" {
		return fmt.Errorf("configure: server %s carries no machine id tag", srv.ServerID)
	}
	// Deliver the opaque blob to the running server over the agent channel and
	// wait for the agent to apply it — a failed join surfaces as FAILED. The agent
	// dials the provider (the provider needs no inbound path to the server), so no
	// server IP is required here.
	cmd := bootstrapCommand{
		Type:      "configure",
		ClusterID: clusterID,
		Blob:      base64.StdEncoding.EncodeToString(bootstrap),
	}
	if err := r.vault.Enqueue(ctx, srv.MachineID, cmd); err != nil {
		return err
	}
	// Record the binding tag only AFTER the bootstrap actually succeeded, so a
	// failed Configure never leaves a server tagged as bound to a cluster it never
	// joined.
	return r.setClusterTag(ctx, srv.ServerID, clusterID)
}

func (r *scwReal) DrainNode(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error {
	if srv.MachineID == "" {
		return fmt.Errorf("drain: server %s carries no machine id tag", srv.ServerID)
	}
	cmd := bootstrapCommand{Type: "drain", GraceSeconds: drainGrace(gracePeriodSeconds)}
	if err := r.vault.Enqueue(ctx, srv.MachineID, cmd); err != nil {
		return err
	}
	return r.clearClusterTag(ctx, srv.ServerID)
}

// serverTypes returns the zone's server-type catalogue, served from a short-TTL
// cache so a burst of per-(type,zone) lookups (pricing.refresh calls PriceUSD
// once per offering pair) collapses to a single catalogue scan per refresh run.
func (r *scwReal) serverTypes(ctx context.Context) (map[string]*instance.ServerType, error) {
	r.typesMu.Lock()
	if r.typesCache != nil && time.Now().Before(r.typesExpiry) {
		cached := r.typesCache
		r.typesMu.Unlock()
		return cached, nil
	}
	r.typesMu.Unlock()

	fresh, err := r.listAllServerTypes(ctx)
	if err != nil {
		return nil, err
	}
	r.typesMu.Lock()
	r.typesCache = fresh
	r.typesExpiry = time.Now().Add(serverTypesTTL)
	r.typesMu.Unlock()
	return fresh, nil
}

// listAllServerTypes pages through the zone's server-type catalogue and returns
// the merged map. ListServersTypes is paginated, so a single call would miss
// types beyond the first page for a large catalogue.
func (r *scwReal) listAllServerTypes(ctx context.Context) (map[string]*instance.ServerType, error) {
	out := make(map[string]*instance.ServerType)
	page := int32(1)
	per := uint32(100)
	for {
		res, err := r.api.ListServersTypes(&instance.ListServersTypesRequest{
			Zone: r.zone, Page: &page, PerPage: &per,
		}, scw.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		if res == nil || len(res.Servers) == 0 {
			break
		}
		for name, st := range res.Servers {
			out[name] = st
		}
		if uint32(len(out)) >= res.TotalCount || len(res.Servers) < int(per) {
			break
		}
		page++
	}
	return out, nil
}

func (r *scwReal) PriceUSD(ctx context.Context, commercialType, _ string) (float64, error) {
	types, err := r.serverTypes(ctx)
	if err != nil {
		return 0, err
	}
	st, ok := types[commercialType]
	if !ok || st == nil {
		return 0, fmt.Errorf("unknown commercial type %q in zone %s", commercialType, r.zone)
	}
	// HourlyPrice is in EUR.
	return float64(st.HourlyPrice) * r.cfg.EURtoUSD, nil
}

func (r *scwReal) DescribeCommercialTypeCapacities(ctx context.Context, commercialTypes []string) (map[string]commercialCapacity, error) {
	types, err := r.serverTypes(ctx)
	if err != nil {
		return nil, err
	}
	want := make(map[string]struct{}, len(commercialTypes))
	for _, t := range commercialTypes {
		want[t] = struct{}{}
	}
	out := make(map[string]commercialCapacity, len(commercialTypes))
	for name, st := range types {
		if st == nil {
			continue
		}
		if _, ok := want[name]; !ok {
			continue
		}
		gpus := 0
		if st.Gpu != nil {
			gpus = int(*st.Gpu)
		}
		out[name] = commercialCapacity{
			VCPU:   int(st.Ncpus),
			MemMiB: int64(st.RAM / (1024 * 1024)),
			GPUs:   gpus,
		}
	}
	return out, nil
}

// --- helpers --------------------------------------------------------------

func (r *scwReal) toServerInstance(srv *instance.Server) serverInstance {
	out := serverInstance{
		ServerID:       srv.ID,
		CommercialType: srv.CommercialType,
		Zone:           srv.Zone.String(),
		MachineID:      tagValue(srv.Tags, tagMachineID),
		ClusterID:      tagValue(srv.Tags, tagCluster),
		Running:        srv.State == instance.ServerStateRunning || srv.State == instance.ServerStateStarting,
	}
	return out
}

// serverByName finds a BigFleet-managed server by its derived name, used to make
// CreateServer idempotent across a retried Create. It filters on the managed tag
// (so a name collision with a server BigFleet does not own can never be "adopted"
// as an idempotent retry) and enumerates all power states via listManaged — a
// Create that died before/at Poweron leaves a STOPPED server, which the API's
// running-only default would hide, causing a duplicate billed server on retry.
func (r *scwReal) serverByName(ctx context.Context, name string) *instance.Server {
	servers, err := r.listManaged(ctx, scw.StringPtr(name))
	if err != nil {
		return nil
	}
	// If a create double-provision left more than one server under this name, pick
	// the lowest server id — the same deterministic survivor the duplicate reaper
	// keeps — so idempotent recovery and the reaper converge on the same server
	// instead of fighting over which one is canonical.
	var chosen *instance.Server
	for _, srv := range servers {
		if srv.Name != name {
			continue
		}
		if chosen == nil || srv.ID < chosen.ID {
			chosen = srv
		}
	}
	return chosen
}

func (r *scwReal) setClusterTag(ctx context.Context, serverID, clusterID string) error {
	return r.updateTags(ctx, serverID, func(tags []string) []string {
		tags = dropTag(tags, tagCluster)
		return append(tags, tagCluster+clusterID)
	})
}

func (r *scwReal) clearClusterTag(ctx context.Context, serverID string) error {
	return r.updateTags(ctx, serverID, func(tags []string) []string {
		return dropTag(tags, tagCluster)
	})
}

func (r *scwReal) updateTags(ctx context.Context, serverID string, mutate func([]string) []string) error {
	res, err := r.api.GetServer(&instance.GetServerRequest{Zone: r.zone, ServerID: serverID}, scw.WithContext(ctx))
	if err != nil {
		return err
	}
	if res == nil || res.Server == nil {
		return fmt.Errorf("server %s not found", serverID)
	}
	tags := mutate(append([]string(nil), res.Server.Tags...))
	_, err = r.api.UpdateServer(&instance.UpdateServerRequest{
		Zone:     r.zone,
		ServerID: serverID,
		Tags:     &tags,
	}, scw.WithContext(ctx))
	return err
}

// combineUserData assembles the cloud-init user-data delivered at server create:
// the operator's base user-data (if any) plus the agent cloud-config. With no
// base it returns the bare agent config; with a base it wraps both in a MIME
// multipart archive cloud-init understands, so the agent injection composes with
// whatever the operator supplied.
func combineUserData(base []byte, agentCfg string) (string, error) {
	if len(bytes.TrimSpace(base)) == 0 {
		return agentCfg, nil
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	header := fmt.Sprintf("Content-Type: multipart/mixed; boundary=%q\nMIME-Version: 1.0\n\n", mw.Boundary())
	addPart := func(ctype string, body []byte) error {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", ctype)
		h.Set("MIME-Version", "1.0")
		pw, err := mw.CreatePart(h)
		if err != nil {
			return err
		}
		_, err = pw.Write(body)
		return err
	}
	if err := addPart(baseUserDataContentType(base), base); err != nil {
		return "", fmt.Errorf("assemble user-data: %w", err)
	}
	if err := addPart("text/cloud-config", []byte(agentCfg)); err != nil {
		return "", fmt.Errorf("assemble user-data: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("assemble user-data: %w", err)
	}
	return header + buf.String(), nil
}

func baseUserDataContentType(base []byte) string {
	s := strings.TrimLeft(string(base), " \t\r\n")
	switch {
	case strings.HasPrefix(s, "#cloud-config"):
		return "text/cloud-config"
	case strings.HasPrefix(s, "#!"):
		return "text/x-shellscript"
	case strings.HasPrefix(s, "#cloud-boothook"):
		return "text/cloud-boothook"
	default:
		return "text/cloud-config"
	}
}

// createTags builds the tag set stamped on a created server: the managed marker
// plus the machine-id tag, so DescribeManaged can recover inventory.
func createTags(machineID string) []string {
	return []string{tagManaged, tagMachineID + machineID}
}

// tagValue returns the value of the first "prefix<value>" tag, or "".
func tagValue(tags []string, prefix string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, prefix) {
			return strings.TrimPrefix(t, prefix)
		}
	}
	return ""
}

// dropTag removes any tag with the given prefix.
func dropTag(tags []string, prefix string) []string {
	out := tags[:0]
	for _, t := range tags {
		if strings.HasPrefix(t, prefix) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// serverName derives a stable, DNS-safe Scaleway server name from the operation
// id (stable across a retried Create), so a transport retry recreates under the
// same name and the create is idempotent.
func serverName(spec serverSpec) string {
	token := spec.IdempotencyToken
	if token == "" {
		token = spec.MachineID
	}
	name := "bigfleet-" + sanitizeName(token)
	if len(name) > 63 {
		name = name[:63]
	}
	// A Scaleway server name must not end with '-' (truncation or an id ending in
	// a non-alphanumeric can leave a trailing dash). The "bigfleet" prefix keeps it
	// non-empty after trimming.
	name = strings.TrimRight(name, "-")
	if name == "" {
		name = "bigfleet"
	}
	return name
}

// sanitizeName lowercases and replaces any non-[a-z0-9-] byte with '-', so an
// opaque machine id / operation id becomes a valid Scaleway server name.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// is404 reports whether err is a Scaleway not-found error.
func is404(err error) bool {
	if err == nil {
		return false
	}
	// Rely only on SDK-typed errors — a substring match on "not found" could
	// misclassify a permission/validation failure as a 404 and silently swallow a
	// real error (notably on the Delete path).
	var notFound *scw.ResourceNotFoundError
	if errors.As(err, &notFound) {
		return true
	}
	var respErr *scw.ResponseError
	if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
		return true
	}
	return false
}

var _ scwClient = (*scwReal)(nil)
