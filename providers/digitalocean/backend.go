package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// digitaloceanBackend is the DigitalOcean implementation of providerkit.Backend
// (+ Deleter). It is pure substrate logic: it maps the kit's lifecycle calls onto
// DigitalOcean API calls and populates the substrate fields it knows
// (instance_type, zone, capacity_type, price_per_hour, interruption_probability,
// resources, allocatable, host). Fencing, idempotency, async dispatch,
// transition timeouts, shard_metadata, and the rest are providerkit's job — this
// file never touches them.
//
// Configure-bootstrap reconciliation: a Droplet's user_data is immutable
// post-launch (cloud-init consumes it only at first boot), so CreateDroplet
// launches the Droplet with the generic pre-binding --base-user-data (which
// installs the on-host agent), and the cluster-specific bootstrap blob is
// delivered later by ConfigureInstance over the agent's mutually-authenticated
// TLS channel (the real client's ApplyBootstrap). This keeps the kit's invariant
// that an Idle machine already carries a real, reachable host, and delivers the
// secret-bearing blob exactly once when the binding is established.
type digitaloceanBackend struct {
	providerName string // HostRef.provider label, e.g. "digitalocean-nyc3"
	client       doClient
	image        string // base image / snapshot for CreateDroplet
	offerings    []offering
	pricing      *pricing
	sizes        *sizeResolver // resolves Machine.allocatable
	baseUserData []byte        // generic pre-binding bootstrap baked in at Create
	logger       *slog.Logger
}

func newDigitaloceanBackend(providerName, image string, client doClient, offerings []offering, pr *pricing, baseUserData []byte, logger *slog.Logger) (*digitaloceanBackend, error) {
	if len(offerings) == 0 {
		return nil, fmt.Errorf("digitalocean backend: no offerings configured")
	}
	for _, off := range offerings {
		if _, err := off.capacityType(); err != nil {
			return nil, fmt.Errorf("digitalocean backend: offering %s/%s: %w", off.Size, off.Region, err)
		}
		if off.Size == "" {
			return nil, fmt.Errorf("digitalocean backend: offering with empty size")
		}
		// The provider registers multi-region (RequireZone), so a regionless
		// offering would only fail later at seed time — reject it up front.
		if off.Region == "" {
			return nil, fmt.Errorf("digitalocean backend: offering %s with empty region", off.Size)
		}
	}
	return &digitaloceanBackend{
		providerName: providerName,
		client:       client,
		image:        image,
		offerings:    offerings,
		pricing:      pr,
		sizes:        newSizeResolver(client, logger),
		baseUserData: baseUserData,
		logger:       logger,
	}, nil
}

// slotID is the stable BigFleet machine id for one offering slot. A Speculative
// slot keeps this id across its whole lifecycle (Created Droplets are tagged with
// it, so DescribeManaged maps back to it).
func slotID(providerName string, capacity providerkit.CapacityType, off offering, i int) string {
	return fmt.Sprintf("%s/%s/%s/%s/%03d", providerName, capacity, off.Size, off.Region, i)
}

// speculativeSlots renders the configured offerings as Speculative Instance
// records with full, validated field shape. Reads only cached pricing state, so
// it never blocks on the network.
func (b *digitaloceanBackend) speculativeSlots() []providerkit.Instance {
	var out []providerkit.Instance
	for _, off := range b.offerings {
		capacity, _ := off.capacityType() // validated in newDigitaloceanBackend
		for i := 0; i < off.Count; i++ {
			id := slotID(b.providerName, capacity, off, i)
			out = append(out, providerkit.Instance{
				ID:           id,
				State:        providerkit.StateSpeculative,
				InstanceType: off.Size,
				Zone:         off.Region,
				CapacityType: capacity,
				PricePerHour: b.pricing.price(off.Size, capacity),
				// DigitalOcean Droplets are on-demand only: no spot market, so the
				// genuine, provider-declared interruption probability is exactly 0.
				InterruptionProbability: dropletInterruptionProbability,
				Resources:               cloneMap(off.Resources),
				Allocatable:             b.sizes.allocatable(off.Size),
				Labels:                  cloneMap(off.Labels),
			})
		}
	}
	return out
}

// Describe returns the substrate inventory: every offering slot as Speculative,
// upgraded to Idle (with its host) when a managed Droplet already backs it, plus
// any orphan managed Droplets. The kit calls this to seed a fresh store; the
// persisted store is the primary restart path.
//
// A machine-id-tagged managed Droplet owns its slot while it is alive, keeping
// the slot from being re-seeded Speculative so Create can't launch a duplicate
// under the same machine id. A deleting Droplet is releasing its slot and is
// correctly absent (the slot returns to Speculative for re-provisioning).
// Untagged-but-running managed Droplets are surfaced as orphans under their
// Droplet id so they are not lost.
func (b *digitaloceanBackend) Describe(ctx context.Context) ([]providerkit.Instance, error) {
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe managed droplets: %w", err)
	}
	bySlot := make(map[string]dropletInstance, len(managed))
	var orphans []dropletInstance
	for _, drv := range managed {
		switch {
		case drv.MachineID != "":
			bySlot[drv.MachineID] = drv // owns its slot, running or not
		case drv.Active:
			orphans = append(orphans, drv) // managed + running, but untagged
		}
	}

	slots := b.speculativeSlots()
	out := make([]providerkit.Instance, 0, len(slots)+len(bySlot)+len(orphans))
	for _, slot := range slots {
		if drv, ok := bySlot[slot.ID]; ok {
			slot.State = providerkit.StateIdle
			slot.Host = providerkit.HostRef{Provider: b.providerName, Ref: drv.DropletID}
			delete(bySlot, slot.ID)
		}
		out = append(out, slot)
	}
	// Tagged Droplets matching no current offering slot (offering shrank, or a
	// manually tagged Droplet), then untagged-but-running managed Droplets.
	for id, drv := range bySlot {
		out = append(out, b.dropletToIdle(id, drv))
	}
	for _, drv := range orphans {
		out = append(out, b.dropletToIdle(drv.DropletID, drv))
	}
	return out, nil
}

func (b *digitaloceanBackend) dropletToIdle(machineID string, drv dropletInstance) providerkit.Instance {
	return providerkit.Instance{
		ID:                      machineID,
		State:                   providerkit.StateIdle,
		Host:                    providerkit.HostRef{Provider: b.providerName, Ref: drv.DropletID},
		InstanceType:            drv.Size,
		Zone:                    drv.Region,
		CapacityType:            providerkit.CapacityOnDemand,
		PricePerHour:            b.pricing.price(drv.Size, providerkit.CapacityOnDemand),
		InterruptionProbability: dropletInterruptionProbability,
		// Recover the per-replica request shape from a still-configured offering
		// for this size, so an orphan / offering-shrank machine that re-binds via
		// Describe still matches its demand profile. Nil only for a truly unknown
		// size, where the FileStore (the primary restart path) restores resources.
		Resources:   b.resourcesForSize(drv.Size, drv.Region),
		Allocatable: b.sizes.allocatable(drv.Size),
	}
}

// resourcesForSize returns the per-replica resources of an offering matching the
// given size, preferring an exact (size, region) match and falling back to the
// same size in any region. Nil when no offering covers the size.
func (b *digitaloceanBackend) resourcesForSize(size, region string) map[string]string {
	var fallback map[string]string
	for _, off := range b.offerings {
		if off.Size != size {
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

// CreateInstance launches the DigitalOcean Droplet for a Speculative slot and
// returns its host. The cluster-specific bootstrap is delivered later by
// ConfigureInstance, because a Droplet's user_data is immutable post-launch.
func (b *digitaloceanBackend) CreateInstance(ctx context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	m := req.Machine
	drv, err := b.client.CreateDroplet(ctx, dropletSpec{
		MachineID:        m.ID,
		Size:             m.InstanceType,
		Region:           m.Zone,
		Image:            b.image,
		IdempotencyToken: req.OperationID,
		BaseUserData:     b.baseUserData,
	})
	if err != nil {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("create droplet %s: %w", m.InstanceType, err)
	}
	// The kit's empty-host guard only fires when BOTH HostRef fields are empty,
	// and Provider is always set here — so guard the Droplet id explicitly. A host
	// with an empty Ref would settle the machine Idle, then break every later
	// Configure/Drain/Delete.
	if drv.DropletID == "" {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("create droplet %s returned no droplet id", m.InstanceType)
	}
	return providerkit.CreateInstanceResult{
		Host:        providerkit.HostRef{Provider: b.providerName, Ref: drv.DropletID},
		Allocatable: b.sizes.allocatable(m.InstanceType),
	}, nil
}

// ConfigureInstance binds the running Droplet to a cluster and delivers the
// opaque bootstrap blob (real impl: over the on-host agent's TLS channel).
func (b *digitaloceanBackend) ConfigureInstance(ctx context.Context, req providerkit.ConfigureInstanceRequest) error {
	drv, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	return b.client.ApplyBootstrap(ctx, drv, req.ClusterID, req.BootstrapBlob)
}

// DrainInstance cordons + drains the kubelet and removes the cluster binding,
// leaving the Droplet running but unbound (Idle).
func (b *digitaloceanBackend) DrainInstance(ctx context.Context, req providerkit.DrainInstanceRequest) error {
	drv, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	return b.client.DrainNode(ctx, drv, req.GracePeriodSeconds)
}

// DeleteInstance deletes the DigitalOcean Droplet; the slot returns to
// Speculative.
func (b *digitaloceanBackend) DeleteInstance(ctx context.Context, req providerkit.DeleteInstanceRequest) error {
	if req.Machine.Host.Ref == "" {
		return fmt.Errorf("delete: machine %s has no host", req.Machine.ID)
	}
	return b.client.DeleteDroplet(ctx, req.Machine.Host.Ref)
}

// resolveHost recovers the substrate Droplet view (including the public IP needed
// for the agent control channel) for a machine the kit holds, by its Droplet id.
func (b *digitaloceanBackend) resolveHost(ctx context.Context, m providerkit.Machine) (dropletInstance, error) {
	if m.Host.Ref == "" {
		return dropletInstance{}, fmt.Errorf("machine %s has no host", m.ID)
	}
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return dropletInstance{}, fmt.Errorf("describe managed droplets: %w", err)
	}
	for _, drv := range managed {
		if drv.DropletID == m.Host.Ref {
			return drv, nil
		}
	}
	// Fall back to a minimal view; the real client can still address the Droplet
	// by id even if a transient describe missed it.
	return dropletInstance{DropletID: m.Host.Ref, MachineID: m.ID, Size: m.InstanceType, Region: m.Zone}, nil
}

// refreshPrices warms / refreshes the on-demand price cache. Call at startup and
// on a timer. Returns the number of sizes that failed.
func (b *digitaloceanBackend) refreshPrices(ctx context.Context) int {
	return b.pricing.refresh(ctx, b.offeredSizes())
}

// refreshSizes warms the allocatable cache from the DigitalOcean Sizes API for
// the offered sizes. Call once at startup (size specs are immutable). Returns the
// number of offered sizes it could not resolve (each still covered by the pinned
// table if present).
func (b *digitaloceanBackend) refreshSizes(ctx context.Context) int {
	return b.sizes.resolve(ctx, b.offeredSizes())
}

// offeredSizes returns the distinct sizes across the configured offerings.
func (b *digitaloceanBackend) offeredSizes() []string {
	out := make([]string, 0, len(b.offerings))
	for _, off := range b.offerings {
		out = append(out, off.Size)
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
	_ providerkit.Backend = (*digitaloceanBackend)(nil)
	_ providerkit.Deleter = (*digitaloceanBackend)(nil)
)
