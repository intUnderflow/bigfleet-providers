package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// hetznerBackend is the Hetzner Cloud implementation of providerkit.Backend (+
// Deleter). It is pure substrate logic: it maps the kit's lifecycle calls onto
// Hetzner Cloud API calls and populates the substrate fields it knows
// (instance_type, zone, capacity_type, price_per_hour, interruption_probability,
// resources, allocatable, host). Fencing, idempotency, async dispatch,
// transition timeouts, shard_metadata, and the rest are providerkit's job — this
// file never touches them.
//
// Configure-bootstrap reconciliation: Hetzner Cloud user-data is immutable
// post-launch (it is consumed by cloud-init only at first boot), so CreateServer
// launches the server with the generic pre-binding --base-user-data, and the
// cluster-specific bootstrap blob is delivered later by ConfigureInstance over
// SSH (the real client's ApplyBootstrap). This keeps the kit's invariant that an
// Idle machine already carries a real, reachable host, and delivers the blob
// exactly once when the binding is established.
type hetznerBackend struct {
	providerName string // HostRef.provider label, e.g. "hetzner-nbg1"
	client       hcloudClient
	image        string // base image for CreateServer
	offerings    []offering
	pricing      *pricing
	serverTypes  *serverTypeResolver // resolves Machine.allocatable
	baseUserData []byte              // generic pre-binding bootstrap baked in at Create
	logger       *slog.Logger
}

func newHetznerBackend(providerName, image string, client hcloudClient, offerings []offering, pr *pricing, baseUserData []byte, logger *slog.Logger) (*hetznerBackend, error) {
	if len(offerings) == 0 {
		return nil, fmt.Errorf("hetzner backend: no offerings configured")
	}
	for _, off := range offerings {
		if _, err := off.capacityType(); err != nil {
			return nil, fmt.Errorf("hetzner backend: offering %s/%s: %w", off.ServerType, off.Location, err)
		}
		if off.ServerType == "" {
			return nil, fmt.Errorf("hetzner backend: offering with empty server_type")
		}
		// The provider registers multi-location (RequireZone), so a locationless
		// offering would only fail later at seed time — reject it up front.
		if off.Location == "" {
			return nil, fmt.Errorf("hetzner backend: offering %s with empty location", off.ServerType)
		}
	}
	return &hetznerBackend{
		providerName: providerName,
		client:       client,
		image:        image,
		offerings:    offerings,
		pricing:      pr,
		serverTypes:  newServerTypeResolver(client, logger),
		baseUserData: baseUserData,
		logger:       logger,
	}, nil
}

// slotID is the stable BigFleet machine id for one offering slot. A Speculative
// slot keeps this id across its whole lifecycle (Created servers are labelled
// with it, so DescribeManaged maps back to it).
func slotID(providerName string, capacity providerkit.CapacityType, off offering, i int) string {
	return fmt.Sprintf("%s/%s/%s/%s/%03d", providerName, capacity, off.ServerType, off.Location, i)
}

// speculativeSlots renders the configured offerings as Speculative Instance
// records with full, validated field shape. Reads only cached pricing state, so
// it never blocks on the network.
func (b *hetznerBackend) speculativeSlots() []providerkit.Instance {
	var out []providerkit.Instance
	for _, off := range b.offerings {
		capacity, _ := off.capacityType() // validated in newHetznerBackend
		for i := 0; i < off.Count; i++ {
			id := slotID(b.providerName, capacity, off, i)
			out = append(out, providerkit.Instance{
				ID:           id,
				State:        providerkit.StateSpeculative,
				InstanceType: off.ServerType,
				Zone:         off.Location,
				CapacityType: capacity,
				PricePerHour: b.pricing.price(off.ServerType, off.Location, capacity),
				// Hetzner Cloud is on-demand only: no spot market, so the genuine,
				// provider-declared interruption probability is exactly 0.
				InterruptionProbability: 0,
				Resources:               cloneMap(off.Resources),
				Allocatable:             b.serverTypes.allocatable(off.ServerType),
				Labels:                  slotLabels(off),
			})
		}
	}
	return out
}

func slotLabels(off offering) map[string]string {
	labels := cloneMap(off.Labels)
	if arch, ok := archLabel(off.ServerType); ok {
		if labels == nil {
			labels = map[string]string{}
		}
		labels["kubernetes.io/arch"] = arch
	}
	return labels
}

// Describe returns the substrate inventory: every offering slot as Speculative,
// upgraded to Idle (with its host) when a managed server already backs it, plus
// any orphan managed servers. The kit calls this to seed a fresh store; the
// persisted store is the primary restart path.
//
// A machine-id-labelled managed server owns its slot while it is alive, keeping
// the slot from being re-seeded Speculative so Create can't launch a duplicate
// under the same machine id. A deleting server is releasing its slot and is
// correctly absent (the slot returns to Speculative for re-provisioning).
// Untagged-but-running managed servers are surfaced as orphans under their
// server id so they are not lost.
func (b *hetznerBackend) Describe(ctx context.Context) ([]providerkit.Instance, error) {
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
			orphans = append(orphans, srv) // managed + running, but unlabelled
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
	// Labelled servers matching no current offering slot (offering shrank, or a
	// manually labelled server), then unlabelled-but-running managed servers.
	for id, srv := range bySlot {
		out = append(out, b.serverToIdle(id, srv))
	}
	for _, srv := range orphans {
		out = append(out, b.serverToIdle(srv.ServerID, srv))
	}
	return out, nil
}

func (b *hetznerBackend) serverToIdle(machineID string, srv serverInstance) providerkit.Instance {
	return providerkit.Instance{
		ID:                      machineID,
		State:                   providerkit.StateIdle,
		Host:                    providerkit.HostRef{Provider: b.providerName, Ref: srv.ServerID},
		InstanceType:            srv.ServerType,
		Zone:                    srv.Location,
		CapacityType:            providerkit.CapacityOnDemand,
		PricePerHour:            b.pricing.price(srv.ServerType, srv.Location, providerkit.CapacityOnDemand),
		InterruptionProbability: 0,
		Allocatable:             b.serverTypes.allocatable(srv.ServerType),
	}
}

// CreateInstance launches the Hetzner Cloud server for a Speculative slot and
// returns its host. The cluster-specific bootstrap is delivered later by
// ConfigureInstance, because Hetzner Cloud user-data is immutable post-launch.
func (b *hetznerBackend) CreateInstance(ctx context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	m := req.Machine
	srv, err := b.client.CreateServer(ctx, serverSpec{
		MachineID:        m.ID,
		ServerType:       m.InstanceType,
		Location:         m.Zone,
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
		Allocatable: b.serverTypes.allocatable(m.InstanceType),
	}, nil
}

// ConfigureInstance binds the running server to a cluster and delivers the
// opaque bootstrap blob (real impl: SSH).
func (b *hetznerBackend) ConfigureInstance(ctx context.Context, req providerkit.ConfigureInstanceRequest) error {
	srv, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	return b.client.ApplyBootstrap(ctx, srv, req.ClusterID, req.BootstrapBlob)
}

// DrainInstance cordons + drains the kubelet and removes the cluster binding,
// leaving the server running but unbound (Idle).
func (b *hetznerBackend) DrainInstance(ctx context.Context, req providerkit.DrainInstanceRequest) error {
	srv, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	return b.client.DrainNode(ctx, srv, req.GracePeriodSeconds)
}

// DeleteInstance deletes the Hetzner Cloud server; the slot returns to
// Speculative.
func (b *hetznerBackend) DeleteInstance(ctx context.Context, req providerkit.DeleteInstanceRequest) error {
	if req.Machine.Host.Ref == "" {
		return fmt.Errorf("delete: machine %s has no host", req.Machine.ID)
	}
	return b.client.DeleteServer(ctx, req.Machine.Host.Ref)
}

// resolveHost recovers the substrate server view (including the public IP needed
// for SSH-based Configure/Drain) for a machine the kit holds, by its server id.
func (b *hetznerBackend) resolveHost(ctx context.Context, m providerkit.Machine) (serverInstance, error) {
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
	// by id even if a transient describe missed it.
	return serverInstance{ServerID: m.Host.Ref, ServerType: m.InstanceType, Location: m.Zone}, nil
}

// pricePairs lists the distinct (serverType, location) pairs across offerings,
// to drive pricing.refresh without touching the List hot path.
func (b *hetznerBackend) pricePairs() []pricePair {
	seen := make(map[string]bool)
	var out []pricePair
	for _, off := range b.offerings {
		k := priceKey(off.ServerType, off.Location)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, pricePair{serverType: off.ServerType, location: off.Location})
	}
	return out
}

// refreshPrices warms / refreshes the on-demand price cache. Call at startup and
// on a timer. Returns the number of (type,location) pairs that failed.
func (b *hetznerBackend) refreshPrices(ctx context.Context) int {
	return b.pricing.refresh(ctx, b.pricePairs())
}

// refreshServerTypes warms the allocatable cache from the Hetzner ServerType API
// for the offered types. Call once at startup (server-type specs are immutable).
// Returns the number of offered types it could not resolve (each still covered
// by the pinned table if present).
func (b *hetznerBackend) refreshServerTypes(ctx context.Context) int {
	return b.serverTypes.resolve(ctx, b.offeredTypes())
}

// offeredTypes returns the distinct server types across the configured offerings.
func (b *hetznerBackend) offeredTypes() []string {
	out := make([]string, 0, len(b.offerings))
	for _, off := range b.offerings {
		out = append(out, off.ServerType)
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
	_ providerkit.Backend = (*hetznerBackend)(nil)
	_ providerkit.Deleter = (*hetznerBackend)(nil)
)
