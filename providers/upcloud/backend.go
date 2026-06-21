package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// upcloudBackend is the UpCloud implementation of providerkit.Backend (+
// Deleter). It is pure substrate logic: it maps the kit's lifecycle calls onto
// UpCloud API calls and populates the substrate fields it knows (instance_type,
// zone, capacity_type, price_per_hour, interruption_probability, resources,
// allocatable, host). Fencing, idempotency, async dispatch, transition
// timeouts, shard_metadata, and the rest are providerkit's job — this file
// never touches them.
//
// Configure-bootstrap reconciliation: UpCloud user-data is consumed by
// cloud-init only at first boot, so CreateServer launches the server with the
// generic pre-binding --base-user-data (which installs the on-host bootstrap
// hook), and the cluster-specific, secret-bearing bootstrap blob is delivered
// later by ConfigureInstance over SSH with a VERIFIED, pinned host key (the real
// client's ApplyBootstrap). This keeps the kit's invariant that an Idle machine
// already carries a real, reachable host, and delivers the secret exactly once
// when the binding is established.
//
// Power-on safety (§4.6): a tracked-Idle/Configured UpCloud server can be
// stopped out-of-band, so ConfigureInstance and DrainInstance both EnsureRunning
// BEFORE their SSH work — otherwise the transition would hang against a stopped
// host until the kit's timeout fired it to FAILED.
type upcloudBackend struct {
	providerName string // HostRef.provider label, e.g. "upcloud-fi-hel1"
	template     string // OS template UUID to clone at CreateServer
	client       upcloudClient
	offerings    []offering
	pricing      *pricing
	plans        *planResolver // resolves Machine.allocatable
	baseUserData []byte        // generic pre-binding bootstrap baked in at Create
	logger       *slog.Logger
}

func newUpcloudBackend(providerName, template string, client upcloudClient, offerings []offering, pr *pricing, baseUserData []byte, logger *slog.Logger) (*upcloudBackend, error) {
	if len(offerings) == 0 {
		return nil, fmt.Errorf("upcloud backend: no offerings configured")
	}
	for _, off := range offerings {
		if _, err := off.capacityType(); err != nil {
			return nil, fmt.Errorf("upcloud backend: offering %s/%s: %w", off.Plan, off.Zone, err)
		}
		if off.Plan == "" {
			return nil, fmt.Errorf("upcloud backend: offering with empty plan")
		}
		// The provider registers multi-zone (RequireZone), so a zoneless offering
		// would only fail later at seed time — reject it up front.
		if off.Zone == "" {
			return nil, fmt.Errorf("upcloud backend: offering %s with empty zone", off.Plan)
		}
	}
	return &upcloudBackend{
		providerName: providerName,
		template:     template,
		client:       client,
		offerings:    offerings,
		pricing:      pr,
		plans:        newPlanResolver(client, logger),
		baseUserData: baseUserData,
		logger:       logger,
	}, nil
}

// slotID is the stable BigFleet machine id for one offering slot. A Speculative
// slot keeps this id across its whole lifecycle (Created servers are labelled
// with it, so DescribeManaged maps back to it).
func slotID(providerName string, capacity providerkit.CapacityType, off offering, i int) string {
	return fmt.Sprintf("%s/%s/%s/%s/%03d", providerName, capacity, off.Plan, off.Zone, i)
}

// speculativeSlots renders the configured offerings as Speculative Instance
// records with full, validated field shape. Reads only cached pricing/plan
// state, so it never blocks on the network.
func (b *upcloudBackend) speculativeSlots() []providerkit.Instance {
	var out []providerkit.Instance
	for _, off := range b.offerings {
		capacity, _ := off.capacityType() // validated in newUpcloudBackend
		for i := 0; i < off.Count; i++ {
			id := slotID(b.providerName, capacity, off, i)
			out = append(out, providerkit.Instance{
				ID:           id,
				State:        providerkit.StateSpeculative,
				InstanceType: off.Plan,
				Zone:         off.Zone,
				CapacityType: capacity,
				PricePerHour: b.pricing.price(off, capacity),
				// UpCloud cloud servers are on-demand only: there is no spot market,
				// so the genuine, provider-declared interruption probability is
				// exactly 0.
				InterruptionProbability: upcloudInterruptionProbability,
				Resources:               cloneMap(off.Resources),
				Allocatable:             b.plans.allocatable(off.Plan),
				Labels:                  cloneMap(off.Labels),
			})
		}
	}
	return out
}

// Describe returns the substrate inventory: every offering slot as Speculative,
// upgraded to Idle (with its host) when a managed server already backs it, plus
// any orphan managed servers. The kit calls this to seed a fresh store; the
// persisted store is the primary restart path.
//
// A machine-id-labelled managed server owns its slot while it is alive — running
// OR stopped — keeping the slot from being re-seeded Speculative so Create can't
// launch a duplicate under the same machine id, and so a server stopped
// out-of-band is reported Idle (reapable) rather than dropped or double-
// provisioned. A deleting server is releasing its slot and is correctly absent
// (the slot returns to Speculative). Unlabelled-but-running managed servers are
// surfaced as orphans under their UUID so they are not lost.
func (b *upcloudBackend) Describe(ctx context.Context) ([]providerkit.Instance, error) {
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe managed servers: %w", err)
	}
	bySlot := make(map[string]serverInstance, len(managed))
	var orphans []serverInstance
	for _, srv := range managed {
		switch {
		case srv.MachineID != "":
			bySlot[srv.MachineID] = srv // owns its slot, running or stopped
		case srv.Running:
			orphans = append(orphans, srv) // managed + running, but unlabelled
		}
	}

	slots := b.speculativeSlots()
	out := make([]providerkit.Instance, 0, len(slots)+len(bySlot)+len(orphans))
	for _, slot := range slots {
		if srv, ok := bySlot[slot.ID]; ok {
			slot.State = providerkit.StateIdle
			slot.Host = providerkit.HostRef{Provider: b.providerName, Ref: srv.UUID}
			delete(bySlot, slot.ID)
		}
		out = append(out, slot)
	}
	// Labelled servers matching no current offering slot (offering shrank, or a
	// manually labelled server), then unlabelled-but-running managed servers.
	for id, srv := range bySlot {
		out = append(out, b.serverToIdle(id, srv))
	}
	for _, srv := range orphans {
		out = append(out, b.serverToIdle(srv.UUID, srv))
	}
	return out, nil
}

func (b *upcloudBackend) serverToIdle(machineID string, srv serverInstance) providerkit.Instance {
	inst := providerkit.Instance{
		ID:                      machineID,
		State:                   providerkit.StateIdle,
		Host:                    providerkit.HostRef{Provider: b.providerName, Ref: srv.UUID},
		InstanceType:            srv.Plan,
		Zone:                    srv.Zone,
		CapacityType:            providerkit.CapacityOnDemand,
		PricePerHour:            b.pricing.priceFor(srv.Plan, srv.Zone, providerkit.CapacityOnDemand),
		InterruptionProbability: upcloudInterruptionProbability,
		// Recover the per-replica request shape from a still-configured offering
		// for this plan, so an orphan / offering-shrank machine that re-binds via
		// Describe still matches its demand profile. Nil only for a truly unknown
		// plan, where the FileStore (the primary restart path) restores resources.
		Resources: b.resourcesForPlan(srv.Plan, srv.Zone),
	}
	// Only declare allocatable when we also know the per-replica resources. Setting
	// allocatable (hardware total) while resources is nil is the inconsistent state
	// the engine reads as density = allocatable / <unknown>; leaving both nil makes
	// the kit treat allocatable == resources (a safe density of 1) for an orphan we
	// cannot size.
	if inst.Resources != nil {
		inst.Allocatable = b.plans.allocatable(srv.Plan)
	}
	return inst
}

// resourcesForPlan returns the per-replica resources of an offering matching the
// given plan, preferring an exact (plan, zone) match and falling back to the
// same plan in any zone. Nil when no offering covers the plan.
func (b *upcloudBackend) resourcesForPlan(plan, zone string) map[string]string {
	var fallback map[string]string
	for _, off := range b.offerings {
		if off.Plan != plan {
			continue
		}
		if off.Zone == zone {
			return cloneMap(off.Resources)
		}
		if fallback == nil {
			fallback = off.Resources
		}
	}
	return cloneMap(fallback)
}

// CreateInstance launches the UpCloud server for a Speculative slot and returns
// its host. The cluster-specific bootstrap is delivered later by
// ConfigureInstance, because UpCloud user-data is consumed only at first boot.
func (b *upcloudBackend) CreateInstance(ctx context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	m := req.Machine
	srv, err := b.client.CreateServer(ctx, serverSpec{
		MachineID:        m.ID,
		Plan:             m.InstanceType,
		Zone:             m.Zone,
		Template:         b.template,
		IdempotencyToken: req.OperationID,
		BaseUserData:     b.baseUserData,
	})
	if err != nil {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("create server %s: %w", m.InstanceType, err)
	}
	// The kit's empty-host guard only fires when BOTH HostRef fields are empty,
	// and Provider is always set here — so guard the UUID explicitly. A host with
	// an empty Ref would settle the machine Idle, then break every later
	// Configure/Drain/Delete.
	if srv.UUID == "" {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("create server %s returned no UUID", m.InstanceType)
	}
	return providerkit.CreateInstanceResult{
		Host:        providerkit.HostRef{Provider: b.providerName, Ref: srv.UUID},
		Allocatable: b.plans.allocatable(m.InstanceType),
	}, nil
}

// ConfigureInstance powers the server on if it was stopped out-of-band, then
// binds the running server to a cluster and delivers the opaque bootstrap blob
// (real impl: SSH over a verified, pinned host key).
func (b *upcloudBackend) ConfigureInstance(ctx context.Context, req providerkit.ConfigureInstanceRequest) error {
	srv, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	srv, err = b.client.EnsureRunning(ctx, srv)
	if err != nil {
		return fmt.Errorf("configure: ensure running: %w", err)
	}
	return b.client.ApplyBootstrap(ctx, srv, req.ClusterID, req.BootstrapBlob)
}

// DrainInstance powers the server on if it was stopped out-of-band, then cordons
// + drains the kubelet and removes the cluster binding, leaving the server
// running but unbound (Idle).
func (b *upcloudBackend) DrainInstance(ctx context.Context, req providerkit.DrainInstanceRequest) error {
	srv, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	srv, err = b.client.EnsureRunning(ctx, srv)
	if err != nil {
		return fmt.Errorf("drain: ensure running: %w", err)
	}
	return b.client.DrainNode(ctx, srv, req.GracePeriodSeconds)
}

// DeleteInstance stops and deletes the UpCloud server AND its attached storage;
// the slot returns to Speculative.
func (b *upcloudBackend) DeleteInstance(ctx context.Context, req providerkit.DeleteInstanceRequest) error {
	if req.Machine.Host.Ref == "" {
		return fmt.Errorf("delete: machine %s has no host", req.Machine.ID)
	}
	return b.client.DeleteServer(ctx, req.Machine.Host.Ref)
}

// resolveHost recovers the substrate server view (including the public IP needed
// for SSH-based Configure/Drain, and the pinned host-key fingerprint) for a
// machine the kit holds, by its server UUID.
func (b *upcloudBackend) resolveHost(ctx context.Context, m providerkit.Machine) (serverInstance, error) {
	if m.Host.Ref == "" {
		return serverInstance{}, fmt.Errorf("machine %s has no host", m.ID)
	}
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return serverInstance{}, fmt.Errorf("describe managed servers: %w", err)
	}
	for _, srv := range managed {
		if srv.UUID == m.Host.Ref {
			return srv, nil
		}
	}
	// Fall back to a minimal view; the real client can still address the server by
	// UUID even if a transient describe missed it.
	return serverInstance{UUID: m.Host.Ref, MachineID: m.ID, Plan: m.InstanceType, Zone: m.Zone}, nil
}

// refreshPlans warms the allocatable cache from the UpCloud Plans API for the
// offered plans. Call once at startup (plan specs are immutable). Returns the
// number of offered plans it could not resolve (each still covered by the pinned
// table if present).
func (b *upcloudBackend) refreshPlans(ctx context.Context) int {
	return b.plans.resolve(ctx, b.offeredPlans())
}

// offeredPlans returns the distinct plans across the configured offerings.
func (b *upcloudBackend) offeredPlans() []string {
	out := make([]string, 0, len(b.offerings))
	for _, off := range b.offerings {
		out = append(out, off.Plan)
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
	_ providerkit.Backend = (*upcloudBackend)(nil)
	_ providerkit.Deleter = (*upcloudBackend)(nil)
)
