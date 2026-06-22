package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// defaultTransitionTimeout is short on purpose: the timeout-shaped B7xx tests
// (B703/B704) wait on it, so a quick value keeps the lane fast.
const defaultTransitionTimeout = 2 * time.Second

// Wire-controlled fault selectors. The conformance suite drives every fault
// purely over the six RPCs — via the Configure cluster_id and the Create
// machine labels — so the faultprovider needs no side channel.
const (
	// createFaultLabel marks a seeded Speculative machine whose CreateInstance
	// must error (drives Create -> FAILED). Set as a label so the suite can
	// List for it and Create exactly that machine.
	createFaultLabel = "conformance.fault/create"
	createFaultValue = "error"

	// Configure cluster_id selectors.
	clusterConfigureError = "fault-error"   // ConfigureInstance errors immediately.
	clusterConfigureTO    = "fault-timeout" // ConfigureInstance blocks until ctx is done.
	clusterConfigureSlow  = "fault-slow-ok" // ConfigureInstance ignores ctx, sleeps past the timeout, then succeeds.
	clusterDrainError     = "fault-drain-error"
	// clusterReadinessBlock: ConfigureInstance succeeds (the substrate side is
	// done) but ConfirmNodeReady blocks until ctx is done — the node never
	// reaches Ready, so the kit must hold CONFIGURING and time out to FAILED,
	// never reporting CONFIGURED (ADR-0056, behavior B708).
	clusterReadinessBlock = "fault-readiness-block"
)

// faultBackend is an in-memory providerkit.Backend (+Deleter) that injects
// substrate faults on command. It is mutex-guarded: Describe is called once at
// boot, but the actuator hooks inspect/record per-machine binding state so the
// fault hooks (Drain-error keyed on the bound cluster) stay consistent.
//
// FAULT HOOKS:
//   - CreateInstance: a machine carrying label createFaultLabel=createFaultValue
//     errors (Create -> FAILED); otherwise it succeeds with a HostRef.
//   - ConfigureInstance switches on cluster_id:
//     fault-error   -> error immediately (actuator failure)
//     fault-timeout -> block until ctx is done, then return ctx.Err() (the kit
//     transition timeout fires -> FAILED)
//     fault-slow-ok -> IGNORE ctx: sleep past the timeout, then succeed. The kit
//     times out to FAILED first; the late success must be DISCARDED.
//     default       -> success; record the bound cluster + shard_metadata.
//   - DrainInstance: errors iff the machine's recorded bound cluster is
//     fault-drain-error; otherwise clears the binding and succeeds.
//   - DeleteInstance: always succeeds (slot returns to Speculative).
type faultBackend struct {
	providerName      string
	transitionTimeout time.Duration
	seeds             []providerkit.Instance

	mu    sync.Mutex
	bound map[string]binding // machine id -> last successful Configure binding
}

type binding struct {
	cluster  string
	metadata map[string]string
}

func newFaultBackend(providerName string, seedCount int, transitionTimeout time.Duration) *faultBackend {
	return &faultBackend{
		providerName:      providerName,
		transitionTimeout: transitionTimeout,
		seeds:             seedInventory(providerName, seedCount),
		bound:             map[string]binding{},
	}
}

// Describe reports the seeded inventory once at boot.
func (b *faultBackend) Describe(_ context.Context) ([]providerkit.Instance, error) {
	return b.seeds, nil
}

// CreateInstance actuates a Speculative slot into an Idle host, erroring for the
// create-fault machines.
func (b *faultBackend) CreateInstance(_ context.Context, req providerkit.CreateInstanceRequest) (providerkit.CreateInstanceResult, error) {
	if req.Machine.Labels[createFaultLabel] == createFaultValue {
		return providerkit.CreateInstanceResult{}, fmt.Errorf("fault: CreateInstance injected error for %s", req.Machine.ID)
	}
	return providerkit.CreateInstanceResult{
		Host: providerkit.HostRef{Provider: b.providerName, Ref: "host-" + req.Machine.ID},
	}, nil
}

// ConfigureInstance injects the configure-time faults keyed on cluster_id.
func (b *faultBackend) ConfigureInstance(ctx context.Context, req providerkit.ConfigureInstanceRequest) error {
	switch req.ClusterID {
	case clusterConfigureError:
		return fmt.Errorf("fault: ConfigureInstance injected error for %s", req.Machine.ID)

	case clusterConfigureTO:
		// Respect ctx: block until the kit's transition timeout cancels us.
		<-ctx.Done()
		return ctx.Err()

	case clusterConfigureSlow:
		// IGNORE ctx: outlast the transition timeout, then report success. The
		// kit will already have moved the machine to FAILED, so this late
		// completion must be discarded (machine stays FAILED).
		time.Sleep(b.transitionTimeout + time.Second)
		b.record(req.Machine.ID, req.ClusterID, req.Machine.ShardMetadata)
		return nil

	default:
		b.record(req.Machine.ID, req.ClusterID, req.Machine.ShardMetadata)
		return nil
	}
}

// ConfirmNodeReady implements providerkit.ReadinessChecker (ADR-0056). The kit
// calls it after ConfigureInstance succeeds and holds the machine at CONFIGURING
// until it returns. For the clusterReadinessBlock selector it blocks until ctx
// is done (the node never reaches Ready), so the kit times the transition out to
// FAILED — the machine must NEVER be reported CONFIGURED on an un-Ready node. For
// every other cluster it returns nil immediately (normal Configure → Configured).
func (b *faultBackend) ConfirmNodeReady(ctx context.Context, req providerkit.ConfirmNodeReadyRequest) error {
	if req.ClusterID == clusterReadinessBlock {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// DrainInstance errors iff the machine was Configured onto the drain-fault
// cluster; otherwise it clears the binding and succeeds.
func (b *faultBackend) DrainInstance(_ context.Context, req providerkit.DrainInstanceRequest) error {
	b.mu.Lock()
	bd := b.bound[req.Machine.ID]
	b.mu.Unlock()
	if bd.cluster == clusterDrainError {
		return fmt.Errorf("fault: DrainInstance injected error for %s (cluster %s)", req.Machine.ID, bd.cluster)
	}
	b.mu.Lock()
	delete(b.bound, req.Machine.ID)
	b.mu.Unlock()
	return nil
}

// DeleteInstance always succeeds, returning the slot to Speculative.
func (b *faultBackend) DeleteInstance(_ context.Context, req providerkit.DeleteInstanceRequest) error {
	b.mu.Lock()
	delete(b.bound, req.Machine.ID)
	b.mu.Unlock()
	return nil
}

// record stores the cluster binding + shard_metadata so DrainInstance can
// inspect the bound cluster and the binding stays consistent.
func (b *faultBackend) record(id, cluster string, md map[string]string) {
	cp := make(map[string]string, len(md))
	for k, v := range md {
		cp[k] = v
	}
	b.mu.Lock()
	b.bound[id] = binding{cluster: cluster, metadata: cp}
	b.mu.Unlock()
}

// seedInventory builds count Speculative quota slots plus 8 extra Speculative
// slots carrying the create-fault label (ids faultcreate-N) for the
// Create-failure behavior (B702). It mirrors the valid field-shape the template
// and AWS use.
func seedInventory(providerName string, count int) []providerkit.Instance {
	out := make([]providerkit.Instance, 0, count+8)
	for i := 0; i < count; i++ {
		spot := i%3 == 0
		capType := providerkit.CapacityOnDemand
		prob := 0.0
		price := 0.40
		if spot {
			capType = providerkit.CapacitySpot
			prob = 0.05 // SPOT MUST declare a real interruption probability
			price = 0.12
		}
		out = append(out, providerkit.Instance{
			ID:                      fmt.Sprintf("%s-spec-%03d", providerName, i),
			State:                   providerkit.StateSpeculative,
			InstanceType:            "fault-standard-4",
			Zone:                    "fault-zone-a",
			CapacityType:            capType,
			PricePerHour:            price,
			InterruptionProbability: prob,
			Resources:               map[string]string{"cpu": "4", "memory": "16Gi"},
			Allocatable:             map[string]string{"cpu": "4", "memory": "16Gi"},
			Labels:                  map[string]string{"fault.conformance/pool": "default"},
		})
	}
	// 8 create-fault slots for B702.
	for i := 0; i < 8; i++ {
		out = append(out, providerkit.Instance{
			ID:                      fmt.Sprintf("faultcreate-%d", i),
			State:                   providerkit.StateSpeculative,
			InstanceType:            "fault-standard-4",
			Zone:                    "fault-zone-a",
			CapacityType:            providerkit.CapacityOnDemand,
			PricePerHour:            0.40,
			InterruptionProbability: 0.0,
			Resources:               map[string]string{"cpu": "4", "memory": "16Gi"},
			Allocatable:             map[string]string{"cpu": "4", "memory": "16Gi"},
			Labels:                  map[string]string{createFaultLabel: createFaultValue},
		})
	}
	return out
}

// Compile-time checks: the faultprovider is a full Backend and a Deleter.
var (
	_ providerkit.Backend = (*faultBackend)(nil)
	_ providerkit.Deleter = (*faultBackend)(nil)
)
