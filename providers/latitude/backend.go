package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// latitudeBackend is the Latitude.sh implementation of providerkit.Backend (+
// Deleter). It is pure substrate logic: it maps the kit's lifecycle calls onto
// Latitude.sh API calls and populates the substrate fields it knows
// (instance_type, zone, capacity_type, price_per_hour, interruption_probability,
// resources, allocatable, host). Fencing, idempotency, async dispatch,
// transition timeouts, shard_metadata, and the rest are providerkit's job — this
// file never touches them.
//
// Latitude.sh is an on-demand bare-metal cloud, so the lifecycle is the cloud
// shape with a REAL Delete (DELETE /servers/{id} deprovisions the physical
// server). The capacity type is therefore ON_DEMAND, not BARE_METAL: since M73
// the shard only emits Delete for ON_DEMAND/SPOT, and declaring BARE_METAL would
// stop it ever reclaiming a deployed server, leaking money. interruption
// probability is a genuine, provider-declared 0 (Latitude bare metal is not a
// preemptible market).
//
// Configure-bootstrap reconciliation: Latitude user_data is consumed once at
// first boot (and stored as a Latitude resource), so CreateServer deploys with
// only the generic --base-user-data, and the cluster-specific bootstrap blob
// (the JOIN SECRET) is delivered later by ConfigureInstance over the
// pinned-host-key SSH channel.
//
// EnsureRunning: a tagged server the kit tracks as Idle/bound may be powered off
// out-of-band. ConfigureInstance and DrainInstance both EnsureRunning (power on
// + wait for reachability) BEFORE delivering the bootstrap / draining, so they
// never act on a stopped server.
type latitudeBackend struct {
	providerName    string // HostRef.provider label, e.g. "latitude-ash"
	client          latitudeClient
	operatingSystem string // OS slug for Server deploy
	offerings       []offering
	pricing         *pricing
	plans           *planResolver // resolves Machine.allocatable
	baseUserData    []byte        // generic pre-binding bootstrap baked in at deploy
	// ensureRunningTimeout / ensureRunningPoll bound EnsureRunning's power-on
	// wait. The kit's per-transition timeout (carried on ctx) is the real cap;
	// these only pace the poll.
	ensureRunningTimeout time.Duration
	ensureRunningPoll    time.Duration
	logger               *slog.Logger
}

func newLatitudeBackend(providerName, operatingSystem string, client latitudeClient, offerings []offering, pr *pricing, baseUserData []byte, logger *slog.Logger) (*latitudeBackend, error) {
	if len(offerings) == 0 {
		return nil, fmt.Errorf("latitude backend: no offerings configured")
	}
	for _, off := range offerings {
		if _, err := off.capacityType(); err != nil {
			return nil, fmt.Errorf("latitude backend: offering %s/%s: %w", off.Plan, off.Site, err)
		}
		if off.Plan == "" {
			return nil, fmt.Errorf("latitude backend: offering with empty plan")
		}
		// The provider registers multi-site (RequireZone), so a siteless offering
		// would only fail later at seed time — reject it up front.
		if off.Site == "" {
			return nil, fmt.Errorf("latitude backend: offering %s with empty site", off.Plan)
		}
	}
	return &latitudeBackend{
		providerName:         providerName,
		client:               client,
		operatingSystem:      operatingSystem,
		offerings:            offerings,
		pricing:              pr,
		plans:                newPlanResolver(client, logger),
		baseUserData:         baseUserData,
		ensureRunningTimeout: 5 * time.Minute,
		ensureRunningPoll:    3 * time.Second,
		logger:               logger,
	}, nil
}

// slotID is the stable BigFleet machine id for one offering slot. A Speculative
// slot keeps this id across its whole lifecycle (deployed servers are tagged
// with it, so DescribeManaged maps back to it).
func slotID(providerName string, capacity providerkit.CapacityType, off offering, i int) string {
	return fmt.Sprintf("%s/%s/%s/%s/%03d", providerName, capacity, off.Plan, off.Site, i)
}

// speculativeSlots renders the configured offerings as Speculative Instance
// records with full, validated field shape. Reads only cached pricing state, so
// it never blocks on the network.
func (b *latitudeBackend) speculativeSlots() []providerkit.Instance {
	var out []providerkit.Instance
	for _, off := range b.offerings {
		capacity, _ := off.capacityType() // validated in newLatitudeBackend
		for i := 0; i < off.Count; i++ {
			id := slotID(b.providerName, capacity, off, i)
			out = append(out, providerkit.Instance{
				ID:           id,
				State:        providerkit.StateSpeculative,
				InstanceType: off.Plan,
				Zone:         off.Site,
				CapacityType: capacity,
				PricePerHour: b.pricing.price(off.Plan, off.Site, capacity),
				// Latitude on-demand bare metal is not preemptible by the provider:
				// the genuine, provider-declared interruption probability is exactly 0.
				InterruptionProbability: 0,
				Resources:               cloneMap(off.Resources),
				Allocatable:             b.plans.allocatable(off.Plan),
				Labels:                  slotLabels(off),
			})
		}
	}
	return out
}

func slotLabels(off offering) map[string]string {
	labels := cloneMap(off.Labels)
	if acc, ok := acceleratorLabel(off.Plan); ok {
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
// A machine-id-tagged managed server owns its slot while it is alive, keeping
// the slot from being re-seeded Speculative so Create can't deploy a duplicate
// under the same machine id. A deprovisioning server is releasing its slot and
// is correctly absent (the slot returns to Speculative for re-provisioning).
// Untagged-but-running managed servers are surfaced as orphans under their
// server id so they are not lost.
//
// Describe DOES NOT power anything on — a tagged-but-stopped server stays Idle
// and reapable in the inventory, owning its slot (only Configure/Drain
// EnsureRunning).
func (b *latitudeBackend) Describe(ctx context.Context) ([]providerkit.Instance, error) {
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe managed servers: %w", err)
	}
	bySlot := make(map[string]serverInstance, len(managed))
	var orphans []serverInstance
	for _, srv := range managed {
		switch {
		case srv.MachineID != "":
			bySlot[srv.MachineID] = srv // owns its slot, powered on or not
		case srv.Running:
			orphans = append(orphans, srv) // managed + live, but untagged
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
	// manually tagged server), then untagged-but-running managed servers. Unlike
	// the slot path above (which sources instance_type/zone from the offering),
	// these source them from the live server view, which can omit them if the API
	// response is sparse. The provider registers RequireZone, so a machine with an
	// empty zone/instance_type would fail the kit's whole seed batch — skip such
	// servers with a warning (the FileStore is the primary recovery path for them).
	for id, srv := range bySlot {
		if inst, ok := b.serverToIdleChecked(id, srv); ok {
			out = append(out, inst)
		}
	}
	for _, srv := range orphans {
		if inst, ok := b.serverToIdleChecked(srv.ServerID, srv); ok {
			out = append(out, inst)
		}
	}
	return out, nil
}

// serverToIdleChecked is serverToIdle plus the required-field guard for the
// leftover/orphan paths: an Idle machine needs a non-empty instance_type (and,
// under RequireZone, a non-empty zone) or the kit rejects the entire seed batch.
// A server whose live view omits either is skipped with a warning rather than
// crashing first boot — the FileStore restores it on the next reconcile.
func (b *latitudeBackend) serverToIdleChecked(machineID string, srv serverInstance) (providerkit.Instance, bool) {
	if srv.Plan == "" || srv.Site == "" {
		if b.logger != nil {
			b.logger.Warn("skipping managed server with empty plan/site in Describe (FileStore is the recovery path)",
				"server", srv.ServerID, "machine", machineID, "plan", srv.Plan, "site", srv.Site)
		}
		return providerkit.Instance{}, false
	}
	return b.serverToIdle(machineID, srv), true
}

func (b *latitudeBackend) serverToIdle(machineID string, srv serverInstance) providerkit.Instance {
	return providerkit.Instance{
		ID:                      machineID,
		State:                   providerkit.StateIdle,
		Host:                    providerkit.HostRef{Provider: b.providerName, Ref: srv.ServerID},
		InstanceType:            srv.Plan,
		Zone:                    srv.Site,
		CapacityType:            providerkit.CapacityOnDemand,
		PricePerHour:            b.pricing.price(srv.Plan, srv.Site, providerkit.CapacityOnDemand),
		InterruptionProbability: 0,
		// Recover the per-replica request shape from a still-configured offering
		// for this plan, so an orphan / offering-shrank machine that re-binds via
		// Describe still matches its demand profile. Nil only for a truly unknown
		// plan, where the FileStore (the primary restart path) restores resources.
		Resources:   b.resourcesForPlan(srv.Plan, srv.Site),
		Allocatable: b.plans.allocatable(srv.Plan),
	}
}

// resourcesForPlan returns the per-replica resources of an offering matching the
// given plan, preferring an exact (plan, site) match and falling back to the
// same plan in any site. Nil when no offering covers the plan.
func (b *latitudeBackend) resourcesForPlan(plan, site string) map[string]string {
	var fallback map[string]string
	for _, off := range b.offerings {
		if off.Plan != plan {
			continue
		}
		if off.Site == site {
			return cloneMap(off.Resources)
		}
		if fallback == nil {
			fallback = off.Resources
		}
	}
	return cloneMap(fallback)
}

// CreateInstance deploys the Latitude.sh server for a Speculative slot and
// returns its host. The cluster-specific bootstrap is delivered later by
// ConfigureInstance, because Latitude user_data is first-boot-only.
func (b *latitudeBackend) CreateInstance(ctx context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	m := req.Machine
	srv, err := b.client.CreateServer(ctx, serverSpec{
		MachineID:        m.ID,
		Plan:             m.InstanceType,
		Site:             m.Zone,
		OperatingSystem:  b.operatingSystem,
		IdempotencyToken: req.OperationID,
		BaseUserData:     b.baseUserData,
	})
	if err != nil {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("deploy server %s: %w", m.InstanceType, err)
	}
	// The kit's empty-host guard only fires when BOTH HostRef fields are empty,
	// and Provider is always set here — so guard the server id explicitly. A host
	// with an empty Ref would settle the machine Idle, then break every later
	// Configure/Drain/Delete.
	if srv.ServerID == "" {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("deploy server %s returned no server id", m.InstanceType)
	}
	return providerkit.CreateInstanceResult{
		Host:        providerkit.HostRef{Provider: b.providerName, Ref: srv.ServerID},
		Allocatable: b.plans.allocatable(m.InstanceType),
	}, nil
}

// ConfigureInstance binds the running server to a cluster and delivers the
// opaque bootstrap blob (real impl: SSH on the pinned host key). It
// EnsureRunning first: a tracked server may have been powered off out-of-band,
// and delivering the bootstrap to a stopped server would fail spuriously.
func (b *latitudeBackend) ConfigureInstance(ctx context.Context, req providerkit.ConfigureInstanceRequest) error {
	srv, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	srv, err = b.ensureRunning(ctx, srv)
	if err != nil {
		return fmt.Errorf("configure: ensure running: %w", err)
	}
	return b.client.ApplyBootstrap(ctx, srv, req.ClusterID, req.BootstrapBlob)
}

// DrainInstance cordons + drains the kubelet and removes the cluster binding,
// leaving the server running but unbound (Idle). It EnsureRunning first for the
// same reason as Configure — a tracked-bound server may have been stopped
// out-of-band, and draining a stopped server would fail spuriously.
func (b *latitudeBackend) DrainInstance(ctx context.Context, req providerkit.DrainInstanceRequest) error {
	srv, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	srv, err = b.ensureRunning(ctx, srv)
	if err != nil {
		return fmt.Errorf("drain: ensure running: %w", err)
	}
	return b.client.DrainNode(ctx, srv, req.GracePeriodSeconds)
}

// DeleteInstance deprovisions the Latitude.sh server (and any resources this
// provider attached to it); the slot returns to Speculative.
func (b *latitudeBackend) DeleteInstance(ctx context.Context, req providerkit.DeleteInstanceRequest) error {
	if req.Machine.Host.Ref == "" {
		return fmt.Errorf("delete: machine %s has no host", req.Machine.ID)
	}
	return b.client.DeleteServer(ctx, req.Machine.Host.Ref, req.Machine.ID)
}

// ensureRunning powers a server on and waits for it to be reachable before
// Configure/Drain touch it. A server already powered on returns immediately. The
// caller's ctx (the kit's transition timeout) bounds the wait.
func (b *latitudeBackend) ensureRunning(ctx context.Context, srv serverInstance) (serverInstance, error) {
	cur, err := b.client.GetServer(ctx, srv.ServerID)
	if err != nil {
		// Fall back to the caller's view; a transient Get miss should not block a
		// server we can still address by id.
		cur = srv
	} else {
		cur = mergeServerView(srv, cur)
	}
	if cur.PoweredOn {
		return cur, nil
	}
	if b.logger != nil {
		b.logger.Info("server is powered off; powering on before bootstrap/drain", "server", cur.ServerID)
	}
	if err := b.client.PowerOn(ctx, cur.ServerID); err != nil {
		return cur, fmt.Errorf("power on server %s: %w", cur.ServerID, err)
	}
	deadline := time.Now().Add(b.ensureRunningTimeout)
	ticker := time.NewTicker(b.ensureRunningPoll)
	defer ticker.Stop()
	for {
		got, gerr := b.client.GetServer(ctx, cur.ServerID)
		if gerr == nil {
			cur = mergeServerView(cur, got)
			if cur.PoweredOn {
				return cur, nil
			}
		}
		select {
		case <-ctx.Done():
			return cur, fmt.Errorf("waiting for server %s to power on: %w", cur.ServerID, ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return cur, fmt.Errorf("server %s did not power on within %s", cur.ServerID, b.ensureRunningTimeout)
			}
		}
	}
}

// mergeServerView prefers the fresh view's fields but keeps the prior view's
// values where the fresh one is empty (e.g. a transient Get that omits the IP).
func mergeServerView(prior, fresh serverInstance) serverInstance {
	out := fresh
	if out.ServerID == "" {
		out.ServerID = prior.ServerID
	}
	if out.Plan == "" {
		out.Plan = prior.Plan
	}
	if out.Site == "" {
		out.Site = prior.Site
	}
	if out.PublicIPv4 == "" {
		out.PublicIPv4 = prior.PublicIPv4
	}
	if out.HostKeyFP == "" {
		out.HostKeyFP = prior.HostKeyFP
	}
	if out.MachineID == "" {
		out.MachineID = prior.MachineID
	}
	return out
}

// resolveHost recovers the substrate server view (including the public IPv4
// needed for SSH-based Configure/Drain) for a machine the kit holds, by its
// server id.
func (b *latitudeBackend) resolveHost(ctx context.Context, m providerkit.Machine) (serverInstance, error) {
	if m.Host.Ref == "" {
		return serverInstance{}, fmt.Errorf("machine %s has no host", m.ID)
	}
	srv, err := b.client.GetServer(ctx, m.Host.Ref)
	if err == nil && srv.ServerID != "" {
		return srv, nil
	}
	// Fall back to a minimal view; the real client can still address the server
	// by id even if a transient Get missed it.
	return serverInstance{ServerID: m.Host.Ref, Plan: m.InstanceType, Site: m.Zone}, nil
}

// pricePairs lists the distinct (plan, site) pairs across offerings, to drive
// pricing.refresh without touching the List hot path.
func (b *latitudeBackend) pricePairs() []pricePair {
	seen := make(map[string]bool)
	var out []pricePair
	for _, off := range b.offerings {
		k := priceKey(off.Plan, off.Site)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, pricePair{plan: off.Plan, site: off.Site})
	}
	return out
}

// refreshPrices warms / refreshes the on-demand price cache. Call at startup and
// on a timer. Returns the number of (plan,site) pairs that failed.
func (b *latitudeBackend) refreshPrices(ctx context.Context) int {
	return b.pricing.refresh(ctx, b.pricePairs())
}

// refreshPlans warms the allocatable cache from the Latitude Plans API for the
// offered plans. Call once at startup (plan specs are immutable). Returns the
// number of offered plans it could not resolve (each still covered by the pinned
// table if present).
func (b *latitudeBackend) refreshPlans(ctx context.Context) int {
	return b.plans.resolve(ctx, b.offeredPlans())
}

// offeredPlans returns the distinct plans across the configured offerings.
func (b *latitudeBackend) offeredPlans() []string {
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
	_ providerkit.Backend = (*latitudeBackend)(nil)
	_ providerkit.Deleter = (*latitudeBackend)(nil)
)
