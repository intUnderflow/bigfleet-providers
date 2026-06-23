package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	baremetal "github.com/scaleway/scaleway-sdk-go/api/baremetal/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

// scwBaremetal is the production Elastic Metal (BARE_METAL) scwClient, backed by
// scaleway-sdk-go's baremetal/v1 API. It is the Elastic Metal analogue of
// [scwReal]: it provisions real physical servers and recovers inventory and
// bindings from server tags, but provisioning is the baremetal two-step (order +
// install + wait-for-install) rather than the Instances create + poweron, and
// there is no detachable boot volume to reap.
//
// It shares the Instances path's bootstrap-delivery model verbatim: the
// cluster-specific bootstrap and the drain are delivered over the on-host agent's
// mutually-authenticated TLS channel (the agent is installed by the generic
// install-time cloud-init user-data), so ApplyBootstrap/DrainNode are identical to
// [scwReal] save the tag write, which goes through the baremetal API.
//
// The kit never calls DeleteServer or ReapOrphanVolumes for a BARE_METAL machine
// (the backend omits providerkit.Deleter, so Delete is codes.Unimplemented, and
// the orphan/duplicate reapers are ON_DEMAND-gated). DeleteServer is implemented
// faithfully anyway (idempotent) to satisfy the interface; ReapOrphanVolumes is a
// no-op (owned hardware has no detachable cloud volumes).
type scwBaremetal struct {
	cfg    scwRealConfig
	zone   scw.Zone
	api    *baremetal.API
	vault  *bootstrapVault
	logger *slog.Logger

	// offersCache memoises the (single-zone) hourly offer catalogue for a short
	// TTL: CreateServer resolves a commercial-type name to an offer id, and
	// DescribeCommercialTypeCapacities resolves capacities, both off the same
	// catalogue, so a burst of lookups collapses to one ListOffers scan.
	offersMu     sync.Mutex
	offersCache  map[string]*baremetal.Offer // keyed by offer Name (e.g. EM-A210R-HDD)
	offersExpiry time.Time

	// osCache memoises the OS catalogue for the same reason (Image label → OsID).
	osMu     sync.Mutex
	osCache  []*baremetal.OS
	osExpiry time.Time
}

// Elastic Metal commissioning + OS install runs far longer than an Instances
// boot — tens of minutes is normal — so the per-poll waits are generous and the
// real bound is the kit's Create timeout (2h for BARE_METAL), carried on ctx.
const (
	emProvisionTimeout = 2 * time.Hour
	emPollInterval     = 15 * time.Second
	emCatalogueTTL     = time.Minute
)

// newSCWElasticMetal builds the production Elastic Metal client. It is called from
// newSCWReal once the shared Scaleway client, zone, and credential/bootstrap
// validation have already succeeded, so it only needs to wire the baremetal API.
func newSCWElasticMetal(cfg scwRealConfig, client *scw.Client, zone scw.Zone, logger *slog.Logger) (scwClient, error) {
	return &scwBaremetal{
		cfg:    cfg,
		zone:   zone,
		api:    baremetal.NewAPI(client),
		vault:  cfg.Vault,
		logger: logger,
	}, nil
}

func (r *scwBaremetal) CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error) {
	// The generic, pre-binding agent bootstrap delivered as install-time cloud-init
	// user-data: the operator's base user-data plus the agent config carrying this
	// machine's token and the provider's pinned endpoint/CA. Identical to the
	// Instances path — the cluster-specific blob arrives later over the agent
	// channel (cloud-init user-data is consumed only at first boot after install).
	token := r.vault.Token(spec.MachineID)
	agentCfg := agentCloudConfig(r.cfg.BootstrapEndpoint, r.cfg.BootstrapCAPEM, spec.MachineID, token)
	userData, err := combineUserData(spec.BaseUserData, agentCfg)
	if err != nil {
		return serverInstance{}, err
	}
	name := serverName(spec)

	// Idempotent create: a retried Create whose name already exists recovers the
	// existing server (finishing install / powering it on) instead of ordering a
	// second physical machine. Recovery is install- and power-state-aware so a
	// Create that died mid-provision converges rather than leaking hardware.
	if existing := r.serverByName(ctx, name); existing != nil {
		return r.provision(ctx, existing, spec, userData)
	}

	offer, err := r.offerByName(ctx, spec.CommercialType)
	if err != nil {
		return serverInstance{}, fmt.Errorf("create %s: %w", spec.CommercialType, err)
	}

	udBytes := []byte(userData)
	res, err := r.api.CreateServer(&baremetal.CreateServerRequest{
		Zone:      r.zone,
		OfferID:   offer.ID,
		Name:      name,
		Tags:      createTags(spec.MachineID),
		ProjectID: optStr(r.cfg.Creds.projectID),
		UserData:  &udBytes,
	}, scw.WithContext(ctx))
	if err != nil {
		// A retried Create that raced another and lost recovers the winner rather
		// than surfacing the conflict (and never orders a duplicate machine).
		if existing := r.serverByName(ctx, name); existing != nil {
			return r.provision(ctx, existing, spec, userData)
		}
		return serverInstance{}, fmt.Errorf("create server %s: %w", spec.CommercialType, err)
	}
	if res == nil {
		return serverInstance{}, fmt.Errorf("create server %s: empty result", spec.CommercialType)
	}
	return r.provision(ctx, res, spec, userData)
}

// provision drives a freshly-created OR recovered server to Idle: wait for
// delivery, ensure the install-time user-data is set, install the OS if needed,
// wait for the install to complete, then ensure the server is powered on. It is
// idempotent — a retried Create that finds an existing server re-enters here, so
// a Create that died mid-provision converges rather than leaking hardware.
func (r *scwBaremetal) provision(ctx context.Context, srv *baremetal.Server, spec serverSpec, userData string) (serverInstance, error) {
	// Wait out the transient delivery/ordering states. WaitForServer returns on the
	// first terminal status (ready/stopped/error/out_of_stock/locked), so the result
	// is inspected explicitly below.
	srv, err := r.api.WaitForServer(&baremetal.WaitForServerRequest{
		Zone:          r.zone,
		ServerID:      srv.ID,
		Timeout:       scw.TimeDurationPtr(emProvisionTimeout),
		RetryInterval: scw.TimeDurationPtr(emPollInterval),
	}, scw.WithContext(ctx))
	if err != nil {
		return serverInstance{}, fmt.Errorf("wait for delivery of %s: %w", spec.CommercialType, err)
	}
	switch srv.Status {
	case baremetal.ServerStatusError, baremetal.ServerStatusOutOfStock, baremetal.ServerStatusLocked, baremetal.ServerStatusDeleting:
		return serverInstance{}, fmt.Errorf("server %s reached non-recoverable status %q", srv.ID, srv.Status)
	}

	// Install the OS (with cloud-init user-data already on the server) unless it is
	// already installed — re-installing a healthy server would wipe it. A retried
	// Create whose install is already in flight only waits, never re-triggers.
	if srv.Install == nil || srv.Install.Status != baremetal.ServerInstallStatusCompleted {
		if srv.Install == nil || srv.Install.Status != baremetal.ServerInstallStatusInstalling {
			// (Re-)assert the user-data before install so a crash between CreateServer
			// and here cannot leave the host agentless: cloud-init consumes it on first
			// boot after the install.
			if err := r.setUserData(ctx, srv.ID, userData); err != nil {
				return serverInstance{}, err
			}
			osID, err := r.resolveOS(ctx, spec.Image)
			if err != nil {
				return serverInstance{}, err
			}
			if _, err := r.api.InstallServer(&baremetal.InstallServerRequest{
				Zone:      r.zone,
				ServerID:  srv.ID,
				OsID:      osID,
				Hostname:  name63(spec.MachineID),
				SSHKeyIDs: r.cfg.BareMetalSSHKeyIDs,
			}, scw.WithContext(ctx)); err != nil {
				return serverInstance{}, fmt.Errorf("install server %s: %w", srv.ID, err)
			}
		}
		srv, err = r.api.WaitForServerInstall(&baremetal.WaitForServerInstallRequest{
			Zone:          r.zone,
			ServerID:      srv.ID,
			Timeout:       scw.TimeDurationPtr(emProvisionTimeout),
			RetryInterval: scw.TimeDurationPtr(emPollInterval),
		}, scw.WithContext(ctx))
		if err != nil {
			return serverInstance{}, fmt.Errorf("wait for install of %s: %w", srv.ID, err)
		}
		if srv.Install == nil || srv.Install.Status != baremetal.ServerInstallStatusCompleted {
			st := baremetal.ServerInstallStatusUnknown
			if srv.Install != nil {
				st = srv.Install.Status
			}
			return serverInstance{}, fmt.Errorf("install of %s did not complete (status %q)", srv.ID, st)
		}
	}

	// A freshly-installed server boots automatically; a recovered one may be
	// stopped. Guarantee it is running so the immediately-following Configure does
	// not bind a powered-off host whose agent can never poll.
	if err := r.powerOn(ctx, srv.ID); err != nil {
		return serverInstance{}, err
	}
	return r.get(ctx, srv.ID)
}

// EnsureRunning powers a stopped server back on and waits for it, used by
// Configure/Drain before delivering the bootstrap. No-op when already up.
func (r *scwBaremetal) EnsureRunning(ctx context.Context, serverID string) error {
	return r.powerOn(ctx, serverID)
}

// powerOn starts the server if it is stopped and waits until it is up. Idempotent;
// a no-op when the server is already ready/starting.
func (r *scwBaremetal) powerOn(ctx context.Context, serverID string) error {
	res, err := r.api.GetServer(&baremetal.GetServerRequest{Zone: r.zone, ServerID: serverID}, scw.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("power on: get server %s: %w", serverID, err)
	}
	if isUp(res.Status) {
		return nil
	}
	if res.Status == baremetal.ServerStatusStopped || res.Status == baremetal.ServerStatusStopping {
		if _, err := r.api.StartServer(&baremetal.StartServerRequest{Zone: r.zone, ServerID: serverID}, scw.WithContext(ctx)); err != nil && !is404(err) {
			return fmt.Errorf("power on %s: %w", serverID, err)
		}
	}
	if _, err := r.api.WaitForServer(&baremetal.WaitForServerRequest{
		Zone:          r.zone,
		ServerID:      serverID,
		Timeout:       scw.TimeDurationPtr(emProvisionTimeout),
		RetryInterval: scw.TimeDurationPtr(emPollInterval),
	}, scw.WithContext(ctx)); err != nil {
		return fmt.Errorf("wait for %s to start: %w", serverID, err)
	}
	return nil
}

func (r *scwBaremetal) setUserData(ctx context.Context, serverID, userData string) error {
	if len(userData) == 0 {
		return nil
	}
	ud := []byte(userData)
	if _, err := r.api.UpdateServer(&baremetal.UpdateServerRequest{
		Zone:     r.zone,
		ServerID: serverID,
		UserData: &ud,
	}, scw.WithContext(ctx)); err != nil {
		return fmt.Errorf("set user-data on %s: %w", serverID, err)
	}
	return nil
}

// DeleteServer is never invoked by the kit for a BARE_METAL machine (the backend
// omits providerkit.Deleter). It is implemented faithfully — idempotent — only to
// satisfy the scwClient interface.
func (r *scwBaremetal) DeleteServer(ctx context.Context, serverID string) error {
	if _, err := r.api.DeleteServer(&baremetal.DeleteServerRequest{Zone: r.zone, ServerID: serverID}, scw.WithContext(ctx)); err != nil && !is404(err) {
		return fmt.Errorf("delete server %s: %w", serverID, err)
	}
	return nil
}

// ReapOrphanVolumes is a no-op for Elastic Metal: owned physical servers have no
// detachable cloud boot volumes that could be orphaned by an out-of-band delete.
func (r *scwBaremetal) ReapOrphanVolumes(context.Context) (int, error) {
	return 0, nil
}

func (r *scwBaremetal) DescribeManaged(ctx context.Context) ([]serverInstance, error) {
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

// listManaged returns the BigFleet-managed Elastic Metal servers (optionally
// filtered by name), across all power states. scw.WithAllPages aggregates the
// paginated catalogue.
func (r *scwBaremetal) listManaged(ctx context.Context, name *string) ([]*baremetal.Server, error) {
	res, err := r.api.ListServers(&baremetal.ListServersRequest{
		Zone:      r.zone,
		Tags:      []string{tagManaged},
		Name:      name,
		ProjectID: optStr(r.cfg.Creds.projectID),
	}, scw.WithContext(ctx), scw.WithAllPages())
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	return res.Servers, nil
}

// serverByName finds a BigFleet-managed server by its derived name, making Create
// idempotent across a retried Create. Filters on the managed tag (a name collision
// with a server BigFleet does not own is never adopted) and, on a double-order,
// picks the lowest server id so recovery is deterministic.
func (r *scwBaremetal) serverByName(ctx context.Context, name string) *baremetal.Server {
	servers, err := r.listManaged(ctx, scw.StringPtr(name))
	if err != nil {
		return nil
	}
	var chosen *baremetal.Server
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

func (r *scwBaremetal) ApplyBootstrap(ctx context.Context, srv serverInstance, clusterID string, bootstrap []byte) error {
	if srv.MachineID == "" {
		return fmt.Errorf("configure: server %s carries no machine id tag", srv.ServerID)
	}
	cmd := bootstrapCommand{
		Type:      "configure",
		ClusterID: clusterID,
		Blob:      base64.StdEncoding.EncodeToString(bootstrap),
	}
	if err := r.vault.Enqueue(ctx, srv.MachineID, cmd); err != nil {
		return err
	}
	// Record the binding tag only after the bootstrap actually succeeded, so a
	// failed Configure never leaves a server tagged as bound to a cluster it never
	// joined.
	return r.setClusterTag(ctx, srv.ServerID, clusterID)
}

func (r *scwBaremetal) DrainNode(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error {
	if srv.MachineID == "" {
		return fmt.Errorf("drain: server %s carries no machine id tag", srv.ServerID)
	}
	cmd := bootstrapCommand{Type: "drain", GraceSeconds: drainGrace(gracePeriodSeconds)}
	if err := r.vault.Enqueue(ctx, srv.MachineID, cmd); err != nil {
		return err
	}
	return r.clearClusterTag(ctx, srv.ServerID)
}

// PriceUSD returns 0: Elastic Metal capacity is owned hardware, already paid for.
// The pricing hot path short-circuits BARE_METAL to 0 before ever calling this, so
// returning 0 here keeps the background refresher from needlessly scanning the
// catalogue for a price that is never consulted.
func (r *scwBaremetal) PriceUSD(context.Context, string, string) (float64, error) {
	return 0, nil
}

func (r *scwBaremetal) DescribeCommercialTypeCapacities(ctx context.Context, commercialTypes []string) (map[string]commercialCapacity, error) {
	offers, err := r.offerCatalogue(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]commercialCapacity, len(commercialTypes))
	for _, t := range commercialTypes {
		if o, ok := offers[t]; ok && o != nil {
			out[t] = offerCapacity(o)
		}
	}
	return out, nil
}

// --- helpers --------------------------------------------------------------

// offerByName resolves a commercial-type name (e.g. EM-A210R-HDD) to its hourly
// offer, from the short-TTL catalogue cache.
func (r *scwBaremetal) offerByName(ctx context.Context, commercialType string) (*baremetal.Offer, error) {
	offers, err := r.offerCatalogue(ctx)
	if err != nil {
		return nil, err
	}
	o, ok := offers[commercialType]
	if !ok || o == nil {
		return nil, fmt.Errorf("commercial type %q is not an available hourly Elastic Metal offer in zone %s", commercialType, r.zone)
	}
	return o, nil
}

// offerCatalogue returns the zone's hourly offer catalogue keyed by name, served
// from a short-TTL cache so create-time resolution and capacity resolution share
// a single ListOffers scan.
func (r *scwBaremetal) offerCatalogue(ctx context.Context) (map[string]*baremetal.Offer, error) {
	r.offersMu.Lock()
	if r.offersCache != nil && time.Now().Before(r.offersExpiry) {
		cached := r.offersCache
		r.offersMu.Unlock()
		return cached, nil
	}
	r.offersMu.Unlock()

	res, err := r.api.ListOffers(&baremetal.ListOffersRequest{
		Zone:               r.zone,
		SubscriptionPeriod: baremetal.OfferSubscriptionPeriodHourly,
	}, scw.WithContext(ctx), scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("list elastic metal offers: %w", err)
	}
	fresh := make(map[string]*baremetal.Offer, len(res.Offers))
	for _, o := range res.Offers {
		if o != nil {
			fresh[o.Name] = o
		}
	}
	r.offersMu.Lock()
	r.offersCache = fresh
	r.offersExpiry = time.Now().Add(emCatalogueTTL)
	r.offersMu.Unlock()
	return fresh, nil
}

// resolveOS maps the operator's --image (an OS id, or a name optionally suffixed
// with a version) to a baremetal OS id. An exact id match wins; otherwise the name
// (and "name version") is matched case-insensitively, preferring a cloud-init
// capable image so the agent's user-data is honoured.
func (r *scwBaremetal) resolveOS(ctx context.Context, image string) (string, error) {
	all, err := r.osCatalogue(ctx)
	if err != nil {
		return "", err
	}
	want := strings.ToLower(strings.TrimSpace(image))
	var nameMatch *baremetal.OS
	for _, os := range all {
		if os == nil {
			continue
		}
		if os.ID == image {
			return os.ID, nil
		}
		full := strings.ToLower(strings.TrimSpace(os.Name + " " + os.Version))
		if want != "" && (strings.ToLower(os.Name) == want || full == want) {
			if os.CloudInitSupported || nameMatch == nil {
				m := os
				nameMatch = m
			}
		}
	}
	if nameMatch != nil {
		return nameMatch.ID, nil
	}
	return "", fmt.Errorf("--image %q does not match an available Elastic Metal OS id or name in zone %s", image, r.zone)
}

func (r *scwBaremetal) osCatalogue(ctx context.Context) ([]*baremetal.OS, error) {
	r.osMu.Lock()
	if r.osCache != nil && time.Now().Before(r.osExpiry) {
		cached := r.osCache
		r.osMu.Unlock()
		return cached, nil
	}
	r.osMu.Unlock()

	res, err := r.api.ListOS(&baremetal.ListOSRequest{Zone: r.zone}, scw.WithContext(ctx), scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("list elastic metal OS catalogue: %w", err)
	}
	r.osMu.Lock()
	r.osCache = res.Os
	r.osExpiry = time.Now().Add(emCatalogueTTL)
	r.osMu.Unlock()
	return res.Os, nil
}

func (r *scwBaremetal) get(ctx context.Context, serverID string) (serverInstance, error) {
	res, err := r.api.GetServer(&baremetal.GetServerRequest{Zone: r.zone, ServerID: serverID}, scw.WithContext(ctx))
	if err != nil {
		return serverInstance{}, fmt.Errorf("get server %s: %w", serverID, err)
	}
	return r.toServerInstance(res), nil
}

func (r *scwBaremetal) toServerInstance(srv *baremetal.Server) serverInstance {
	return serverInstance{
		ServerID:       srv.ID,
		CommercialType: srv.OfferName,
		Zone:           srv.Zone.String(),
		MachineID:      tagValue(srv.Tags, tagMachineID),
		ClusterID:      tagValue(srv.Tags, tagCluster),
		Running:        isUp(srv.Status),
	}
}

func (r *scwBaremetal) setClusterTag(ctx context.Context, serverID, clusterID string) error {
	return r.updateTags(ctx, serverID, func(tags []string) []string {
		tags = dropTag(tags, tagCluster)
		return append(tags, tagCluster+clusterID)
	})
}

func (r *scwBaremetal) clearClusterTag(ctx context.Context, serverID string) error {
	return r.updateTags(ctx, serverID, func(tags []string) []string {
		return dropTag(tags, tagCluster)
	})
}

func (r *scwBaremetal) updateTags(ctx context.Context, serverID string, mutate func([]string) []string) error {
	res, err := r.api.GetServer(&baremetal.GetServerRequest{Zone: r.zone, ServerID: serverID}, scw.WithContext(ctx))
	if err != nil {
		return err
	}
	tags := mutate(append([]string(nil), res.Tags...))
	_, err = r.api.UpdateServer(&baremetal.UpdateServerRequest{
		Zone:     r.zone,
		ServerID: serverID,
		Tags:     &tags,
	}, scw.WithContext(ctx))
	return err
}

// isUp reports whether a baremetal server is in a live (reachable) power state.
func isUp(s baremetal.ServerStatus) bool {
	return s == baremetal.ServerStatusReady || s == baremetal.ServerStatusStarting
}

// offerCapacity sums an offer's hardware (a baremetal server can carry several
// CPUs/DIMMs/GPUs) into the allocatable shape.
func offerCapacity(o *baremetal.Offer) commercialCapacity {
	cores := 0
	for _, c := range o.CPUs {
		if c != nil {
			cores += int(c.CoreCount)
		}
	}
	var memBytes uint64
	for _, m := range o.Memories {
		if m != nil {
			memBytes += uint64(m.Capacity)
		}
	}
	return commercialCapacity{
		VCPU:   cores,
		MemMiB: int64(memBytes / (1024 * 1024)),
		GPUs:   len(o.Gpus),
	}
}

// name63 derives a stable, DNS-safe hostname (≤63 chars, no trailing dash) from an
// opaque id, reusing the server-name sanitiser.
func name63(id string) string {
	return serverName(serverSpec{MachineID: id})
}

var _ scwClient = (*scwBaremetal)(nil)
