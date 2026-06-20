package main

import "context"

// ociClient is the entire OCI Compute substrate surface the [ociBackend] drives.
// It is deliberately tiny and substrate-shaped (not BigFleet-shaped) —
// providerkit owns every cross-cutting concern, so this is the only place OCI
// appears.
//
// Two implementations ship:
//   - ociReal (ocireal.go) wraps oci-go-sdk (core.ComputeClient +
//     computeinstanceagent for the Run Command bootstrap delivery) — the
//     production client.
//   - ociFake (ocifake.go) is an in-memory simulator that backs unit tests and
//     credential-free conformance/certification runs.
//
// Every method is scoped to one compartment + one region, fixed at construction
// (one provider process per compartment/region, per the author guide).
type ociClient interface {
	// LaunchInstance launches exactly one instance and returns its substrate
	// identity. It tags the instance with the BigFleet machine id (a freeform
	// tag) so DescribeManaged can recover inventory after a restart.
	LaunchInstance(ctx context.Context, spec launchSpec) (ociInstance, error)

	// TerminateInstance terminates the instance with the given OCID. The slot
	// returns to Speculative.
	TerminateInstance(ctx context.Context, instanceID string) error

	// DescribeManaged returns every BigFleet-managed instance in the compartment
	// (instances carrying the bigfleet-managed=true freeform tag), so a provider
	// with no persisted store can still rebuild inventory.
	DescribeManaged(ctx context.Context) ([]ociInstance, error)

	// ApplyBootstrap binds a running instance to a cluster and delivers the opaque
	// bootstrap blob (real impl: an Oracle Cloud Agent Run Command that writes the
	// blob and runs the bootstrap hook). The blob is the kubelet join data — never
	// parse it.
	ApplyBootstrap(ctx context.Context, inst ociInstance, clusterID string, bootstrap []byte) error

	// DrainNode cordons and drains the kubelet off a running instance, honouring
	// the grace period, and removes its cluster binding — leaving the instance
	// running but unbound (Idle). Real impl: a Run Command (kubectl cordon/drain).
	DrainNode(ctx context.Context, inst ociInstance, gracePeriodSeconds int64) error
}

// launchSpec is the launch request handed to LaunchInstance.
type launchSpec struct {
	MachineID          string
	Shape              string // OCI shape, e.g. VM.Standard.E5.Flex, BM.Standard.E5.192
	AvailabilityDomain string // OCI AD, e.g. Uocm:EU-FRANKFURT-1-AD-1
	OCPUs              float64
	MemoryGB           float64
	// Preemptible launches the instance as an OCI Preemptible Instance (spot).
	Preemptible bool
	// IdempotencyToken is the kit's per-operation id. The fake uses it to model
	// idempotent launch (a repeated token returns the existing instance); the
	// real client folds it into the display name so a retried launch maps to the
	// same instance.
	IdempotencyToken string
	// BaseUserData is the generic pre-binding cloud-init baked in at launch (first
	// boot only). The cluster-specific bootstrap arrives later via ApplyBootstrap,
	// because cloud-init user_data is consumed only at first boot.
	BaseUserData []byte
}

// ociInstance is the substrate view of one OCI compute instance, free of any
// oci-go-sdk types so the backend never sees the SDK.
type ociInstance struct {
	InstanceID         string // instance OCID
	MachineID          string // bigfleet-machine-id freeform tag
	Shape              string
	AvailabilityDomain string
	OCPUs              float64 // launch ShapeConfig (flexible shapes)
	MemoryGB           float64
	Preemptible        bool   // launched as an OCI Preemptible Instance
	ClusterID          string // bigfleet-cluster freeform tag, empty when unbound
	PrivateIP          string // for Run Command targeting / diagnostics
	// Running reports whether the instance is in a live state (provisioning /
	// starting / running), as opposed to stopping / stopped / terminated.
	Running bool
}
