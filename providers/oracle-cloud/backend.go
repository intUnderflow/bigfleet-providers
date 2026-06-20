package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// ociBackend is the Oracle Cloud Infrastructure (OCI) Compute implementation of
// providerkit.Backend (+ Deleter). It is pure substrate logic: it maps the kit's
// lifecycle calls onto OCI Compute API calls and populates the substrate fields
// it knows (instance_type/shape, zone/availability-domain, capacity_type,
// price_per_hour, interruption_probability, resources, allocatable, host).
// Fencing, idempotency, async dispatch, transition timeouts, shard_metadata, and
// the rest are providerkit's job — this file never touches them.
//
// Configure-bootstrap reconciliation: cloud-init user_data is consumed only at
// first boot, so LaunchInstance bakes in the generic pre-binding --base-user-data,
// and the cluster-specific bootstrap blob is delivered later by ConfigureInstance
// over the Oracle Cloud Agent Run Command (the real client's ApplyBootstrap).
// This keeps the kit's invariant that an Idle machine already carries a real,
// reachable host, and delivers the blob exactly once when the binding is
// established.
type ociBackend struct {
	providerName string // HostRef.provider label, e.g. "oci-eu-frankfurt-1"
	client       ociClient
	offerings    []offering
	pricing      *pricing
	interruption *interruption
	baseUserData []byte // generic pre-binding cloud-init baked in at Launch
	logger       *slog.Logger
}

func newOCIBackend(providerName string, client ociClient, offerings []offering, pr *pricing, in *interruption, baseUserData []byte, logger *slog.Logger) (*ociBackend, error) {
	if len(offerings) == 0 {
		return nil, fmt.Errorf("oci backend: no offerings configured")
	}
	for _, off := range offerings {
		if off.Shape == "" {
			return nil, fmt.Errorf("oci backend: offering with empty shape")
		}
		if _, err := off.capacityType(); err != nil {
			return nil, fmt.Errorf("oci backend: offering %s/%s: %w", off.Shape, off.AvailabilityDomain, err)
		}
		// The provider registers multi-AD (RequireZone), so an AD-less offering
		// would only fail later at seed time — reject it up front.
		if off.AvailabilityDomain == "" {
			return nil, fmt.Errorf("oci backend: offering %s with empty availability_domain", off.Shape)
		}
		// A flexible shape needs OCPU/memory to size the launch and allocatable.
		if isFlexShape(off.Shape) && (off.OCPUs <= 0 || off.MemoryGB <= 0) {
			return nil, fmt.Errorf("oci backend: flexible shape %s needs ocpus and memory_gb", off.Shape)
		}
	}
	return &ociBackend{
		providerName: providerName,
		client:       client,
		offerings:    offerings,
		pricing:      pr,
		interruption: in,
		baseUserData: baseUserData,
		logger:       logger,
	}, nil
}

// slotID is the stable BigFleet machine id for one offering slot. A Speculative
// slot keeps this id across its whole lifecycle (launched instances are tagged
// with it, so DescribeManaged maps back to it).
func slotID(providerName string, capacity providerkit.CapacityType, off offering, i int) string {
	return fmt.Sprintf("%s/%s/%s/%s/%03d", providerName, capacity, off.Shape, off.AvailabilityDomain, i)
}

// speculativeSlots renders the configured offerings as Speculative Instance
// records with full, validated field shape. Reads only pinned pricing /
// interruption / shape state, so it never blocks on the network.
func (b *ociBackend) speculativeSlots() []providerkit.Instance {
	var out []providerkit.Instance
	for _, off := range b.offerings {
		capacity, _ := off.capacityType() // validated in newOCIBackend
		for i := 0; i < off.Count; i++ {
			id := slotID(b.providerName, capacity, off, i)
			out = append(out, providerkit.Instance{
				ID:                      id,
				State:                   providerkit.StateSpeculative,
				InstanceType:            off.Shape,
				Zone:                    off.AvailabilityDomain,
				CapacityType:            capacity,
				PricePerHour:            b.pricing.price(off.Shape, off.OCPUs, off.MemoryGB, capacity),
				InterruptionProbability: b.interruption.probability(id, off.Shape, capacity),
				Resources:               cloneMap(off.Resources),
				Allocatable:             allocatable(off.Shape, off.OCPUs, off.MemoryGB),
				Labels:                  shapeLabels(off),
			})
		}
	}
	return out
}

// Describe returns the substrate inventory: every offering slot as Speculative,
// upgraded to Idle (with its host) when a managed instance already backs it, plus
// any orphan managed instances. The kit calls this to seed a fresh store; the
// persisted store is the primary restart path.
//
// A machine-id-tagged managed instance owns its slot while it is alive, keeping
// the slot from being re-seeded Speculative so Create can't launch a duplicate
// under the same machine id. A terminated instance is releasing its slot and is
// correctly absent (the slot returns to Speculative for re-provisioning).
// Untagged-but-running managed instances are surfaced as orphans under their
// instance OCID so they are not lost.
func (b *ociBackend) Describe(ctx context.Context) ([]providerkit.Instance, error) {
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe managed instances: %w", err)
	}
	bySlot := make(map[string]ociInstance, len(managed))
	var orphans []ociInstance
	for _, inst := range managed {
		switch {
		case inst.MachineID != "":
			bySlot[inst.MachineID] = inst // owns its slot, running or not
		case inst.Running:
			orphans = append(orphans, inst) // managed + running, but untagged
		}
	}

	slots := b.speculativeSlots()
	out := make([]providerkit.Instance, 0, len(slots)+len(bySlot)+len(orphans))
	for _, slot := range slots {
		if inst, ok := bySlot[slot.ID]; ok {
			slot.State = providerkit.StateIdle
			slot.Host = providerkit.HostRef{Provider: b.providerName, Ref: inst.InstanceID}
			delete(bySlot, slot.ID)
		}
		out = append(out, slot)
	}
	// Tagged instances matching no current offering slot (offering shrank, or a
	// manually tagged instance), then untagged-but-running managed instances.
	for id, inst := range bySlot {
		out = append(out, b.instanceToIdle(id, inst))
	}
	for _, inst := range orphans {
		out = append(out, b.instanceToIdle(inst.InstanceID, inst))
	}
	return out, nil
}

func (b *ociBackend) instanceToIdle(machineID string, inst ociInstance) providerkit.Instance {
	capacity := providerkit.CapacityOnDemand
	switch {
	case isBareMetalShape(inst.Shape):
		capacity = providerkit.CapacityBareMetal
	case inst.Preemptible:
		capacity = providerkit.CapacitySpot
	}
	return providerkit.Instance{
		ID:                      machineID,
		State:                   providerkit.StateIdle,
		Host:                    providerkit.HostRef{Provider: b.providerName, Ref: inst.InstanceID},
		InstanceType:            inst.Shape,
		Zone:                    inst.AvailabilityDomain,
		CapacityType:            capacity,
		PricePerHour:            b.pricing.price(inst.Shape, inst.OCPUs, inst.MemoryGB, capacity),
		InterruptionProbability: b.interruption.probability(machineID, inst.Shape, capacity),
		// Recover the per-replica request shape from a still-configured offering
		// for this shape, so an orphan / offering-shrank machine that re-binds via
		// Describe still matches its demand profile. Nil only for a truly unknown
		// shape, where the FileStore (the primary restart path) restores resources.
		Resources:   b.resourcesForShape(inst.Shape, inst.AvailabilityDomain),
		Allocatable: allocatable(inst.Shape, inst.OCPUs, inst.MemoryGB),
	}
}

// resourcesForShape returns the per-replica resources of an offering matching the
// given shape, preferring an exact (shape, AD) match and falling back to the same
// shape in any AD. Nil when no offering covers the shape.
func (b *ociBackend) resourcesForShape(shape, ad string) map[string]string {
	var fallback map[string]string
	for _, off := range b.offerings {
		if off.Shape != shape {
			continue
		}
		if off.AvailabilityDomain == ad {
			return cloneMap(off.Resources)
		}
		if fallback == nil {
			fallback = off.Resources
		}
	}
	return cloneMap(fallback)
}

// CreateInstance launches the OCI instance for a Speculative slot and returns its
// host. The cluster-specific bootstrap is delivered later by ConfigureInstance,
// because cloud-init user_data is consumed only at first boot.
func (b *ociBackend) CreateInstance(ctx context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	m := req.Machine
	ocpus, memGiB := b.sizingFor(m.InstanceType, m.Zone, m.CapacityType)
	inst, err := b.client.LaunchInstance(ctx, launchSpec{
		MachineID:          m.ID,
		Shape:              m.InstanceType,
		AvailabilityDomain: m.Zone,
		OCPUs:              ocpus,
		MemoryGB:           memGiB,
		Preemptible:        m.CapacityType == providerkit.CapacitySpot,
		IdempotencyToken:   req.OperationID,
		BaseUserData:       b.baseUserData,
	})
	if err != nil {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("launch instance %s: %w", m.InstanceType, err)
	}
	// The kit's empty-host guard only fires when BOTH HostRef fields are empty,
	// and Provider is always set here — so guard the instance OCID explicitly. A
	// host with an empty Ref would settle the machine Idle, then break every later
	// Configure/Drain/Delete.
	if inst.InstanceID == "" {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("launch instance %s returned no OCID", m.InstanceType)
	}
	return providerkit.CreateInstanceResult{
		Host:        providerkit.HostRef{Provider: b.providerName, Ref: inst.InstanceID},
		Allocatable: allocatable(m.InstanceType, ocpus, memGiB),
	}, nil
}

// ConfigureInstance binds the running instance to a cluster and delivers the
// opaque bootstrap blob (real impl: Oracle Cloud Agent Run Command).
func (b *ociBackend) ConfigureInstance(ctx context.Context, req providerkit.ConfigureInstanceRequest) error {
	inst, err := b.resolveHost(req.Machine)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	return b.client.ApplyBootstrap(ctx, inst, req.ClusterID, req.BootstrapBlob)
}

// DrainInstance cordons + drains the kubelet and removes the cluster binding,
// leaving the instance running but unbound (Idle).
func (b *ociBackend) DrainInstance(ctx context.Context, req providerkit.DrainInstanceRequest) error {
	inst, err := b.resolveHost(req.Machine)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	return b.client.DrainNode(ctx, inst, req.GracePeriodSeconds)
}

// DeleteInstance terminates the OCI instance; the slot returns to Speculative.
func (b *ociBackend) DeleteInstance(ctx context.Context, req providerkit.DeleteInstanceRequest) error {
	if req.Machine.Host.Ref == "" {
		return fmt.Errorf("delete: machine %s has no host", req.Machine.ID)
	}
	if err := b.client.TerminateInstance(ctx, req.Machine.Host.Ref); err != nil {
		return err
	}
	// Only drop the observed interruption escalation once the terminate actuated.
	b.interruption.clear(req.Machine.ID)
	return nil
}

// resolveHost builds the substrate instance view needed to actuate Configure /
// Drain on a machine the kit holds. Both the real client (Run Command targets the
// instance by its OCID) and the fake address the instance purely by OCID, so this
// is built directly from the machine's HostRef — no ListInstances call, keeping
// Configure/Drain O(1) rather than O(fleet size) and not amplifying OCI API load.
func (b *ociBackend) resolveHost(m providerkit.Machine) (ociInstance, error) {
	if m.Host.Ref == "" {
		return ociInstance{}, fmt.Errorf("machine %s has no host", m.ID)
	}
	return ociInstance{InstanceID: m.Host.Ref, Shape: m.InstanceType, AvailabilityDomain: m.Zone}, nil
}

// sizingFor returns the (OCPUs, MemoryGB) to launch a flexible-shape machine
// with, taken from the originating offering. It matches on the full offering key
// — (shape, availability domain, capacity type) — so two offerings that declare
// the same .Flex shape at different sizes (e.g. a 2-OCPU on-demand lane and an
// 8-OCPU spot lane, or different sizes per AD) each launch with their own
// ShapeConfig rather than silently inheriting the first one's. Falls back to any
// same-shape offering if no exact match is found, and returns 0/0 for fixed
// shapes (which pin their own OCPU/memory and ignore the ShapeConfig).
func (b *ociBackend) sizingFor(shape, zone string, capacity providerkit.CapacityType) (float64, float64) {
	if !isFlexShape(shape) {
		return 0, 0
	}
	var fbOCPUs, fbMemGiB float64
	for _, off := range b.offerings {
		if off.Shape != shape || off.OCPUs <= 0 || off.MemoryGB <= 0 {
			continue
		}
		oc, _ := off.capacityType()
		if off.AvailabilityDomain == zone && oc == capacity {
			return off.OCPUs, off.MemoryGB
		}
		if fbOCPUs == 0 {
			fbOCPUs, fbMemGiB = off.OCPUs, off.MemoryGB
		}
	}
	return fbOCPUs, fbMemGiB
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
	_ providerkit.Backend = (*ociBackend)(nil)
	_ providerkit.Deleter = (*ociBackend)(nil)
)
