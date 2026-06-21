package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// ovhBackend is the OVH Public Cloud (OpenStack) implementation of
// providerkit.Backend (+ Deleter). It is pure substrate logic: it maps the kit's
// lifecycle calls onto OpenStack API calls and populates the substrate fields it
// knows (instance_type, zone, capacity_type, price_per_hour,
// interruption_probability, resources, allocatable, host). Fencing, idempotency,
// async dispatch, transition timeouts, shard_metadata, and the rest are
// providerkit's job — this file never touches them.
//
// Configure-bootstrap reconciliation: OpenStack user_data is consumed by
// cloud-init only at FIRST boot, so it cannot re-bootstrap a running instance.
// CreateServer therefore launches the instance with the generic pre-binding
// --base-user-data (the first boot), and the cluster-specific bootstrap blob is
// delivered later by ConfigureInstance over SSH (the real client's
// ApplyBootstrap). This keeps the kit's invariant that an Idle machine already
// carries a real, reachable host, and delivers the secret-bearing blob exactly
// once when the binding is established.
type ovhBackend struct {
	providerName string // HostRef.provider label, e.g. "ovh-public-GRA"
	region       string
	client       ovhClient
	image        string // base image for CreateServer
	offerings    []offering
	pricing      *pricing
	flavors      *flavorResolver // resolves Machine.allocatable
	baseUserData []byte          // generic pre-binding bootstrap baked in at Create
	logger       *slog.Logger
}

func newOVHBackend(providerName, region, image string, client ovhClient, offerings []offering, pr *pricing, baseUserData []byte, logger *slog.Logger) (*ovhBackend, error) {
	if len(offerings) == 0 {
		return nil, fmt.Errorf("ovh backend: no offerings configured")
	}
	for _, off := range offerings {
		if _, err := off.capacityType(); err != nil {
			return nil, fmt.Errorf("ovh backend: offering %s/%s: %w", off.Flavor, off.Region, err)
		}
		if off.Flavor == "" {
			return nil, fmt.Errorf("ovh backend: offering with empty flavor")
		}
		// The provider registers multi-region (RequireZone), so a regionless
		// offering would only fail later at seed time — reject it up front.
		if off.Region == "" {
			return nil, fmt.Errorf("ovh backend: offering %s with empty region", off.Flavor)
		}
		// The real OpenStack client is scoped to ONE region (--region selects the
		// endpoint), so an offering for a different region would create instances
		// in the configured region while advertising a different Machine.zone —
		// breaking zone semantics. Reject the mismatch (one process per region).
		// When region is empty (the fake backend / credential-free conformance) the
		// client is not region-bound, so the multi-region default offerings are
		// allowed.
		if region != "" && off.Region != region {
			return nil, fmt.Errorf("ovh backend: offering %s is in region %q but the provider is configured for region %q (one process per region; every offering must match --region)", off.Flavor, off.Region, region)
		}
		// Price comes from a pinned EUR table (OVH has no price API). A flavor
		// absent from the table (and with no override) would publish
		// price_per_hour=0 — i.e. effectively free — and always win the shard's
		// cost ranking. Warn loudly so the operator adds a table entry or an
		// override; not fatal, since price is a relative ranking signal and the
		// override path exists.
		if pr != nil && logger != nil && !pr.known(off.Flavor) {
			logger.Warn("offering flavor has no pinned price and no override; price_per_hour will be 0 (add it to the pinned table or set a price override)",
				"flavor", off.Flavor, "region", off.Region)
		}
	}
	return &ovhBackend{
		providerName: providerName,
		region:       region,
		client:       client,
		image:        image,
		offerings:    offerings,
		pricing:      pr,
		flavors:      newFlavorResolver(client, logger),
		baseUserData: baseUserData,
		logger:       logger,
	}, nil
}

// slotID is the stable BigFleet machine id for one offering slot. A Speculative
// slot keeps this id across its whole lifecycle (Created servers are tagged with
// it in OpenStack metadata, so DescribeManaged maps back to it).
func slotID(providerName string, capacity providerkit.CapacityType, off offering, i int) string {
	return fmt.Sprintf("%s/%s/%s/%s/%03d", providerName, capacity, off.Flavor, off.Region, i)
}

// speculativeSlots renders the configured offerings as Speculative Instance
// records with full, validated field shape. Reads only in-memory pricing state,
// so it never blocks on the network.
func (b *ovhBackend) speculativeSlots() []providerkit.Instance {
	var out []providerkit.Instance
	for _, off := range b.offerings {
		capacity, _ := off.capacityType() // validated in newOVHBackend
		for i := 0; i < off.Count; i++ {
			id := slotID(b.providerName, capacity, off, i)
			out = append(out, providerkit.Instance{
				ID:           id,
				State:        providerkit.StateSpeculative,
				InstanceType: off.Flavor,
				Zone:         off.Region,
				CapacityType: capacity,
				PricePerHour: b.pricing.price(off.Flavor),
				// OVH Public Cloud is on-demand only: no spot market, so the
				// genuine, provider-declared interruption probability is exactly 0.
				InterruptionProbability: 0,
				Resources:               cloneMap(off.Resources),
				Allocatable:             b.flavors.allocatable(off.Flavor),
				Labels:                  slotLabels(off),
			})
		}
	}
	return out
}

func slotLabels(off offering) map[string]string {
	labels := cloneMap(off.Labels)
	if acc, ok := gpuLabel(off.Flavor); ok {
		if labels == nil {
			labels = map[string]string{}
		}
		labels["bigfleet.io/accelerator"] = acc
	}
	return labels
}

// Describe returns the substrate inventory: every offering slot as Speculative,
// upgraded to Idle (with its host) when a managed server already backs it, plus
// any orphan managed servers. The kit calls this to seed a fresh store; the
// persisted store is the primary restart path.
//
// A machine-id-tagged, RUNNING managed server owns its slot, keeping the slot
// from being re-seeded Speculative so Create can't launch a duplicate under the
// same machine id. A deleting server is releasing its slot and is correctly
// absent (the slot returns to Speculative for re-provisioning).
//
// A NON-running tagged server (SHUTOFF/ERROR — a powered-off image-backed
// instance can't be made Idle/reachable, and there is no power-on path) does NOT
// back its slot: backing it would publish a powered-off host as a reachable Idle
// node (Configure/Drain would then fail). It is instead surfaced as an orphan for
// cleanup (the shard's idle-release can Delete it), and its slot returns to
// Speculative for a fresh Create — consistent with serverByMachineID, the Create
// guard, which likewise only recovers running servers. Untagged-but-running
// managed servers are likewise surfaced as orphans so they are not lost.
func (b *ovhBackend) Describe(ctx context.Context) ([]providerkit.Instance, error) {
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe managed servers: %w", err)
	}
	bySlot := make(map[string]serverInstance, len(managed))
	var orphans []serverInstance
	for _, srv := range managed {
		switch {
		case srv.MachineID != "" && srv.Running:
			if _, dup := bySlot[srv.MachineID]; dup {
				// Two live servers carry the same machine id (e.g. a botched
				// create that left a duplicate). Keep the first as the slot
				// backing and surface the extra as an orphan under its server UUID
				// — never silently drop it, or a paid instance becomes invisible
				// to inventory and cleanup.
				orphans = append(orphans, srv)
				continue
			}
			bySlot[srv.MachineID] = srv // owns its slot
		case srv.Running:
			orphans = append(orphans, srv) // managed + running, but untagged
		default:
			// Non-running (SHUTOFF/ERROR), tagged or not — a dead remnant. Surface
			// it as an orphan for cleanup; it does NOT back a slot (a powered-off
			// server is not a reachable Idle host), so its slot returns to
			// Speculative for a fresh Create.
			orphans = append(orphans, srv)
		}
	}

	slots := b.speculativeSlots()
	out := make([]providerkit.Instance, 0, len(slots)+len(bySlot)+len(orphans))
	for _, slot := range slots {
		if srv, ok := bySlot[slot.ID]; ok {
			slot.State = providerkit.StateIdle
			slot.Host = providerkit.HostRef{Provider: b.providerName, Ref: srv.ServerID}
			delete(bySlot, slot.ID)
		}
		out = append(out, slot)
	}
	// Tagged servers matching no current offering slot (offering shrank, or a
	// manually tagged server), then untagged-but-running managed servers.
	for id, srv := range bySlot {
		out = append(out, b.serverToIdle(id, srv))
	}
	for _, srv := range orphans {
		out = append(out, b.serverToIdle(srv.ServerID, srv))
	}
	return out, nil
}

func (b *ovhBackend) serverToIdle(machineID string, srv serverInstance) providerkit.Instance {
	region := srv.Region
	if region == "" {
		region = b.region
	}
	return providerkit.Instance{
		ID:                      machineID,
		State:                   providerkit.StateIdle,
		Host:                    providerkit.HostRef{Provider: b.providerName, Ref: srv.ServerID},
		InstanceType:            srv.Flavor,
		Zone:                    region,
		CapacityType:            providerkit.CapacityOnDemand,
		PricePerHour:            b.pricing.price(srv.Flavor),
		InterruptionProbability: 0,
		// Recover the per-replica request shape from a still-configured offering
		// for this flavor, so an orphan / offering-shrank machine that re-binds
		// via Describe still matches its demand profile. Nil only for a truly
		// unknown flavor, where the FileStore (the primary restart path) is what
		// restores resources.
		Resources:   b.resourcesForFlavor(srv.Flavor, region),
		Allocatable: b.flavors.allocatable(srv.Flavor),
	}
}

// resourcesForFlavor returns the per-replica resources of an offering matching
// the given flavor, preferring an exact (flavor, region) match and falling back
// to the same flavor in any region. Nil when no offering covers the flavor.
func (b *ovhBackend) resourcesForFlavor(flavor, region string) map[string]string {
	var fallback map[string]string
	for _, off := range b.offerings {
		if off.Flavor != flavor {
			continue
		}
		if off.Region == region {
			return cloneMap(off.Resources)
		}
		if fallback == nil {
			fallback = off.Resources
		}
	}
	return cloneMap(fallback)
}

// CreateInstance launches the OpenStack server for a Speculative slot and
// returns its host. The cluster-specific bootstrap is delivered later by
// ConfigureInstance, because OpenStack user_data only runs at first boot.
func (b *ovhBackend) CreateInstance(ctx context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	m := req.Machine
	srv, err := b.client.CreateServer(ctx, serverSpec{
		MachineID:        m.ID,
		Flavor:           m.InstanceType,
		Region:           m.Zone,
		Image:            b.image,
		IdempotencyToken: req.OperationID,
		BaseUserData:     b.baseUserData,
	})
	if err != nil {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("create server %s: %w", m.InstanceType, err)
	}
	// The kit's empty-host guard only fires when BOTH HostRef fields are empty,
	// and Provider is always set here — so guard the server UUID explicitly. A
	// host with an empty Ref would settle the machine Idle, then break every
	// later Configure/Drain/Delete.
	if srv.ServerID == "" {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("create server %s returned no server id", m.InstanceType)
	}
	return providerkit.CreateInstanceResult{
		Host:        providerkit.HostRef{Provider: b.providerName, Ref: srv.ServerID},
		Allocatable: b.flavors.allocatable(m.InstanceType),
	}, nil
}

// ConfigureInstance binds the running server to a cluster and delivers the
// opaque bootstrap blob (real impl: SSH).
func (b *ovhBackend) ConfigureInstance(ctx context.Context, req providerkit.ConfigureInstanceRequest) error {
	srv, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	return b.client.ApplyBootstrap(ctx, srv, req.ClusterID, req.BootstrapBlob)
}

// DrainInstance cordons + drains the kubelet and removes the cluster binding,
// leaving the server running but unbound (Idle).
func (b *ovhBackend) DrainInstance(ctx context.Context, req providerkit.DrainInstanceRequest) error {
	srv, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	return b.client.DrainNode(ctx, srv, req.GracePeriodSeconds)
}

// DeleteInstance deletes the OpenStack server; the slot returns to Speculative.
func (b *ovhBackend) DeleteInstance(ctx context.Context, req providerkit.DeleteInstanceRequest) error {
	if req.Machine.Host.Ref == "" {
		return fmt.Errorf("delete: machine %s has no host", req.Machine.ID)
	}
	return b.client.DeleteServer(ctx, req.Machine.Host.Ref)
}

// resolveHost recovers the substrate server view (including the public IP needed
// for SSH-based Configure/Drain) for a machine the kit holds, by its server UUID.
func (b *ovhBackend) resolveHost(ctx context.Context, m providerkit.Machine) (serverInstance, error) {
	if m.Host.Ref == "" {
		return serverInstance{}, fmt.Errorf("machine %s has no host", m.ID)
	}
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return serverInstance{}, fmt.Errorf("describe managed servers: %w", err)
	}
	for _, srv := range managed {
		if srv.ServerID == m.Host.Ref {
			return srv, nil
		}
	}
	// Fall back to a minimal view; the real client can still address the server
	// by UUID even if a transient describe missed it.
	return serverInstance{ServerID: m.Host.Ref, Flavor: m.InstanceType, Region: m.Zone}, nil
}

// refreshFlavors warms the allocatable cache from the Nova flavors API for the
// offered flavors. Call once at startup (flavor specs are immutable). Returns the
// number of offered flavors it could not resolve (each still covered by the
// pinned table if present).
func (b *ovhBackend) refreshFlavors(ctx context.Context) int {
	return b.flavors.resolve(ctx, b.offeredFlavors())
}

// offeredFlavors returns the distinct flavors across the configured offerings.
func (b *ovhBackend) offeredFlavors() []string {
	out := make([]string, 0, len(b.offerings))
	for _, off := range b.offerings {
		out = append(out, off.Flavor)
	}
	return out
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

var (
	_ providerkit.Backend = (*ovhBackend)(nil)
	_ providerkit.Deleter = (*ovhBackend)(nil)
)
