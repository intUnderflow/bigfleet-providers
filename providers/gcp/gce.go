package main

import "context"

// gceClient is the entire Google Compute Engine substrate surface the
// [gcpBackend] drives. It is deliberately tiny and substrate-shaped (not
// BigFleet-shaped) — providerkit owns every cross-cutting concern, so this is
// the only place GCE appears.
//
// Two implementations ship:
//   - gceReal (gcereal.go) wraps cloud.google.com/go/compute/apiv1 — the
//     production client.
//   - gceFake (gcefake.go) is an in-memory simulator that backs unit tests and
//     credential-free certification runs.
//
// Every method is scoped to one GCP project + region, fixed at construction
// (one provider process per project/region, per the author guide).
type gceClient interface {
	// Insert launches exactly one instance (compute.Instances.Insert) and
	// returns its substrate identity. It labels the instance with the BigFleet
	// machine id so DescribeManaged can recover inventory after a restart.
	Insert(ctx context.Context, spec instanceSpec) (gceInstance, error)

	// DeleteInstance deletes the instance identified by (zone, name)
	// (compute.Instances.Delete). The slot returns to Speculative. It is
	// idempotent: deleting an already-gone instance succeeds.
	DeleteInstance(ctx context.Context, zone, name string) error

	// DescribeManaged returns every BigFleet-managed instance in the region
	// (instances across the region's zones carrying the bigfleet-managed=true
	// label), so a provider with no persisted store can still rebuild inventory.
	DescribeManaged(ctx context.Context) ([]gceInstance, error)

	// ApplyBootstrap binds a running instance to a cluster and delivers the
	// opaque bootstrap blob by writing it to the instance's `startup-script`
	// metadata and resetting the instance so the script runs and the node joins
	// the cluster (real impl: Instances.SetMetadata + Instances.Reset + a
	// bigfleet-cluster label). The blob is the kubelet join data — never parse
	// it.
	ApplyBootstrap(ctx context.Context, inst gceInstance, clusterID string, bootstrap []byte) error

	// DrainNode removes the cluster binding: it strips the delivered
	// `startup-script` metadata (so the node will not rejoin on a future boot),
	// honours the grace period, and clears the bigfleet-cluster label — leaving
	// the instance running but unbound (Idle). Real impl: Instances.SetMetadata.
	DrainNode(ctx context.Context, inst gceInstance, gracePeriodSeconds int64) error

	// DescribeMachineTypeCapacities resolves the hardware capacity (vCPU +
	// memory) of the given machine types via compute.MachineTypes.Get, for
	// Machine.allocatable. Types GCE does not return are simply absent from the
	// result (the caller falls back to the pinned table).
	DescribeMachineTypeCapacities(ctx context.Context, refs []machineTypeRef) (map[string]machineCapacity, error)
}

// instanceSpec is the launch request handed to Insert.
type instanceSpec struct {
	MachineID   string
	MachineType string // GCE machine type short name, e.g. n2-standard-8
	Zone        string // GCE zone, e.g. us-central1-a
	// Spot selects the SPOT provisioning model (scheduling.provisioningModel =
	// SPOT) — the current preemptible model.
	Spot bool
	// Capacity is the canonical capacity-type string ("on_demand" | "spot" |
	// "reserved"), stamped as a bigfleet-capacity label so the capacity type is
	// recoverable from GCE alone (DescribeManaged), not just guessed from the
	// provisioning model.
	Capacity string
	// IdempotencyToken is the kit's per-operation id. The real client folds it
	// into the instance name so a retried Insert maps to the same instance
	// (name collisions are the idempotent case); the fake models the same via a
	// token→instance map.
	IdempotencyToken string
	// BaseStartupScript is the generic pre-binding startup script baked in at
	// launch (a cluster-agnostic node bootstrap). The cluster-specific bootstrap
	// arrives later via ApplyBootstrap, which overwrites `startup-script`.
	BaseStartupScript []byte
}

// gceInstance is the substrate view of one GCE instance, free of any GCE SDK
// types so the backend never sees the SDK.
type gceInstance struct {
	Name        string // GCE instance name
	Zone        string // GCE zone short name
	MachineID   string // bigfleet-machine-id label (decoded)
	MachineType string // machine type short name
	Spot        bool
	Capacity    string // bigfleet-capacity label (canonical capacity string)
	ClusterID   string // bigfleet-cluster label, empty when unbound
	SelfLink    string // fully-qualified instance self-link (informational)
	// Running reports whether the instance is in a live state (PROVISIONING /
	// STAGING / RUNNING / REPAIRING), as opposed to STOPPING / TERMINATED.
	Running bool
	// Preempted reports that GCE has preempted this SPOT instance. The provider
	// only ever Deletes instances (never stops them), so a SPOT VM observed in
	// TERMINATED status was stopped by GCE — a preemption. Drives the observed
	// interruption-probability signal.
	Preempted bool
}

// machineTypeRef identifies one machine type in a specific zone, for capacity
// resolution (GCE machine types are zone-scoped resources).
type machineTypeRef struct {
	MachineType string
	Zone        string
}

// machineCapacity is the real hardware capacity of a GCE machine type, used to
// populate Machine.allocatable. Memory is held in MiB — the unit GCE reports
// (MachineType.memory_mb) — so a type whose memory is not a whole GiB resolves
// exactly instead of truncating.
type machineCapacity struct {
	VCPU   int
	MemMiB int64
}
