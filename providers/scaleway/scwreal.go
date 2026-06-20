package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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

// complete reports whether enough credentials are present to talk to the real
// Scaleway API. Used by `--scaleway-backend=auto` to fall back to the fake when
// no credentials are configured (the credential-free certification path).
func (c scwCredentials) complete() bool {
	return c.accessKey != "" && c.secretKey != ""
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
	// AgentToken authorises the on-host agent to fetch its own bootstrap blob over
	// the mutually-authenticated TLS control channel (see scwbootstrap.go).
	AgentToken string

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
// bootstrap is published for the on-host agent to fetch over the
// mutually-authenticated TLS control channel (Scaleway user-data is consumed
// only at first boot), and Drain is driven over the same channel.
type scwReal struct {
	cfg     scwRealConfig
	zone    scw.Zone
	api     *instance.API
	deliver bootstrapDeliverer
	logger  *slog.Logger
}

// newSCWReal builds the production client for the configured substrate. The
// Elastic Metal path is reported as unsupported-in-binary here so that a misbuilt
// deployment fails loudly rather than silently provisioning Instances for a
// bare-metal offering; the Instances path is fully implemented.
func newSCWReal(cfg scwRealConfig, logger *slog.Logger) (scwClient, error) {
	if !cfg.Creds.complete() {
		return nil, fmt.Errorf("scaleway: access key + secret key are required for the scaleway backend")
	}
	if cfg.Image == "" {
		return nil, fmt.Errorf("scaleway: --image is required for the scaleway backend")
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
		cfg:     cfg,
		zone:    zone,
		api:     instance.NewAPI(client),
		deliver: newHTTPDeliverer(cfg.AgentToken, logger),
		logger:  logger,
	}, nil
}

func (r *scwReal) CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error) {
	name := serverName(spec)
	// Idempotent create: a retried Create whose name already exists recovers the
	// existing server instead of launching a second one.
	if existing := r.serverByName(ctx, name); existing != nil {
		return r.waitRunning(ctx, existing.ID)
	}

	userData, err := r.buildUserData(spec)
	if err != nil {
		return serverInstance{}, err
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
			return r.waitRunning(ctx, existing.ID)
		}
		return serverInstance{}, fmt.Errorf("create server %s: %w", spec.CommercialType, err)
	}
	if res == nil || res.Server == nil {
		return serverInstance{}, fmt.Errorf("create server %s: empty result", spec.CommercialType)
	}
	srv := res.Server

	// Set the generic base user-data (consumed at first boot only), then power on.
	if len(userData) > 0 {
		if err := r.api.SetServerUserData(&instance.SetServerUserDataRequest{
			Zone:     r.zone,
			ServerID: srv.ID,
			Key:      "cloud-init",
			Content:  strings.NewReader(string(userData)),
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
	// Power off before deletion so attached resources release cleanly. The
	// terminate action also deletes the server's block/local volumes.
	if res.Server.State == instance.ServerStateRunning {
		if _, err := r.api.ServerAction(&instance.ServerActionRequest{
			Zone: r.zone, ServerID: serverID, Action: instance.ServerActionPoweroff,
		}, scw.WithContext(ctx)); err != nil && !is404(err) {
			return fmt.Errorf("delete: power off %s: %w", serverID, err)
		}
		if _, err := r.api.WaitForServer(&instance.WaitForServerRequest{Zone: r.zone, ServerID: serverID}, scw.WithContext(ctx)); err != nil {
			r.logger.Warn("delete: wait for poweroff failed; proceeding to delete", "server", serverID, "err", err)
		}
	}
	if err := r.api.DeleteServer(&instance.DeleteServerRequest{Zone: r.zone, ServerID: serverID}, scw.WithContext(ctx)); err != nil && !is404(err) {
		return fmt.Errorf("delete server %s: %w", serverID, err)
	}
	return nil
}

func (r *scwReal) DescribeManaged(ctx context.Context) ([]serverInstance, error) {
	var out []serverInstance
	tags := []string{tagManaged}
	page := int32(1)
	per := uint32(100)
	for {
		res, err := r.api.ListServers(&instance.ListServersRequest{
			Zone:    r.zone,
			Tags:    tags,
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
			out = append(out, r.toServerInstance(srv))
		}
		if len(res.Servers) < int(per) {
			break
		}
		page++
	}
	return out, nil
}

func (r *scwReal) ApplyBootstrap(ctx context.Context, srv serverInstance, clusterID string, bootstrap []byte) error {
	srv, err := r.ensureIPv4(ctx, srv)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	// Deliver the opaque bootstrap blob over the mutually-authenticated TLS
	// channel: the on-host agent (installed by the base user-data at Create) fetches
	// its own machine-specific blob and applies it. We wait for it to SUCCEED, so a
	// failed bootstrap surfaces as FAILED.
	if err := r.deliver.Deliver(ctx, srv, clusterID, bootstrap); err != nil {
		return err
	}
	// Record the binding tag only AFTER the bootstrap actually succeeded, so a
	// failed Configure never leaves a server tagged as bound to a cluster it never
	// joined.
	return r.setClusterTag(ctx, srv.ServerID, clusterID)
}

func (r *scwReal) DrainNode(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error {
	srv, err := r.ensureIPv4(ctx, srv)
	if err != nil {
		// No reachable host: at least remove the binding tag so the machine returns
		// to an unbound state in inventory.
		return r.clearClusterTag(ctx, srv.ServerID)
	}
	if err := r.deliver.Drain(ctx, srv, gracePeriodSeconds); err != nil {
		return err
	}
	return r.clearClusterTag(ctx, srv.ServerID)
}

func (r *scwReal) PriceUSD(ctx context.Context, commercialType, _ string) (float64, error) {
	res, err := r.api.ListServersTypes(&instance.ListServersTypesRequest{Zone: r.zone}, scw.WithContext(ctx))
	if err != nil {
		return 0, err
	}
	if res == nil {
		return 0, fmt.Errorf("empty server-types response")
	}
	st, ok := res.Servers[commercialType]
	if !ok || st == nil {
		return 0, fmt.Errorf("unknown commercial type %q in zone %s", commercialType, r.zone)
	}
	// HourlyPrice is in EUR.
	return float64(st.HourlyPrice) * r.cfg.EURtoUSD, nil
}

func (r *scwReal) DescribeCommercialTypeCapacities(ctx context.Context, commercialTypes []string) (map[string]commercialCapacity, error) {
	res, err := r.api.ListServersTypes(&instance.ListServersTypesRequest{Zone: r.zone}, scw.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	if res == nil {
		return map[string]commercialCapacity{}, nil
	}
	want := make(map[string]struct{}, len(commercialTypes))
	for _, t := range commercialTypes {
		want[t] = struct{}{}
	}
	out := make(map[string]commercialCapacity, len(commercialTypes))
	for name, st := range res.Servers {
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
	out.PublicIPv4 = firstPublicIPv4(srv.PublicIPs)
	return out
}

// firstPublicIPv4 returns the first IPv4 (inet) address from a server's public
// IPs, or "". Uses PublicIPs (the deprecated singular PublicIP field is avoided).
func firstPublicIPv4(ips []*instance.ServerIP) string {
	for _, ip := range ips {
		if ip == nil || ip.Address == nil {
			continue
		}
		if ip.Family == instance.ServerIPIPFamilyInet6 {
			continue
		}
		return ip.Address.String()
	}
	return ""
}

func (r *scwReal) serverByName(ctx context.Context, name string) *instance.Server {
	res, err := r.api.ListServers(&instance.ListServersRequest{
		Zone:    r.zone,
		Name:    scw.StringPtr(name),
		Project: optStr(r.cfg.Creds.projectID),
	}, scw.WithContext(ctx))
	if err != nil || res == nil {
		return nil
	}
	for _, srv := range res.Servers {
		if srv.Name == name {
			return srv
		}
	}
	return nil
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

func (r *scwReal) ensureIPv4(ctx context.Context, srv serverInstance) (serverInstance, error) {
	if srv.PublicIPv4 != "" {
		return srv, nil
	}
	res, err := r.api.GetServer(&instance.GetServerRequest{Zone: r.zone, ServerID: srv.ServerID}, scw.WithContext(ctx))
	if err != nil {
		return srv, fmt.Errorf("look up server %s: %w", srv.ServerID, err)
	}
	if res == nil || res.Server == nil {
		return srv, fmt.Errorf("server %s not found", srv.ServerID)
	}
	full := r.toServerInstance(res.Server)
	if full.PublicIPv4 == "" {
		return srv, fmt.Errorf("server %s has no public IPv4 for bootstrap delivery", srv.ServerID)
	}
	return full, nil
}

func (r *scwReal) buildUserData(spec serverSpec) ([]byte, error) {
	if len(spec.BaseUserData) == 0 {
		return nil, nil
	}
	return spec.BaseUserData, nil
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
	var notFound *scw.ResourceNotFoundError
	if errors.As(err, &notFound) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}

var _ scwClient = (*scwReal)(nil)
