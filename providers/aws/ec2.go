package main

import "context"

// ec2Client is the entire AWS substrate surface the [awsBackend] drives. It is
// deliberately tiny and substrate-shaped (not BigFleet-shaped) — providerkit
// owns every cross-cutting concern, so this is the only place AWS appears.
//
// Two implementations ship:
//   - ec2Real (ec2real.go) wraps aws-sdk-go-v2 — the production client.
//   - ec2Fake (ec2fake.go) is an in-memory simulator that backs unit tests and
//     credential-free conformance runs.
//
// Every method is scoped to one AWS region, fixed at construction (one
// provider process per region, per the author guide).
type ec2Client interface {
	// RunInstance launches exactly one instance (RunInstances, Min=Max=1) and
	// returns its substrate identity. It tags the instance with the BigFleet
	// machine id so DescribeManaged can recover inventory after a restart.
	RunInstance(ctx context.Context, spec runSpec) (ec2Instance, error)

	// TerminateInstance terminates the instance with the given EC2 id
	// (TerminateInstances). The slot returns to Speculative.
	TerminateInstance(ctx context.Context, instanceID string) error

	// DescribeManaged returns every BigFleet-managed instance in the region
	// (DescribeInstances filtered by the bigfleet:managed=true tag), so a
	// provider with no persisted store can still rebuild inventory.
	DescribeManaged(ctx context.Context) ([]ec2Instance, error)

	// ApplyBootstrap binds a running instance to a cluster and delivers the
	// opaque bootstrap blob (real impl: SSM SendCommand + a bigfleet:cluster
	// tag). The blob is the kubelet join data — never parse it.
	ApplyBootstrap(ctx context.Context, instanceID, clusterID string, bootstrap []byte) error

	// DrainNode cordons and drains the kubelet off a running instance,
	// honouring the grace period, and removes its cluster binding — leaving
	// the instance running but unbound (Idle). Real impl: SSM SendCommand.
	DrainNode(ctx context.Context, instanceID string, gracePeriodSeconds int64) error

	// SpotPriceUSD returns the most recent spot price for (instanceType, zone)
	// in USD/hour (DescribeSpotPriceHistory, newest entry).
	SpotPriceUSD(ctx context.Context, instanceType, zone string) (float64, error)

	// DescribeInstanceCapacities resolves the hardware capacity (vCPU + memory)
	// of the given instance types via ec2:DescribeInstanceTypes, for
	// Machine.allocatable. Types AWS does not return are simply absent from the
	// result (the caller falls back to the pinned table).
	DescribeInstanceCapacities(ctx context.Context, instanceTypes []string) (map[string]instanceCapacity, error)
}

// runSpec is the launch request handed to RunInstance.
type runSpec struct {
	MachineID    string
	InstanceType string
	Zone         string
	Spot         bool
	// Capacity is the canonical capacity-type string ("on_demand" | "spot" |
	// "reserved" | "bare_metal"), stamped as a bigfleet:capacity tag so the
	// capacity type is recoverable from EC2 alone (DescribeManaged), not just
	// guessed from the spot lifecycle.
	Capacity string
	// IdempotencyToken is the kit's per-operation id, used as the EC2
	// RunInstances ClientToken so a retried launch can't double-provision.
	IdempotencyToken string
	// BaseUserData is the pre-binding bootstrap baked in at launch (a generic,
	// cluster-agnostic node bootstrap). The cluster-specific bootstrap arrives
	// later via ApplyBootstrap, because EC2 user-data is immutable post-launch.
	BaseUserData []byte
}

// ec2Instance is the substrate view of one EC2 instance, free of any aws-sdk
// types so the backend never sees the SDK.
type ec2Instance struct {
	InstanceID   string // i-0abc…
	MachineID    string // bigfleet:machine-id tag
	InstanceType string
	Zone         string
	Spot         bool
	PrivateDNS   string
	ClusterID    string // bigfleet:cluster tag, empty when unbound
	Capacity     string // bigfleet:capacity tag (canonical capacity string)
	// Running reports whether the instance is in a live state (pending /
	// running), as opposed to stopping / stopped / shutting-down / terminated.
	Running bool
}
