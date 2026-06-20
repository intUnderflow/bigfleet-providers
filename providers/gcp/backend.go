package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// gcpBackend is the Google Compute Engine implementation of providerkit.Backend
// (+ Deleter). It is pure substrate logic: it maps the kit's lifecycle calls
// onto GCE API calls and populates the substrate fields it knows (instance_type,
// zone, capacity_type, price_per_hour, interruption_probability, resources,
// allocatable, host). Fencing, idempotency, async dispatch, transition timeouts,
// shard_metadata, and the rest are providerkit's job — this file never touches
// them.
//
// Configure-bootstrap reconciliation: a GCE instance consumes its startup-script
// metadata at boot, so Insert launches the instance with the generic pre-binding
// --base-startup-script (not yet joined to any cluster), and the cluster-specific
// bootstrap blob is delivered later by ConfigureInstance, which writes the blob
// to the instance's `startup-script` metadata and resets it so the node joins.
// This keeps the kit's invariant that an Idle machine already carries a real,
// reachable host, and delivers the blob exactly once when the binding is set.
type gcpBackend struct {
	providerName      string // HostRef.provider label, e.g. "gcp-us-central1"
	region            string
	client            gceClient
	offerings         []offering
	pricing           *pricing
	interruption      *interruption
	machineTypes      *machineTypeResolver // resolves Machine.allocatable
	baseStartupScript []byte               // generic pre-binding bootstrap baked in at Insert
	logger            *slog.Logger
}

func newGCPBackend(providerName, region string, client gceClient, offerings []offering, pr *pricing, in *interruption, baseStartupScript []byte, logger *slog.Logger) (*gcpBackend, error) {
	if len(offerings) == 0 {
		return nil, fmt.Errorf("gcp backend: no offerings configured")
	}
	for _, off := range offerings {
		if _, err := off.capacityType(); err != nil {
			return nil, fmt.Errorf("gcp backend: offering %s/%s: %w", off.MachineType, off.Zone, err)
		}
		if off.MachineType == "" {
			return nil, fmt.Errorf("gcp backend: offering with empty machine_type")
		}
		// The provider registers multi-zone (RequireZone), so a zoneless offering
		// would only fail later at seed time — reject it up front.
		if off.Zone == "" {
			return nil, fmt.Errorf("gcp backend: offering %s with empty zone", off.MachineType)
		}
		// Every offered type must be priced; an unpinned type would otherwise
		// silently publish price_per_hour = 0 (including SPOT via spotFraction×0),
		// skewing effective-cost ranking and violating the "never zero for a real
		// VM" intent. Fail loudly so the operator pins the price (see pricing.go).
		if !pr.hasPrice(off.MachineType) {
			return nil, fmt.Errorf("gcp backend: offering %s has no pinned price for region %q (add it to pricing.go's onDemand table)", off.MachineType, region)
		}
	}
	return &gcpBackend{
		providerName:      providerName,
		region:            region,
		client:            client,
		offerings:         offerings,
		pricing:           pr,
		interruption:      in,
		machineTypes:      newMachineTypeResolver(client, logger),
		baseStartupScript: baseStartupScript,
		logger:            logger,
	}, nil
}

// slotID is the stable BigFleet machine id for one offering slot. A Speculative
// slot keeps this id across its whole lifecycle (Created instances record it in
// bigfleet-machine-id instance metadata, so DescribeManaged maps back to it).
func slotID(providerName string, capacity providerkit.CapacityType, off offering, i int) string {
	return fmt.Sprintf("%s/%s/%s/%s/%03d", providerName, capacity, off.MachineType, off.Zone, i)
}

// speculativeSlots renders the configured offerings as Speculative Instance
// records with full, validated field shape. Reads only cached pricing /
// interruption state, so it never blocks on the network.
func (b *gcpBackend) speculativeSlots() []providerkit.Instance {
	var out []providerkit.Instance
	for _, off := range b.offerings {
		capacity, _ := off.capacityType() // validated in newGCPBackend
		for i := 0; i < off.Count; i++ {
			id := slotID(b.providerName, capacity, off, i)
			out = append(out, providerkit.Instance{
				ID:                      id,
				State:                   providerkit.StateSpeculative,
				InstanceType:            off.MachineType,
				Zone:                    off.Zone,
				CapacityType:            capacity,
				PricePerHour:            b.pricing.price(off.MachineType, capacity),
				InterruptionProbability: b.interruption.probability(id, off.MachineType, capacity),
				Resources:               cloneMap(off.Resources),
				Allocatable:             b.machineTypes.allocatable(off.MachineType),
				Labels:                  slotLabels(off),
			})
		}
	}
	return out
}

func slotLabels(off offering) map[string]string {
	labels := cloneMap(off.Labels)
	if acc, ok := acceleratorLabel(off.MachineType); ok {
		if labels == nil {
			labels = map[string]string{}
		}
		labels["bigfleet.io/accelerator"] = acc
	}
	return labels
}

// Describe returns the substrate inventory: every offering slot as Speculative,
// upgraded to Idle (with its host) when a managed instance already backs it,
// plus any orphan managed instances. The kit calls this to seed a fresh store;
// the persisted store is the primary restart path.
//
// A machine-id-labelled managed instance owns its slot while it is alive,
// keeping the slot from being re-seeded Speculative so Create can't launch a
// duplicate under the same machine id. A terminated instance is releasing its
// slot and is correctly absent (the slot returns to Speculative for
// re-provisioning). Unlabelled-but-running managed instances are surfaced as
// orphans under their own id so they are not lost.
func (b *gcpBackend) Describe(ctx context.Context) ([]providerkit.Instance, error) {
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe managed instances: %w", err)
	}
	bySlot := make(map[string]gceInstance, len(managed))
	var orphans []gceInstance
	for _, inst := range managed {
		// Only a live instance is a reachable host; a STOPPING/TERMINATED/
		// SUSPENDED managed instance is releasing its slot, so skip it and let the
		// slot return to Speculative (an Idle machine must carry a reachable host).
		if !inst.Running {
			continue
		}
		switch {
		case inst.MachineID != "":
			bySlot[inst.MachineID] = inst // a running, machine-id-labelled instance owns its slot
		default:
			orphans = append(orphans, inst) // managed + running, but unlabelled
		}
	}

	slots := b.speculativeSlots()
	out := make([]providerkit.Instance, 0, len(slots)+len(bySlot)+len(orphans))
	for _, slot := range slots {
		if inst, ok := bySlot[slot.ID]; ok {
			slot.State = providerkit.StateIdle
			slot.Host = providerkit.HostRef{Provider: b.providerName, Ref: hostRef(inst)}
			delete(bySlot, slot.ID)
		}
		out = append(out, slot)
	}
	// Labelled instances matching no current offering slot (offering shrank, or a
	// manually labelled instance), then unlabelled-but-running managed instances.
	for id, inst := range bySlot {
		out = append(out, b.instanceToIdle(id, inst))
	}
	for _, inst := range orphans {
		out = append(out, b.instanceToIdle(inst.MachineID, inst))
	}
	return out, nil
}

func (b *gcpBackend) instanceToIdle(machineID string, inst gceInstance) providerkit.Instance {
	if machineID == "" {
		machineID = hostRef(inst)
	}
	// Prefer the capacity recorded at Insert (bigfleet-capacity label); fall back
	// to the provisioning model for instances launched without the label.
	// Defaulting a Reserved instance to ON_DEMAND would make the shard's
	// idle-release path eligible to Delete capacity it should hold differently.
	capacity := parseCapacityLabel(inst.Capacity)
	if capacity == providerkit.CapacityUnspecified {
		capacity = providerkit.CapacityOnDemand
		if inst.Spot {
			capacity = providerkit.CapacitySpot
		}
	}
	return providerkit.Instance{
		ID:                      machineID,
		State:                   providerkit.StateIdle,
		Host:                    providerkit.HostRef{Provider: b.providerName, Ref: hostRef(inst)},
		InstanceType:            inst.MachineType,
		Zone:                    inst.Zone,
		CapacityType:            capacity,
		PricePerHour:            b.pricing.price(inst.MachineType, capacity),
		InterruptionProbability: b.interruption.probability(machineID, inst.MachineType, capacity),
		// Recover the per-replica request shape from a still-configured offering
		// for this type, so an orphan / offering-shrank machine that re-binds via
		// Describe still matches its demand profile. Nil only for a truly unknown
		// type, where the FileStore (the primary restart path) restores resources.
		Resources:   b.resourcesForType(inst.MachineType, inst.Zone),
		Allocatable: b.machineTypes.allocatable(inst.MachineType),
	}
}

// resourcesForType returns the per-replica resources of an offering matching the
// given machine type, preferring an exact (type, zone) match and falling back to
// the same type in any zone. Nil when no offering covers the type.
func (b *gcpBackend) resourcesForType(machineType, zone string) map[string]string {
	var fallback map[string]string
	for _, off := range b.offerings {
		if off.MachineType != machineType {
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

// CreateInstance launches the GCE instance for a Speculative slot and returns
// its host. The cluster-specific bootstrap is delivered later by
// ConfigureInstance, because the startup-script is consumed at boot.
func (b *gcpBackend) CreateInstance(ctx context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	m := req.Machine
	inst, err := b.client.Insert(ctx, instanceSpec{
		MachineID:         m.ID,
		MachineType:       m.InstanceType,
		Zone:              m.Zone,
		Spot:              m.CapacityType == providerkit.CapacitySpot,
		Capacity:          capacityString(m.CapacityType),
		IdempotencyToken:  req.OperationID,
		BaseStartupScript: b.baseStartupScript,
	})
	if err != nil {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("insert instance %s: %w", m.InstanceType, err)
	}
	// The kit's empty-host guard only fires when BOTH HostRef fields are empty,
	// and Provider is always set here — so guard the instance ref explicitly. A
	// host with an empty Ref would settle the machine Idle, then break every
	// later Configure/Drain/Delete.
	if inst.Name == "" {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("insert instance %s returned no name", m.InstanceType)
	}
	return providerkit.CreateInstanceResult{
		Host:        providerkit.HostRef{Provider: b.providerName, Ref: hostRef(inst)},
		Allocatable: b.machineTypes.allocatable(m.InstanceType),
	}, nil
}

// ConfigureInstance binds the running instance to a cluster and delivers the
// opaque bootstrap blob (real impl: SetMetadata startup-script + Reset).
func (b *gcpBackend) ConfigureInstance(ctx context.Context, req providerkit.ConfigureInstanceRequest) error {
	inst, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	return b.client.ApplyBootstrap(ctx, inst, req.ClusterID, req.BootstrapBlob)
}

// DrainInstance strips the cluster binding (removes the delivered startup-script
// metadata + clears the binding label), leaving the instance running but unbound
// (Idle).
func (b *gcpBackend) DrainInstance(ctx context.Context, req providerkit.DrainInstanceRequest) error {
	inst, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	return b.client.DrainNode(ctx, inst, req.GracePeriodSeconds)
}

// DeleteInstance deletes the GCE instance; the slot returns to Speculative.
func (b *gcpBackend) DeleteInstance(ctx context.Context, req providerkit.DeleteInstanceRequest) error {
	zone, name, err := parseHostRef(req.Machine.Host.Ref)
	if err != nil {
		return fmt.Errorf("delete: machine %s: %w", req.Machine.ID, err)
	}
	if err := b.client.DeleteInstance(ctx, zone, name); err != nil {
		return err
	}
	// Only drop the observed interruption escalation once the delete actuated.
	b.interruption.clear(req.Machine.ID)
	return nil
}

// resolveHost recovers the substrate instance view for a machine the kit holds,
// by its host ref. It looks the instance up in DescribeManaged (so the real
// client has the live metadata fingerprint it needs for SetMetadata), falling
// back to a minimal view parsed from the ref when a transient describe missed it.
func (b *gcpBackend) resolveHost(ctx context.Context, m providerkit.Machine) (gceInstance, error) {
	zone, name, err := parseHostRef(m.Host.Ref)
	if err != nil {
		return gceInstance{}, fmt.Errorf("machine %s: %w", m.ID, err)
	}
	managed, derr := b.client.DescribeManaged(ctx)
	if derr == nil {
		for _, inst := range managed {
			if inst.Zone == zone && inst.Name == name {
				return inst, nil
			}
		}
	}
	// Minimal fallback view; the real client can still address the instance by
	// (zone, name) even if a transient DescribeManaged missed it.
	return gceInstance{Name: name, Zone: zone, MachineType: m.InstanceType}, nil
}

// observePreemptions scans managed instances for SPOT VMs that GCE has preempted
// (a SPOT instance in TERMINATED status — the provider only ever Deletes, never
// stops, so a stopped spot VM is a preemption) and raises their observed
// interruption probability. This is the "observed" half of the field-shape
// contract: once a slot has a real preemption in its history, it publishes an
// elevated interruption_probability on its next Speculative/Idle description,
// above the bare per-family forecast. Call it on a timer (the reconcile loop).
// Returns the number of preemptions observed this pass.
func (b *gcpBackend) observePreemptions(ctx context.Context) (int, error) {
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return 0, fmt.Errorf("observe preemptions: %w", err)
	}
	n := 0
	for _, inst := range managed {
		if inst.Preempted && inst.MachineID != "" {
			b.interruption.markPreempted(inst.MachineID, observedPreemptionProbability)
			n++
		}
	}
	return n, nil
}

// refreshMachineTypes warms the allocatable cache from the GCE MachineTypes API
// for the offered types. Call once at startup (machine-type specs are
// immutable). Returns the number of offered types it could not resolve (each
// still covered by the pinned table if present).
func (b *gcpBackend) refreshMachineTypes(ctx context.Context) int {
	return b.machineTypes.resolve(ctx, b.offeredRefs())
}

// offeredRefs returns the distinct (machine type, zone) refs across offerings.
func (b *gcpBackend) offeredRefs() []machineTypeRef {
	out := make([]machineTypeRef, 0, len(b.offerings))
	for _, off := range b.offerings {
		out = append(out, machineTypeRef{MachineType: off.MachineType, Zone: off.Zone})
	}
	return out
}

// hostRef renders the stable host reference for an instance: "<zone>/<name>".
// Stable for the life of the instance and parseable back into (zone, name).
func hostRef(inst gceInstance) string {
	return inst.Zone + "/" + inst.Name
}

// parseHostRef splits a "<zone>/<name>" host ref back into its parts.
func parseHostRef(ref string) (zone, name string, err error) {
	z, n, ok := strings.Cut(ref, "/")
	if !ok || z == "" || n == "" {
		return "", "", fmt.Errorf("invalid host ref %q (want <zone>/<name>)", ref)
	}
	return z, n, nil
}

// capacityString renders a kit CapacityType as the canonical label string.
func capacityString(c providerkit.CapacityType) string {
	switch c {
	case providerkit.CapacitySpot:
		return "spot"
	case providerkit.CapacityReserved:
		return "reserved"
	case providerkit.CapacityBareMetal:
		return "bare_metal"
	case providerkit.CapacityOnDemand:
		return "on_demand"
	default:
		return ""
	}
}

// parseCapacityLabel maps a bigfleet-capacity label value back to a kit
// CapacityType; an empty/unknown label yields CapacityUnspecified so the caller
// can fall back to the provisioning model.
func parseCapacityLabel(s string) providerkit.CapacityType {
	switch s {
	case "spot":
		return providerkit.CapacitySpot
	case "reserved":
		return providerkit.CapacityReserved
	case "bare_metal":
		return providerkit.CapacityBareMetal
	case "on_demand":
		return providerkit.CapacityOnDemand
	default:
		return providerkit.CapacityUnspecified
	}
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
	_ providerkit.Backend = (*gcpBackend)(nil)
	_ providerkit.Deleter = (*gcpBackend)(nil)
)
