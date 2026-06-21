package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// azureBackend is the Azure implementation of providerkit.Backend (+ Deleter).
// It is pure substrate logic: it maps the kit's lifecycle calls onto Azure API
// calls and populates the substrate fields it knows (instance_type/vm_size,
// zone, capacity_type, price_per_hour, interruption_probability, resources,
// allocatable, host). Fencing, idempotency, async dispatch, transition
// timeouts, shard_metadata, and the rest are providerkit's job — this file
// never touches them.
//
// Configure-bootstrap reconciliation: Azure customData (cloud-init) is consumed
// only at first boot, so CreateVM provisions the VM with the generic pre-binding
// --base-user-data, and the cluster-specific bootstrap blob is delivered later
// by ConfigureInstance via a CustomScript VM extension (the real client's
// ApplyBootstrap). This keeps the kit's invariant that an Idle machine already
// carries a real, reachable host, and delivers the blob exactly once when the
// binding is established.
type azureBackend struct {
	providerName string // HostRef.provider label, e.g. "azure-eastus"
	location     string
	client       azureClient
	offerings    []offering
	pricing      *pricing
	interruption *interruption
	vmSizes      *vmSizeResolver // resolves Machine.allocatable
	baseUserData []byte          // generic pre-binding bootstrap baked in at Create
	logger       *slog.Logger
}

func newAzureBackend(providerName, location string, client azureClient, offerings []offering, pr *pricing, in *interruption, baseUserData []byte, logger *slog.Logger) (*azureBackend, error) {
	if len(offerings) == 0 {
		return nil, fmt.Errorf("azure backend: no offerings configured")
	}
	for _, off := range offerings {
		if _, err := off.capacityType(); err != nil {
			return nil, fmt.Errorf("azure backend: offering %s/%s: %w", off.VMSize, off.Zone, err)
		}
		if off.VMSize == "" {
			return nil, fmt.Errorf("azure backend: offering with empty vm_size")
		}
		// The provider registers multi-zone (RequireZone), so a zoneless offering
		// would only fail later at seed time — reject it up front.
		if off.Zone == "" {
			return nil, fmt.Errorf("azure backend: offering %s with empty zone", off.VMSize)
		}
	}
	return &azureBackend{
		providerName: providerName,
		location:     location,
		client:       client,
		offerings:    offerings,
		pricing:      pr,
		interruption: in,
		vmSizes:      newVMSizeResolver(client, logger),
		baseUserData: baseUserData,
		logger:       logger,
	}, nil
}

// slotID is the stable BigFleet machine id for one offering slot. A Speculative
// slot keeps this id across its whole lifecycle (Created VMs are tagged with it,
// so DescribeManaged maps back to it).
func slotID(providerName string, capacity providerkit.CapacityType, off offering, i int) string {
	return fmt.Sprintf("%s/%s/%s/%s/%03d", providerName, capacity, off.VMSize, off.Zone, i)
}

// speculativeSlots renders the configured offerings as Speculative Instance
// records with full, validated field shape. Reads only cached pricing /
// interruption state, so it never blocks on the network.
func (b *azureBackend) speculativeSlots() []providerkit.Instance {
	var out []providerkit.Instance
	for _, off := range b.offerings {
		capacity, _ := off.capacityType() // validated in newAzureBackend
		for i := 0; i < off.Count; i++ {
			id := slotID(b.providerName, capacity, off, i)
			out = append(out, providerkit.Instance{
				ID:                      id,
				State:                   providerkit.StateSpeculative,
				InstanceType:            off.VMSize,
				Zone:                    off.Zone,
				CapacityType:            capacity,
				PricePerHour:            b.pricing.price(off.VMSize, capacity),
				InterruptionProbability: b.interruption.probability(id, off.VMSize, capacity),
				Resources:               cloneMap(off.Resources),
				Allocatable:             b.vmSizes.allocatable(off.VMSize),
				Labels:                  slotLabels(off),
			})
		}
	}
	return out
}

func slotLabels(off offering) map[string]string {
	labels := cloneMap(off.Labels)
	if acc, ok := acceleratorLabel(off.VMSize); ok {
		if labels == nil {
			labels = map[string]string{}
		}
		labels["bigfleet.io/accelerator"] = acc
	}
	return labels
}

// Describe returns the substrate inventory: every offering slot as Speculative,
// upgraded to Idle (with its host) when a managed VM already backs it, plus any
// orphan managed VMs. The kit calls this to seed a fresh store; the persisted
// store is the primary restart path.
//
// A machine-id-tagged managed VM owns its slot while it is alive, keeping the
// slot from being re-seeded Speculative so Create can't provision a duplicate
// under the same machine id. A deleting/evicted VM is releasing its slot and is
// correctly absent (the slot returns to Speculative for re-provisioning).
// Untagged-but-running managed VMs are surfaced as orphans under their resource
// id so they are not lost.
func (b *azureBackend) Describe(ctx context.Context) ([]providerkit.Instance, error) {
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe managed vms: %w", err)
	}
	bySlot := make(map[string]vmInstance, len(managed))
	var orphans []vmInstance
	byMachine := make(map[string][]vmInstance)
	for _, vm := range managed {
		switch {
		case vm.MachineID != "" && vm.Running:
			byMachine[vm.MachineID] = append(byMachine[vm.MachineID], vm)
		case vm.MachineID != "":
			// Tagged but deleting/evicted: releasing its slot. Drop it so the slot
			// returns to Speculative rather than seeding an Idle machine whose host
			// ref points at a vanishing resource (which would fail later
			// Configure/Drain/Delete).
		case vm.Running:
			orphans = append(orphans, vm) // managed + running, but untagged
		}
	}
	for mid, vms := range byMachine {
		// Normally one VM per machine id. A re-driven Create after a partial failure
		// can leave two running VMs sharing a machine id; pick a deterministic owner
		// (smallest resource id) and surface the rest as orphans so they are tracked
		// and reclaimable, never silently leaked.
		owner := vms[0]
		for _, vm := range vms[1:] {
			if vm.ResourceID < owner.ResourceID {
				owner = vm
			}
		}
		bySlot[mid] = owner
		for _, vm := range vms {
			if vm.ResourceID != owner.ResourceID {
				orphans = append(orphans, vm)
			}
		}
	}

	slots := b.speculativeSlots()
	out := make([]providerkit.Instance, 0, len(slots)+len(bySlot)+len(orphans))
	for _, slot := range slots {
		if vm, ok := bySlot[slot.ID]; ok {
			slot.State = providerkit.StateIdle
			slot.Host = providerkit.HostRef{Provider: b.providerName, Ref: vm.ResourceID}
			delete(bySlot, slot.ID)
		}
		out = append(out, slot)
	}
	// Tagged VMs matching no current offering slot (offering shrank, or a manually
	// tagged VM), then untagged-but-running managed VMs.
	for id, vm := range bySlot {
		out = append(out, b.vmToIdle(id, vm))
	}
	for _, vm := range orphans {
		out = append(out, b.vmToIdle(vm.ResourceID, vm))
	}
	return out, nil
}

func (b *azureBackend) vmToIdle(machineID string, vm vmInstance) providerkit.Instance {
	// Prefer the capacity recorded at Create (bigfleet-capacity tag); fall back to
	// the Spot priority for VMs created without the tag. Defaulting a
	// Reserved-backed VM to ON_DEMAND would only skew cost ranking, not idle-hold;
	// preserving the tag keeps both honest.
	capacity := parseCapacityTag(vm.Capacity)
	if capacity == providerkit.CapacityUnspecified {
		capacity = providerkit.CapacityOnDemand
		if vm.Spot {
			capacity = providerkit.CapacitySpot
		}
	}
	return providerkit.Instance{
		ID:                      machineID,
		State:                   providerkit.StateIdle,
		Host:                    providerkit.HostRef{Provider: b.providerName, Ref: vm.ResourceID},
		InstanceType:            vm.VMSize,
		Zone:                    vm.Zone,
		CapacityType:            capacity,
		PricePerHour:            b.pricing.price(vm.VMSize, capacity),
		InterruptionProbability: b.interruption.probability(machineID, vm.VMSize, capacity),
		// Recover the per-replica request shape from a still-configured offering
		// for this size, so an orphan / offering-shrank machine that re-binds via
		// Describe still matches its demand profile. Nil only for a truly unknown
		// size, where the FileStore (the primary restart path) restores resources.
		Resources:   b.resourcesForSize(vm.VMSize, vm.Zone),
		Allocatable: b.vmSizes.allocatable(vm.VMSize),
	}
}

// resourcesForSize returns the per-replica resources of an offering matching the
// given VM size, preferring an exact (size, zone) match and falling back to the
// same size in any zone. Nil when no offering covers the size.
func (b *azureBackend) resourcesForSize(vmSize, zone string) map[string]string {
	var fallback map[string]string
	for _, off := range b.offerings {
		if off.VMSize != vmSize {
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

// CreateInstance provisions the Azure VM for a Speculative slot and returns its
// host. The cluster-specific bootstrap is delivered later by ConfigureInstance,
// because customData (cloud-init) is consumed only at first boot.
func (b *azureBackend) CreateInstance(ctx context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	m := req.Machine
	vm, err := b.client.CreateVM(ctx, vmSpec{
		MachineID:        m.ID,
		VMSize:           m.InstanceType,
		Zone:             m.Zone,
		Spot:             m.CapacityType == providerkit.CapacitySpot,
		Capacity:         capacityString(m.CapacityType),
		IdempotencyToken: req.OperationID,
		BaseUserData:     b.baseUserData,
	})
	if err != nil {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("create vm %s: %w", m.InstanceType, err)
	}
	// The kit's empty-host guard only fires when BOTH HostRef fields are empty,
	// and Provider is always set here — so guard the resource id explicitly. A
	// host with an empty Ref would settle the machine Idle, then break every later
	// Configure/Drain/Delete.
	if vm.ResourceID == "" {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("create vm %s returned no resource id", m.InstanceType)
	}
	return providerkit.CreateInstanceResult{
		Host:        providerkit.HostRef{Provider: b.providerName, Ref: vm.ResourceID},
		Allocatable: b.vmSizes.allocatable(m.InstanceType),
	}, nil
}

// ConfigureInstance binds the running VM to a cluster and delivers the opaque
// bootstrap blob (real impl: a CustomScript VM extension).
func (b *azureBackend) ConfigureInstance(ctx context.Context, req providerkit.ConfigureInstanceRequest) error {
	vm, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	return b.client.ApplyBootstrap(ctx, vm, req.ClusterID, req.BootstrapBlob)
}

// DrainInstance cordons + drains the kubelet and removes the cluster binding,
// leaving the VM running but unbound (Idle).
func (b *azureBackend) DrainInstance(ctx context.Context, req providerkit.DrainInstanceRequest) error {
	vm, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	return b.client.DrainNode(ctx, vm, req.GracePeriodSeconds)
}

// DeleteInstance deletes the Azure VM; the slot returns to Speculative.
func (b *azureBackend) DeleteInstance(ctx context.Context, req providerkit.DeleteInstanceRequest) error {
	if req.Machine.Host.Ref == "" {
		return fmt.Errorf("delete: machine %s has no host", req.Machine.ID)
	}
	if err := b.client.DeleteVM(ctx, req.Machine.Host.Ref); err != nil {
		return err
	}
	// Only drop the observed interruption escalation once the delete actuated.
	b.interruption.clear(req.Machine.ID)
	return nil
}

// resolveHost builds the substrate VM view the extension client needs to address
// a machine the kit holds, from its host resource id. Configure/Drain run their
// CustomScript extensions and tag updates by resource name, so only the resource
// id is required — we deliberately avoid a DescribeManaged here, which would turn
// every lifecycle transition into an O(N) list of the resource group (latency +
// ARM throttling at scale). DeleteInstance already addresses the VM directly the
// same way.
func (b *azureBackend) resolveHost(_ context.Context, m providerkit.Machine) (vmInstance, error) {
	if m.Host.Ref == "" {
		return vmInstance{}, fmt.Errorf("machine %s has no host", m.ID)
	}
	return vmInstance{ResourceID: m.Host.Ref, VMSize: m.InstanceType, Zone: m.Zone}, nil
}

// refreshPrices warms / refreshes the Spot price cache. Call at startup and on a
// timer. Returns the number of sizes that failed to refresh.
func (b *azureBackend) refreshPrices(ctx context.Context) int {
	return b.pricing.refresh(ctx, b.spotSizes())
}

// spotSizes lists the distinct VM sizes of SPOT offerings, to drive
// pricing.refresh without touching the List hot path.
func (b *azureBackend) spotSizes() []string {
	var out []string
	for _, off := range b.offerings {
		capacity, _ := off.capacityType()
		if capacity == providerkit.CapacitySpot {
			out = append(out, off.VMSize)
		}
	}
	return out
}

// refreshVMSizes warms the allocatable cache from the Resource SKUs API for the
// offered sizes. Call once at startup (VM size specs are immutable). Returns the
// number of offered sizes it could not resolve (each still covered by the pinned
// table if present).
func (b *azureBackend) refreshVMSizes(ctx context.Context) int {
	return b.vmSizes.resolve(ctx, b.offeredSizes())
}

// offeredSizes returns the distinct VM sizes across the configured offerings.
func (b *azureBackend) offeredSizes() []string {
	out := make([]string, 0, len(b.offerings))
	for _, off := range b.offerings {
		out = append(out, off.VMSize)
	}
	return out
}

// capacityString renders a kit CapacityType as the canonical tag string.
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

// parseCapacityTag maps a bigfleet-capacity tag value back to a kit
// CapacityType; an empty/unknown tag yields CapacityUnspecified so the caller can
// fall back to the Spot priority.
func parseCapacityTag(s string) providerkit.CapacityType {
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
	_ providerkit.Backend = (*azureBackend)(nil)
	_ providerkit.Deleter = (*azureBackend)(nil)
)
