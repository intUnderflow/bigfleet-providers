package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// scalewayBackend is the Scaleway implementation of providerkit.Backend. It is
// pure substrate logic: it maps the kit's lifecycle calls onto Scaleway API
// calls and populates the substrate fields it knows (instance_type, zone,
// capacity_type, price_per_hour, interruption_probability, resources,
// allocatable, host). Fencing, idempotency, async dispatch, transition timeouts,
// shard_metadata, and the rest are providerkit's job — this file never touches
// them.
//
// One backend serves one substrate (Instances OR Elastic Metal) in one
// region/zone. The Delete capability is substrate-specific and is therefore NOT
// on this type: the Instances (cloud) path wraps it in a [cloudBackend] that
// adds DeleteInstance (providerkit.Deleter), while the Elastic Metal
// (free-pool) path uses scalewayBackend directly, so the kit answers Delete with
// codes.Unimplemented.
//
// Configure-bootstrap reconciliation: Scaleway cloud-init user-data is consumed
// only at first boot, so CreateServer launches the server with the generic
// pre-binding --base-user-data (which installs the on-host agent), and the
// cluster-specific bootstrap blob is delivered later by ConfigureInstance over a
// mutually-authenticated TLS channel (the real client's ApplyBootstrap). This
// keeps the kit's invariant that an Idle machine already carries a real,
// reachable host, and delivers the blob exactly once when the binding is
// established.
type scalewayBackend struct {
	providerName string // HostRef.provider label, e.g. "scaleway-fr-par"
	capacity     providerkit.CapacityType
	client       scwClient
	image        string // base image for CreateServer
	offerings    []offering
	pricing      *pricing
	types        *commercialTypeResolver // resolves Machine.allocatable
	baseUserData []byte                  // generic pre-binding bootstrap baked in at Create
	logger       *slog.Logger
}

func newScalewayBackend(providerName string, capacity providerkit.CapacityType, image string, client scwClient, offerings []offering, pr *pricing, baseUserData []byte, logger *slog.Logger) (*scalewayBackend, error) {
	if len(offerings) == 0 {
		return nil, fmt.Errorf("scaleway backend: no offerings configured")
	}
	for _, off := range offerings {
		cap, err := off.capacityType()
		if err != nil {
			return nil, fmt.Errorf("scaleway backend: offering %s/%s: %w", off.CommercialType, off.Zone, err)
		}
		if cap != capacity {
			return nil, fmt.Errorf("scaleway backend: offering %s/%s declares capacity_type %s but this process serves %s (one substrate per process)", off.CommercialType, off.Zone, cap, capacity)
		}
		if off.CommercialType == "" {
			return nil, fmt.Errorf("scaleway backend: offering with empty commercial_type")
		}
		// The provider registers multi-zone (RequireZone), so a zoneless offering
		// would only fail later at seed time — reject it up front.
		if off.Zone == "" {
			return nil, fmt.Errorf("scaleway backend: offering %s with empty zone", off.CommercialType)
		}
	}
	return &scalewayBackend{
		providerName: providerName,
		capacity:     capacity,
		client:       client,
		image:        image,
		offerings:    offerings,
		pricing:      pr,
		types:        newCommercialTypeResolver(client, logger),
		baseUserData: baseUserData,
		logger:       logger,
	}, nil
}

// slotID is the stable BigFleet machine id for one offering slot. A Speculative
// slot keeps this id across its whole lifecycle (Created servers are tagged with
// it, so DescribeManaged maps back to it).
func slotID(providerName string, capacity providerkit.CapacityType, off offering, i int) string {
	return fmt.Sprintf("%s/%s/%s/%s/%03d", providerName, capacity, off.CommercialType, off.Zone, i)
}

// speculativeSlots renders the configured offerings as Speculative Instance
// records with full, validated field shape. Reads only cached pricing/spec
// state, so it never blocks on the network.
func (b *scalewayBackend) speculativeSlots() []providerkit.Instance {
	var out []providerkit.Instance
	for _, off := range b.offerings {
		for i := 0; i < off.Count; i++ {
			id := slotID(b.providerName, b.capacity, off, i)
			out = append(out, providerkit.Instance{
				ID:           id,
				State:        providerkit.StateSpeculative,
				InstanceType: off.CommercialType,
				Zone:         off.Zone,
				CapacityType: b.capacity,
				PricePerHour: b.pricing.price(off.CommercialType, off.Zone, b.capacity),
				// Scaleway has no spot market (neither Instances nor Elastic Metal),
				// so the genuine, provider-declared interruption probability is 0.
				InterruptionProbability: 0,
				Resources:               cloneMap(off.Resources),
				Allocatable:             b.types.allocatable(off.CommercialType),
				Labels:                  slotLabels(off),
			})
		}
	}
	return out
}

func slotLabels(off offering) map[string]string {
	labels := cloneMap(off.Labels)
	add := func(k, v string) {
		if labels == nil {
			labels = map[string]string{}
		}
		labels[k] = v
	}
	if arch, ok := archLabel(off.CommercialType); ok {
		add("kubernetes.io/arch", arch)
	}
	if accel, ok := gpuLabel(off.CommercialType); ok {
		add("accelerator-type", accel)
	}
	return labels
}

// Describe returns the substrate inventory: every offering slot as Speculative,
// upgraded to Idle (with its host) when a managed server already backs it, plus
// any orphan managed servers. The kit calls this to seed a fresh store; the
// persisted store is the primary restart path.
//
// A machine-id-tagged managed server owns its slot while it is alive, keeping
// the slot from being re-seeded Speculative so Create can't launch a duplicate
// under the same machine id. A deleting server is releasing its slot and is
// correctly absent (the slot returns to Speculative for re-provisioning).
// Untagged-but-running managed servers are surfaced as orphans under their
// server id so they are not lost.
func (b *scalewayBackend) Describe(ctx context.Context) ([]providerkit.Instance, error) {
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe managed servers: %w", err)
	}
	bySlot := make(map[string]serverInstance, len(managed))
	var orphans []serverInstance
	for _, srv := range managed {
		switch {
		case srv.MachineID != "":
			bySlot[srv.MachineID] = srv // owns its slot, running or not
		case srv.Running:
			orphans = append(orphans, srv) // managed + running, but untagged
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

func (b *scalewayBackend) serverToIdle(machineID string, srv serverInstance) providerkit.Instance {
	return providerkit.Instance{
		ID:                      machineID,
		State:                   providerkit.StateIdle,
		Host:                    providerkit.HostRef{Provider: b.providerName, Ref: srv.ServerID},
		InstanceType:            srv.CommercialType,
		Zone:                    srv.Zone,
		CapacityType:            b.capacity,
		PricePerHour:            b.pricing.price(srv.CommercialType, srv.Zone, b.capacity),
		InterruptionProbability: 0,
		// Recover the per-replica request shape from a still-configured offering for
		// this commercial type, so an orphan / offering-shrank machine that re-binds
		// via Describe still matches its demand profile. Nil only for a truly unknown
		// type, where the FileStore (the primary restart path) restores resources.
		Resources:   b.resourcesForType(srv.CommercialType, srv.Zone),
		Allocatable: b.types.allocatable(srv.CommercialType),
	}
}

// resourcesForType returns the per-replica resources of an offering matching the
// given commercial type, preferring an exact (type, zone) match and falling back
// to the same type in any zone. Nil when no offering covers the type.
func (b *scalewayBackend) resourcesForType(commercialType, zone string) map[string]string {
	var fallback map[string]string
	for _, off := range b.offerings {
		if off.CommercialType != commercialType {
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

// CreateInstance provisions the Scaleway server for a Speculative slot and
// returns its host. The cluster-specific bootstrap is delivered later by
// ConfigureInstance, because Scaleway user-data is consumed only at first boot.
func (b *scalewayBackend) CreateInstance(ctx context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	m := req.Machine
	srv, err := b.client.CreateServer(ctx, serverSpec{
		MachineID:        m.ID,
		CommercialType:   m.InstanceType,
		Zone:             m.Zone,
		Image:            b.image,
		IdempotencyToken: req.OperationID,
		BaseUserData:     b.baseUserData,
	})
	if err != nil {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("create server %s: %w", m.InstanceType, err)
	}
	// The kit's empty-host guard only fires when BOTH HostRef fields are empty,
	// and Provider is always set here — so guard the server id explicitly. A host
	// with an empty Ref would settle the machine Idle, then break every later
	// Configure/Drain/Delete.
	if srv.ServerID == "" {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("create server %s returned no server id", m.InstanceType)
	}
	return providerkit.CreateInstanceResult{
		Host:        providerkit.HostRef{Provider: b.providerName, Ref: srv.ServerID},
		Allocatable: b.types.allocatable(m.InstanceType),
	}, nil
}

// ConfigureInstance binds the running server to a cluster and delivers the opaque
// bootstrap blob (real impl: published for the on-host agent to fetch over mTLS).
func (b *scalewayBackend) ConfigureInstance(ctx context.Context, req providerkit.ConfigureInstanceRequest) error {
	srv, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	return b.client.ApplyBootstrap(ctx, srv, req.ClusterID, req.BootstrapBlob)
}

// DrainInstance cordons + drains the kubelet and removes the cluster binding,
// leaving the server running but unbound (Idle).
func (b *scalewayBackend) DrainInstance(ctx context.Context, req providerkit.DrainInstanceRequest) error {
	srv, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	return b.client.DrainNode(ctx, srv, req.GracePeriodSeconds)
}

// resolveHost recovers the substrate server view (including the public IP the
// control channel needs) for a machine the kit holds, by its server id.
func (b *scalewayBackend) resolveHost(ctx context.Context, m providerkit.Machine) (serverInstance, error) {
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
	// Fall back to a minimal view; the real client addresses the agent channel by
	// machine id (which the kit always knows), so carry it through even when a
	// transient describe missed the server.
	return serverInstance{ServerID: m.Host.Ref, MachineID: m.ID, CommercialType: m.InstanceType, Zone: m.Zone}, nil
}

// pricePairs lists the distinct (commercialType, zone) pairs across offerings, to
// drive pricing.refresh without touching the List hot path.
func (b *scalewayBackend) pricePairs() []pricePair {
	seen := make(map[string]bool)
	var out []pricePair
	for _, off := range b.offerings {
		k := priceKey(off.CommercialType, off.Zone)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, pricePair{commercialType: off.CommercialType, zone: off.Zone})
	}
	return out
}

// refreshPrices warms / refreshes the on-demand price cache. Call at startup and
// on a timer. Returns the number of (type,zone) pairs that failed.
func (b *scalewayBackend) refreshPrices(ctx context.Context) int {
	return b.pricing.refresh(ctx, b.pricePairs())
}

// refreshTypes warms the allocatable cache from the Scaleway catalogue for the
// offered types. Call once at startup (specs are immutable). Returns the number
// of offered types it could not resolve (each still covered by the pinned table
// if present).
func (b *scalewayBackend) refreshTypes(ctx context.Context) int {
	return b.types.resolve(ctx, b.offeredTypes())
}

// offeredTypes returns the distinct commercial types across the offerings.
func (b *scalewayBackend) offeredTypes() []string {
	out := make([]string, 0, len(b.offerings))
	for _, off := range b.offerings {
		out = append(out, off.CommercialType)
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

// cloudBackend is the Instances (ON_DEMAND) backend: a scalewayBackend that also
// implements providerkit.Deleter, because a cloud VM can be torn down and its
// slot returned to Speculative. The Elastic Metal (free-pool) path uses the bare
// *scalewayBackend, which does NOT implement Deleter, so the kit answers Delete
// with codes.Unimplemented.
type cloudBackend struct {
	*scalewayBackend
}

// DeleteInstance tears the Scaleway Instance down; the slot returns to
// Speculative.
func (b *cloudBackend) DeleteInstance(ctx context.Context, req providerkit.DeleteInstanceRequest) error {
	if req.Machine.Host.Ref == "" {
		return fmt.Errorf("delete: machine %s has no host", req.Machine.ID)
	}
	return b.client.DeleteServer(ctx, req.Machine.Host.Ref)
}

var (
	_ providerkit.Backend = (*scalewayBackend)(nil)
	_ providerkit.Backend = (*cloudBackend)(nil)
	_ providerkit.Deleter = (*cloudBackend)(nil)
)
