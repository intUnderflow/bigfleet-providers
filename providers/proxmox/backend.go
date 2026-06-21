package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// proxmoxBackend is the Proxmox VE implementation of providerkit.Backend (+
// Deleter). It is pure substrate logic: it maps the kit's lifecycle calls onto
// Proxmox VM operations (clone / start / stop / destroy + guest-agent
// file-write/exec) and populates the substrate fields it knows (instance_type,
// zone, capacity_type, price_per_hour, interruption_probability, resources,
// allocatable, host). Fencing, idempotency, async dispatch, transition
// timeouts, shard_metadata, and the rest are providerkit's job — this file never
// touches them.
//
// Configure-bootstrap reconciliation: the cluster-JOIN SECRET in bootstrap_blob
// is delivered later by ConfigureInstance over the qemu guest agent (through the
// TLS-verified, token-authenticated Proxmox API), never via cloud-init (which is
// first-boot-only and not a confidential per-Configure channel). CreateInstance
// clones a generic, cluster-agnostic template; the cluster-specific bootstrap
// arrives only when the binding is established. This keeps the kit's invariant
// that an Idle machine already carries a real, reachable host, and delivers the
// blob exactly once per Configure.
type proxmoxBackend struct {
	providerName string // HostRef.provider label, e.g. "proxmox-dc1"
	client       proxmoxClient
	offerings    []offering
	catalog      *instanceCatalog
	pricing      *pricing
	logger       *slog.Logger
}

func newProxmoxBackend(providerName string, client proxmoxClient, offerings []offering, catalog *instanceCatalog, pr *pricing, logger *slog.Logger) (*proxmoxBackend, error) {
	if len(offerings) == 0 {
		return nil, fmt.Errorf("proxmox backend: no offerings configured")
	}
	for _, off := range offerings {
		if _, err := off.capacityType(); err != nil {
			return nil, fmt.Errorf("proxmox backend: offering %s/%s: %w", off.InstanceType, off.Zone, err)
		}
		if off.InstanceType == "" {
			return nil, fmt.Errorf("proxmox backend: offering with empty instance_type")
		}
		if !catalog.has(off.InstanceType) {
			return nil, fmt.Errorf("proxmox backend: offering instance_type %q is not in the instance-type catalog", off.InstanceType)
		}
		// The provider registers multi-zone (RequireZone), so a zoneless offering
		// would only fail later at seed time — reject it up front. zone == the
		// Proxmox node the clone lands on.
		if off.Zone == "" {
			return nil, fmt.Errorf("proxmox backend: offering %s with empty zone (Proxmox node)", off.InstanceType)
		}
		// A non-positive count yields no slots; reject it so the provider never
		// silently starts with an effectively empty quota.
		if off.Count <= 0 {
			return nil, fmt.Errorf("proxmox backend: offering %s/%s has non-positive count %d", off.InstanceType, off.Zone, off.Count)
		}
	}
	return &proxmoxBackend{
		providerName: providerName,
		client:       client,
		offerings:    offerings,
		catalog:      catalog,
		pricing:      pr,
		logger:       logger,
	}, nil
}

// slotID is the stable BigFleet machine id for one offering slot. A Speculative
// slot keeps this id across its whole lifecycle (created VMs are tagged with it,
// so DescribeManaged maps back to it).
func slotID(providerName string, capacity providerkit.CapacityType, off offering, i int) string {
	return fmt.Sprintf("%s/%s/%s/%s/%03d", providerName, capacity, off.InstanceType, off.Zone, i)
}

// speculativeSlots renders the configured offerings as Speculative Instance
// records with full, validated field shape. Reads only static catalog/pricing
// state, so it never blocks on the network.
func (b *proxmoxBackend) speculativeSlots() []providerkit.Instance {
	var out []providerkit.Instance
	for _, off := range b.offerings {
		capacity, _ := off.capacityType() // validated in newProxmoxBackend
		for i := 0; i < off.Count; i++ {
			id := slotID(b.providerName, capacity, off, i)
			out = append(out, providerkit.Instance{
				ID:           id,
				State:        providerkit.StateSpeculative,
				InstanceType: off.InstanceType,
				Zone:         off.Zone,
				CapacityType: capacity,
				PricePerHour: b.pricing.price(off.InstanceType),
				// Proxmox VMs are not preemptible: the genuine,
				// provider-declared interruption probability is exactly 0.
				InterruptionProbability: 0,
				Resources:               cloneMap(off.Resources),
				Allocatable:             b.catalog.allocatable(off.InstanceType),
				Labels:                  cloneMap(off.Labels),
			})
		}
	}
	return out
}

// Describe returns the substrate inventory: every offering slot as Speculative,
// upgraded to Idle (with its host) when a managed VM already backs it, plus any
// orphan managed VMs. The kit calls this to seed a fresh store; the persisted
// store is the primary restart path.
//
// A machine-id-tagged managed VM owns its slot while it is alive, keeping the
// slot from being re-seeded Speculative so Create can't clone a duplicate under
// the same machine id. Untagged managed VMs are surfaced as orphans under their
// host ref so they are not lost.
func (b *proxmoxBackend) Describe(ctx context.Context) ([]providerkit.Instance, error) {
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe managed VMs: %w", err)
	}
	bySlot := make(map[string]vmInstance, len(managed))
	var orphans []vmInstance
	for _, vm := range managed {
		switch {
		case vm.MachineID != "":
			bySlot[vm.MachineID] = vm // running or not; decided per-slot below
		case vm.Running:
			orphans = append(orphans, vm) // managed + running, but untagged
		}
	}

	slots := b.speculativeSlots()
	out := make([]providerkit.Instance, 0, len(slots)+len(bySlot)+len(orphans))
	for _, slot := range slots {
		if vm, ok := bySlot[slot.ID]; ok {
			delete(bySlot, slot.ID)
			// The VM owns its slot whether or not it is currently running: a
			// tagged-but-stopped VM (host power-cycle, an HA stop, an in-guest
			// poweroff) is surfaced Idle WITH its host, so it stays reapable via
			// Delete (the kit emits no Delete for a Speculative slot) and Create
			// can't clone a duplicate under the same machine id. The out-of-band
			// stop is healed by EnsureRunning in Configure/Drain before any guest
			// work runs. (Only a destroyed VM — gone from DescribeManaged —
			// releases its slot back to Speculative.)
			slot.State = providerkit.StateIdle
			slot.Host = providerkit.HostRef{Provider: b.providerName, Ref: vm.hostRef()}
		}
		out = append(out, slot)
	}
	// Tagged VMs matching no current offering slot (offering shrank, or a manually
	// tagged VM). These have no Speculative slot to fall back to, so they are
	// surfaced under their machine id regardless of power state — a stopped one
	// must NOT be silently dropped (Describe is the only path that surfaces
	// managed VMs without a persisted store; losing it would leak its disks and
	// leave it unmanaged). Surfaced Idle-with-host, it stays reapable: the kit
	// scales the now-offering-less id in and Delete tears the VM + disks down.
	for id, vm := range bySlot {
		out = append(out, b.vmToIdle(id, vm))
	}
	for _, vm := range orphans {
		out = append(out, b.vmToIdle(vm.hostRef(), vm))
	}
	return out, nil
}

func (b *proxmoxBackend) vmToIdle(machineID string, vm vmInstance) providerkit.Instance {
	// Prefer the instance_type that slotID encoded into the machine id: for a
	// managed slot whose offering was removed, the id is ground truth for what the
	// VM was actually cloned as, so allocatable/price can't be mis-stated by an
	// arbitrary same-zone offering (which could be a LARGER type and cause
	// overcommit). Fall back to a representative same-zone offering only for an
	// orphan (whose id is a host ref, not a slot id) or an unknown type.
	instanceType, ok := b.parseSlotInstanceType(machineID, vm.Node)
	var resources map[string]string
	if !ok {
		off, found := b.recoverOffering(vm.Node)
		if found {
			instanceType = off.InstanceType
			resources = off.Resources
		}
	} else if off, found := b.offeringFor(vm.Node, instanceType); found {
		// Same-node offering of the SAME type still configured: use its declared
		// request shape. Otherwise leave Resources nil (the true per-replica shape
		// left with the removed offering; identity/allocatable/price stay accurate).
		resources = off.Resources
	}
	return providerkit.Instance{
		ID:                      machineID,
		State:                   providerkit.StateIdle,
		Host:                    providerkit.HostRef{Provider: b.providerName, Ref: vm.hostRef()},
		InstanceType:            instanceType,
		Zone:                    vm.Node,
		CapacityType:            providerkit.CapacityOnDemand,
		PricePerHour:            b.pricing.price(instanceType),
		InterruptionProbability: 0,
		Resources:               cloneMap(resources),
		Allocatable:             b.catalog.allocatable(instanceType),
	}
}

// parseSlotInstanceType extracts the instance_type that slotID encoded into a
// machine id ("<provider>/<capacity>/<instance_type>/<zone>/<NNN>") when the id
// is one this provider minted, its zone (node) matches, and the instance_type is
// still in the catalog. Parsed from the right because the provider label may
// itself contain '/', while capacity/instance_type/zone/index never do (the node
// label rejects '/' would be the assumption; the index is numeric).
func (b *proxmoxBackend) parseSlotInstanceType(id, node string) (string, bool) {
	parts := strings.Split(id, "/")
	if len(parts) < 5 {
		return "", false
	}
	n := len(parts)
	provider := strings.Join(parts[:n-4], "/")
	capacityStr, instanceType, slotZone, index := parts[n-4], parts[n-3], parts[n-2], parts[n-1]
	if provider != b.providerName || slotZone != node {
		return "", false
	}
	if _, err := strconv.Atoi(index); err != nil {
		return "", false
	}
	if !capacityKnown(capacityStr) || !b.catalog.has(instanceType) {
		return "", false
	}
	return instanceType, true
}

// capacityKnown reports whether s is the String() form of a kit CapacityType the
// slot id could have embedded. Proxmox only ever mints OnDemand, but accepting
// the full set keeps the parser robust to a manually crafted id.
func capacityKnown(s string) bool {
	for _, c := range []providerkit.CapacityType{
		providerkit.CapacityBareMetal,
		providerkit.CapacityReserved,
		providerkit.CapacityOnDemand,
		providerkit.CapacitySpot,
	} {
		if c.String() == s {
			return true
		}
	}
	return false
}

// offeringFor returns the configured offering for an exact (zone, instance_type),
// if one is still configured.
func (b *proxmoxBackend) offeringFor(zone, instanceType string) (offering, bool) {
	for _, off := range b.offerings {
		if off.Zone == zone && off.InstanceType == instanceType {
			return off, true
		}
	}
	return offering{}, false
}

// recoverOffering returns the configured offering that best describes a recovered
// VM on the given node, preferring an offering on that exact node and falling
// back to the first configured offering. The bool is false only when no
// offerings are configured.
func (b *proxmoxBackend) recoverOffering(node string) (offering, bool) {
	for _, off := range b.offerings {
		if off.Zone == node {
			return off, true
		}
	}
	if len(b.offerings) > 0 {
		return b.offerings[0], true
	}
	return offering{}, false
}

// CreateInstance clones + starts the Proxmox VM for a Speculative slot and
// returns its host. The cluster-specific bootstrap is delivered later by
// ConfigureInstance, over the qemu guest agent.
func (b *proxmoxBackend) CreateInstance(ctx context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	m := req.Machine
	cap, ok := b.catalog.capacity(m.InstanceType)
	if !ok {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("create: unknown instance_type %q", m.InstanceType)
	}
	vm, err := b.client.CloneVM(ctx, vmSpec{
		MachineID:        m.ID,
		InstanceType:     m.InstanceType,
		Zone:             m.Zone,
		TemplateVMID:     cap.TemplateVMID,
		Cores:            cap.VCPU,
		MemoryMiB:        cap.MemMiB,
		IdempotencyToken: req.OperationID,
	})
	if err != nil {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("clone VM %s: %w", m.InstanceType, err)
	}
	// The kit's empty-host guard only fires when BOTH HostRef fields are empty,
	// and Provider is always set here — so guard the VMID explicitly. A host with
	// no VMID would settle the machine Idle, then break every later
	// Configure/Drain/Delete.
	if vm.VMID == 0 {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("clone VM %s returned no VMID", m.InstanceType)
	}
	return providerkit.CreateInstanceResult{
		Host:        providerkit.HostRef{Provider: b.providerName, Ref: vm.hostRef()},
		Allocatable: b.catalog.allocatable(m.InstanceType),
	}, nil
}

// ConfigureInstance binds the running VM to a cluster and delivers the opaque
// bootstrap blob over the qemu guest agent.
func (b *proxmoxBackend) ConfigureInstance(ctx context.Context, req providerkit.ConfigureInstanceRequest) error {
	vm, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	// Heal an out-of-band stop before talking to the guest agent: a VM the kit
	// holds Idle may have been stopped (operator power-cycle, an HA event, a
	// maintenance reboot), and the guest-agent bootstrap would otherwise loop
	// until the transition times out. EnsureRunning is a no-op when already
	// running + agent-reachable.
	if err := b.client.EnsureRunning(ctx, vm); err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	return b.client.ApplyBootstrap(ctx, vm, req.ClusterID, req.BootstrapBlob)
}

// DrainInstance drains the kubelet and removes the cluster binding, leaving the
// VM running but unbound (Idle).
func (b *proxmoxBackend) DrainInstance(ctx context.Context, req providerkit.DrainInstanceRequest) error {
	vm, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	// Same out-of-band-stop heal as Configure: the drain hook runs over the guest
	// agent, so the VM must be powered on first.
	if err := b.client.EnsureRunning(ctx, vm); err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	return b.client.DrainNode(ctx, vm, req.GracePeriodSeconds)
}

// DeleteInstance stops + destroys the Proxmox VM and its disks (purge); the slot
// returns to Speculative.
func (b *proxmoxBackend) DeleteInstance(ctx context.Context, req providerkit.DeleteInstanceRequest) error {
	node, vmid, ok := splitHostRef(req.Machine.Host.Ref)
	if !ok {
		return fmt.Errorf("delete: machine %s has no usable host ref %q", req.Machine.ID, req.Machine.Host.Ref)
	}
	return b.client.DeleteVM(ctx, node, vmid)
}

// resolveHost recovers the substrate VM view for a machine the kit holds, by its
// host ref ("<node>/<vmid>").
func (b *proxmoxBackend) resolveHost(ctx context.Context, m providerkit.Machine) (vmInstance, error) {
	node, vmid, ok := splitHostRef(m.Host.Ref)
	if !ok {
		return vmInstance{}, fmt.Errorf("machine %s has no usable host ref %q", m.ID, m.Host.Ref)
	}
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return vmInstance{}, fmt.Errorf("describe managed VMs: %w", err)
	}
	for _, vm := range managed {
		if vm.Node == node && vm.VMID == vmid {
			return vm, nil
		}
	}
	// Fall back to a minimal view; the real client can still address the VM by
	// (node, vmid) even if a transient describe missed it.
	return vmInstance{Node: node, VMID: vmid, MachineID: m.ID}, nil
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
	_ providerkit.Backend = (*proxmoxBackend)(nil)
	_ providerkit.Deleter = (*proxmoxBackend)(nil)
)
