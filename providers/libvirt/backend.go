package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// libvirtBackend is the libvirt implementation of providerkit.Backend (+
// Deleter). It is pure substrate logic: it maps the kit's lifecycle calls onto
// libvirt domain operations and populates the substrate fields it knows
// (instance_type, zone, capacity_type, price_per_hour, interruption_probability,
// resources, allocatable, host). Fencing, idempotency, async dispatch,
// transition timeouts, shard_metadata, and the rest are providerkit's job — this
// file never touches them.
//
// Configure-bootstrap reconciliation: a libvirt domain's cloud-init NoCloud
// datasource is consumed by cloud-init at first boot, so CreateDomain defines
// the domain with the generic pre-binding --base-user-data, and the
// cluster-specific bootstrap blob is delivered later by ConfigureInstance (the
// real client writes the blob into the guest and runs the in-image bootstrap
// hook via the qemu guest agent). This keeps the kit's invariant that an Idle
// machine already carries a real, reachable host, and delivers the blob exactly
// once when the binding is established.
type libvirtBackend struct {
	providerName string // HostRef.provider label, e.g. "libvirt-rack1"
	client       libvirtClient
	image        string // base/golden cloud image volume name for CreateDomain
	offerings    []offering
	catalog      *instanceCatalog
	pricing      *pricing
	baseUserData []byte // generic pre-binding cloud-init baked in at Create
	logger       *slog.Logger
}

func newLibvirtBackend(providerName, image string, client libvirtClient, offerings []offering, catalog *instanceCatalog, pr *pricing, baseUserData []byte, logger *slog.Logger) (*libvirtBackend, error) {
	if len(offerings) == 0 {
		return nil, fmt.Errorf("libvirt backend: no offerings configured")
	}
	for _, off := range offerings {
		if _, err := off.capacityType(); err != nil {
			return nil, fmt.Errorf("libvirt backend: offering %s/%s: %w", off.InstanceType, off.Zone, err)
		}
		if off.InstanceType == "" {
			return nil, fmt.Errorf("libvirt backend: offering with empty instance_type")
		}
		if !catalog.has(off.InstanceType) {
			return nil, fmt.Errorf("libvirt backend: offering instance_type %q is not in the instance-type catalog", off.InstanceType)
		}
		// The provider registers multi-zone (RequireZone), so a zoneless offering
		// would only fail later at seed time — reject it up front.
		if off.Zone == "" {
			return nil, fmt.Errorf("libvirt backend: offering %s with empty zone (libvirt host)", off.InstanceType)
		}
	}
	return &libvirtBackend{
		providerName: providerName,
		client:       client,
		image:        image,
		offerings:    offerings,
		catalog:      catalog,
		pricing:      pr,
		baseUserData: baseUserData,
		logger:       logger,
	}, nil
}

// slotID is the stable BigFleet machine id for one offering slot. A Speculative
// slot keeps this id across its whole lifecycle (created domains are tagged with
// it, so DescribeManaged maps back to it).
func slotID(providerName string, capacity providerkit.CapacityType, off offering, i int) string {
	return fmt.Sprintf("%s/%s/%s/%s/%03d", providerName, capacity, off.InstanceType, off.Zone, i)
}

// speculativeSlots renders the configured offerings as Speculative Instance
// records with full, validated field shape. Reads only static catalog/pricing
// state, so it never blocks on the network.
func (b *libvirtBackend) speculativeSlots() []providerkit.Instance {
	var out []providerkit.Instance
	for _, off := range b.offerings {
		capacity, _ := off.capacityType() // validated in newLibvirtBackend
		for i := 0; i < off.Count; i++ {
			id := slotID(b.providerName, capacity, off, i)
			out = append(out, providerkit.Instance{
				ID:           id,
				State:        providerkit.StateSpeculative,
				InstanceType: off.InstanceType,
				Zone:         off.Zone,
				CapacityType: capacity,
				PricePerHour: b.pricing.price(off.InstanceType, capacity),
				// Local KVM VMs have no preemption market: the genuine,
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
// upgraded to Idle (with its host) when a managed domain already backs it, plus
// any orphan managed domains. The kit calls this to seed a fresh store; the
// persisted store is the primary restart path.
//
// A machine-id-tagged managed domain owns its slot while it is alive, keeping the
// slot from being re-seeded Speculative so Create can't define a duplicate under
// the same machine id. Untagged managed domains are surfaced as orphans under
// their host ref so they are not lost.
func (b *libvirtBackend) Describe(ctx context.Context) ([]providerkit.Instance, error) {
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe managed domains: %w", err)
	}
	bySlot := make(map[string]domainInstance, len(managed))
	var orphans []domainInstance
	for _, dom := range managed {
		switch {
		case dom.MachineID != "":
			bySlot[dom.MachineID] = dom // owns its slot, running or not
		case dom.Running:
			orphans = append(orphans, dom) // managed + running, but untagged
		}
	}

	slots := b.speculativeSlots()
	out := make([]providerkit.Instance, 0, len(slots)+len(bySlot)+len(orphans))
	for _, slot := range slots {
		if dom, ok := bySlot[slot.ID]; ok {
			slot.State = providerkit.StateIdle
			slot.Host = providerkit.HostRef{Provider: b.providerName, Ref: dom.hostRef()}
			delete(bySlot, slot.ID)
		}
		out = append(out, slot)
	}
	// Tagged domains matching no current offering slot (offering shrank, or a
	// manually tagged domain), then untagged-but-running managed domains.
	for id, dom := range bySlot {
		out = append(out, b.domainToIdle(id, dom))
	}
	for _, dom := range orphans {
		out = append(out, b.domainToIdle(dom.hostRef(), dom))
	}
	return out, nil
}

func (b *libvirtBackend) domainToIdle(machineID string, dom domainInstance) providerkit.Instance {
	// Recover a representative instance type for the orphan from a still-configured
	// offering on the same host, so it keeps matching a demand profile. The domain
	// itself does not report its catalog flavor name, only raw vCPU/memory, so we
	// trust the offering catalog. Falls back to the first offering's type.
	instanceType, resources := b.recoverShape(dom.Zone)
	capacity := providerkit.CapacityOnDemand
	if len(b.offerings) > 0 {
		if c, err := b.offerings[0].capacityType(); err == nil {
			capacity = c
		}
	}
	return providerkit.Instance{
		ID:                      machineID,
		State:                   providerkit.StateIdle,
		Host:                    providerkit.HostRef{Provider: b.providerName, Ref: dom.hostRef()},
		InstanceType:            instanceType,
		Zone:                    dom.Zone,
		CapacityType:            capacity,
		PricePerHour:            b.pricing.price(instanceType, capacity),
		InterruptionProbability: 0,
		Resources:               resources,
		Allocatable:             b.catalog.allocatable(instanceType),
	}
}

// recoverShape returns a representative instance type and per-replica resources
// for an orphan domain on the given zone, preferring an offering on that exact
// host. Falls back to the first configured offering.
func (b *libvirtBackend) recoverShape(zone string) (string, map[string]string) {
	for _, off := range b.offerings {
		if off.Zone == zone {
			return off.InstanceType, cloneMap(off.Resources)
		}
	}
	if len(b.offerings) > 0 {
		return b.offerings[0].InstanceType, cloneMap(b.offerings[0].Resources)
	}
	return "", nil
}

// CreateInstance defines + starts the libvirt domain for a Speculative slot and
// returns its host. The cluster-specific bootstrap is delivered later by
// ConfigureInstance, because the NoCloud datasource is consumed at first boot.
func (b *libvirtBackend) CreateInstance(ctx context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	m := req.Machine
	cap, ok := b.catalog.capacity(m.InstanceType)
	if !ok {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("create: unknown instance_type %q", m.InstanceType)
	}
	dom, err := b.client.CreateDomain(ctx, domainSpec{
		MachineID:        m.ID,
		InstanceType:     m.InstanceType,
		Zone:             m.Zone,
		VCPUs:            cap.VCPU,
		MemoryMiB:        cap.MemMiB,
		Image:            b.image,
		IdempotencyToken: req.OperationID,
		BaseUserData:     b.baseUserData,
	})
	if err != nil {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("create domain %s: %w", m.InstanceType, err)
	}
	// The kit's empty-host guard only fires when BOTH HostRef fields are empty,
	// and Provider is always set here — so guard the domain name explicitly. A host
	// with an empty Ref would settle the machine Idle, then break every later
	// Configure/Drain/Delete.
	if dom.DomainName == "" {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("create domain %s returned no domain name", m.InstanceType)
	}
	return providerkit.CreateInstanceResult{
		Host:        providerkit.HostRef{Provider: b.providerName, Ref: dom.hostRef()},
		Allocatable: b.catalog.allocatable(m.InstanceType),
	}, nil
}

// ConfigureInstance binds the running domain to a cluster and delivers the opaque
// bootstrap blob (real impl: write the blob + run the in-image hook via the qemu
// guest agent).
func (b *libvirtBackend) ConfigureInstance(ctx context.Context, req providerkit.ConfigureInstanceRequest) error {
	dom, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	return b.client.ApplyBootstrap(ctx, dom, req.ClusterID, req.BootstrapBlob)
}

// DrainInstance drains the kubelet and removes the cluster binding, leaving the
// domain running but unbound (Idle).
func (b *libvirtBackend) DrainInstance(ctx context.Context, req providerkit.DrainInstanceRequest) error {
	dom, err := b.resolveHost(ctx, req.Machine)
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	return b.client.DrainNode(ctx, dom, req.GracePeriodSeconds)
}

// DeleteInstance destroys + undefines the libvirt domain and deletes its overlay
// disk; the slot returns to Speculative.
func (b *libvirtBackend) DeleteInstance(ctx context.Context, req providerkit.DeleteInstanceRequest) error {
	zone, name, ok := splitHostRef(req.Machine.Host.Ref)
	if !ok {
		return fmt.Errorf("delete: machine %s has no host ref", req.Machine.ID)
	}
	return b.client.DeleteDomain(ctx, zone, name)
}

// resolveHost recovers the substrate domain view for a machine the kit holds, by
// its host ref ("<zone>/<domain>").
func (b *libvirtBackend) resolveHost(ctx context.Context, m providerkit.Machine) (domainInstance, error) {
	zone, name, ok := splitHostRef(m.Host.Ref)
	if !ok {
		return domainInstance{}, fmt.Errorf("machine %s has no host ref", m.ID)
	}
	managed, err := b.client.DescribeManaged(ctx)
	if err != nil {
		return domainInstance{}, fmt.Errorf("describe managed domains: %w", err)
	}
	for _, dom := range managed {
		if dom.Zone == zone && dom.DomainName == name {
			return dom, nil
		}
	}
	// Fall back to a minimal view; the real client can still address the domain by
	// (zone, name) even if a transient describe missed it.
	return domainInstance{Zone: zone, DomainName: name, MachineID: m.ID}, nil
}

// splitHostRef splits a "<zone>/<domain>" host ref into its parts.
func splitHostRef(ref string) (zone, domain string, ok bool) {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '/' {
			return ref[:i], ref[i+1:], ref[:i] != "" && ref[i+1:] != ""
		}
	}
	return "", "", false
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
	_ providerkit.Backend = (*libvirtBackend)(nil)
	_ providerkit.Deleter = (*libvirtBackend)(nil)
)
