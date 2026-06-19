package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// awsBackend is the AWS EC2 implementation of providerkit.Backend (+ Deleter).
// It is pure substrate logic: it maps the kit's lifecycle calls onto EC2 API
// calls and populates the substrate fields it knows (instance_type, zone,
// capacity_type, price_per_hour, interruption_probability, resources,
// allocatable, host). Fencing, idempotency, async dispatch, transition
// timeouts, shard_metadata, and the rest are providerkit's job — this file
// never touches them.
type awsBackend struct {
	providerName string // HostRef.provider label, e.g. "aws-us-east-1"
	region       string
	ec2          ec2Client
	offerings    []offering
	pricing      *pricing
	interruption *interruption
	baseUserData []byte // generic pre-binding bootstrap baked in at Create
	logger       *slog.Logger
}

func newAWSBackend(providerName, region string, ec2 ec2Client, offerings []offering, pr *pricing, in *interruption, baseUserData []byte, logger *slog.Logger) (*awsBackend, error) {
	if len(offerings) == 0 {
		return nil, fmt.Errorf("aws backend: no offerings configured")
	}
	for _, off := range offerings {
		if _, err := off.capacityType(); err != nil {
			return nil, fmt.Errorf("aws backend: offering %s/%s: %w", off.InstanceType, off.Zone, err)
		}
		if off.InstanceType == "" {
			return nil, fmt.Errorf("aws backend: offering with empty instance_type")
		}
		// The provider registers multi-zone (RequireZone), so a zoneless
		// offering would only fail later at seed time — reject it up front.
		if off.Zone == "" {
			return nil, fmt.Errorf("aws backend: offering %s with empty zone", off.InstanceType)
		}
	}
	return &awsBackend{
		providerName: providerName,
		region:       region,
		ec2:          ec2,
		offerings:    offerings,
		pricing:      pr,
		interruption: in,
		baseUserData: baseUserData,
		logger:       logger,
	}, nil
}

// slotID is the stable BigFleet machine id for one offering slot. A Speculative
// slot keeps this id across its whole lifecycle (Created instances are tagged
// with it, so DescribeManaged maps back to it).
func slotID(providerName string, capacity providerkit.CapacityType, off offering, i int) string {
	return fmt.Sprintf("%s/%s/%s/%s/%03d", providerName, capacity, off.InstanceType, off.Zone, i)
}

// speculativeSlots renders the configured offerings as Speculative Machine
// records with full, validated field shape. Reads only cached pricing /
// interruption state, so it never blocks on the network.
func (b *awsBackend) speculativeSlots() []providerkit.Instance {
	var out []providerkit.Instance
	for _, off := range b.offerings {
		capacity, _ := off.capacityType() // validated in newAWSBackend
		for i := 0; i < off.Count; i++ {
			id := slotID(b.providerName, capacity, off, i)
			out = append(out, providerkit.Instance{
				ID:                      id,
				State:                   providerkit.StateSpeculative,
				InstanceType:            off.InstanceType,
				Zone:                    off.Zone,
				CapacityType:            capacity,
				PricePerHour:            b.pricing.price(off.InstanceType, off.Zone, capacity),
				InterruptionProbability: b.interruption.probability(id, off.InstanceType, capacity),
				Resources:               cloneMap(off.Resources),
				Allocatable:             allocatable(off.InstanceType),
				Labels:                  slotLabels(off),
			})
		}
	}
	return out
}

func slotLabels(off offering) map[string]string {
	labels := cloneMap(off.Labels)
	if acc, ok := acceleratorLabel(off.InstanceType); ok {
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
// A machine-id-tagged managed instance owns its slot while it is alive —
// DescribeManaged returns pending/running/stopping/stopped instances, and any
// of those keeps the slot from being re-seeded Speculative so Create can't
// launch a duplicate under the same machine id. A shutting-down/terminated
// instance is releasing its slot and is correctly absent (the slot returns to
// Speculative for re-provisioning). Untagged-but-running managed instances are
// surfaced as orphans under their instance id so they are not lost; untagged
// non-running ones are skipped (too anomalous to seed).
func (b *awsBackend) Describe(ctx context.Context) ([]providerkit.Instance, error) {
	managed, err := b.ec2.DescribeManaged(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe managed instances: %w", err)
	}
	bySlot := make(map[string]ec2Instance, len(managed))
	var orphans []ec2Instance
	for _, m := range managed {
		switch {
		case m.MachineID != "":
			bySlot[m.MachineID] = m // owns its slot, running or not
		case m.Running:
			orphans = append(orphans, m) // managed + running, but untagged
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

func (b *awsBackend) instanceToIdle(machineID string, inst ec2Instance) providerkit.Instance {
	// Prefer the capacity recorded at Create (bigfleet:capacity tag); fall back
	// to the spot lifecycle for instances launched without the tag. Defaulting
	// a Reserved/BareMetal instance to ON_DEMAND would make the shard's
	// idle-release path eligible to Delete capacity it should hold forever.
	capacity := parseCapacityTag(inst.Capacity)
	if capacity == providerkit.CapacityUnspecified {
		capacity = providerkit.CapacityOnDemand
		if inst.Spot {
			capacity = providerkit.CapacitySpot
		}
	}
	return providerkit.Instance{
		ID:                      machineID,
		State:                   providerkit.StateIdle,
		Host:                    providerkit.HostRef{Provider: b.providerName, Ref: inst.InstanceID},
		InstanceType:            inst.InstanceType,
		Zone:                    inst.Zone,
		CapacityType:            capacity,
		PricePerHour:            b.pricing.price(inst.InstanceType, inst.Zone, capacity),
		InterruptionProbability: b.interruption.probability(machineID, inst.InstanceType, capacity),
		Allocatable:             allocatable(inst.InstanceType),
	}
}

// CreateInstance launches the EC2 instance for a Speculative slot and returns
// its host. The cluster-specific bootstrap is delivered later by
// ConfigureInstance, because EC2 user-data is immutable post-launch.
func (b *awsBackend) CreateInstance(ctx context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	m := req.Machine
	inst, err := b.ec2.RunInstance(ctx, runSpec{
		MachineID:        m.ID,
		InstanceType:     m.InstanceType,
		Zone:             m.Zone,
		Spot:             m.CapacityType == providerkit.CapacitySpot,
		Capacity:         capacityString(m.CapacityType),
		IdempotencyToken: req.OperationID,
		BaseUserData:     b.baseUserData,
	})
	if err != nil {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("RunInstances %s: %w", m.InstanceType, err)
	}
	// The kit's empty-host guard only fires when BOTH HostRef fields are empty,
	// and Provider is always set here — so guard the instance id explicitly. A
	// host with an empty Ref would settle the machine Idle, then break every
	// later Configure/Drain/Delete.
	if inst.InstanceID == "" {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("RunInstances %s returned no instance id", m.InstanceType)
	}
	return providerkit.CreateInstanceResult{
		Host:        providerkit.HostRef{Provider: b.providerName, Ref: inst.InstanceID},
		Allocatable: allocatable(m.InstanceType),
	}, nil
}

// ConfigureInstance binds the running instance to a cluster and delivers the
// opaque bootstrap blob (real impl: SSM SendCommand).
func (b *awsBackend) ConfigureInstance(ctx context.Context, req providerkit.ConfigureInstanceRequest) error {
	if req.Machine.Host.Ref == "" {
		return fmt.Errorf("configure: machine %s has no host", req.Machine.ID)
	}
	return b.ec2.ApplyBootstrap(ctx, req.Machine.Host.Ref, req.ClusterID, req.BootstrapBlob)
}

// DrainInstance cordons + drains the kubelet and removes the cluster binding,
// leaving the instance running but unbound (Idle).
func (b *awsBackend) DrainInstance(ctx context.Context, req providerkit.DrainInstanceRequest) error {
	if req.Machine.Host.Ref == "" {
		return fmt.Errorf("drain: machine %s has no host", req.Machine.ID)
	}
	return b.ec2.DrainNode(ctx, req.Machine.Host.Ref, req.GracePeriodSeconds)
}

// DeleteInstance terminates the EC2 instance; the slot returns to Speculative.
func (b *awsBackend) DeleteInstance(ctx context.Context, req providerkit.DeleteInstanceRequest) error {
	if req.Machine.Host.Ref == "" {
		return fmt.Errorf("delete: machine %s has no host", req.Machine.ID)
	}
	if err := b.ec2.TerminateInstance(ctx, req.Machine.Host.Ref); err != nil {
		return err
	}
	// Only drop the observed interruption escalation once the delete actuated.
	b.interruption.clear(req.Machine.ID)
	return nil
}

// spotPairs lists the distinct (instanceType, zone) pairs of SPOT offerings, to
// drive pricing.refresh without touching the List hot path.
func (b *awsBackend) spotPairs() []spotPair {
	seen := make(map[string]bool)
	var out []spotPair
	for _, off := range b.offerings {
		capacity, _ := off.capacityType()
		if capacity != providerkit.CapacitySpot {
			continue
		}
		k := spotKey(off.InstanceType, off.Zone)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, spotPair{instanceType: off.InstanceType, zone: off.Zone})
	}
	return out
}

// refreshPrices warms / refreshes the spot price cache. Call at startup and on
// a timer. Returns the number of (type,zone) pairs that failed to refresh.
func (b *awsBackend) refreshPrices(ctx context.Context) int {
	return b.pricing.refresh(ctx, b.spotPairs())
}

// machineIDFor resolves an EC2 instance id to its BigFleet machine id (via the
// bigfleet:machine-id tag), or "" if it isn't a managed instance. Used by the
// interruption poller to attribute a spot notice to a machine.
func (b *awsBackend) machineIDFor(ctx context.Context, instanceID string) string {
	managed, err := b.ec2.DescribeManaged(ctx)
	if err != nil {
		return ""
	}
	for _, inst := range managed {
		if inst.InstanceID == instanceID {
			return inst.MachineID
		}
	}
	return ""
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

// parseCapacityTag maps a bigfleet:capacity tag value back to a kit
// CapacityType; an empty/unknown tag yields CapacityUnspecified so the caller
// can fall back to the spot lifecycle.
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
	_ providerkit.Backend = (*awsBackend)(nil)
	_ providerkit.Deleter = (*awsBackend)(nil)
)
