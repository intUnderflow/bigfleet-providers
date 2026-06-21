package main

import "context"

// upcloudClient is the entire UpCloud substrate surface the [upcloudBackend]
// drives. It is deliberately tiny and substrate-shaped (not BigFleet-shaped) —
// providerkit owns every cross-cutting concern (fencing, idempotency, async
// dispatch, transition timeouts, shard_metadata, field-shape), so this is the
// only place UpCloud appears.
//
// Two implementations ship:
//   - upcloudReal (upcloudreal.go) wraps upcloud-go-api/v8 + SSH — the
//     production client.
//   - upcloudFake (upcloudfake.go) is an in-memory simulator that backs unit
//     tests and credential-free conformance / certification runs.
//
// Every method is scoped to one UpCloud zone, fixed at construction (one
// provider process per zone, per the author guide).
type upcloudClient interface {
	// CreateServer launches exactly one cloud server and returns its substrate
	// identity. It labels the server with the BigFleet machine id so
	// DescribeManaged can recover inventory after a restart, mints + pins an SSH
	// host key (verified on every later Configure/Drain), and bakes the generic,
	// pre-binding agent user-data into the server (which is read-only after first
	// boot — the cluster-specific blob is delivered later by ApplyBootstrap over
	// the verified SSH channel).
	CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error)

	// DeleteServer stops the server (UpCloud refuses to delete a running one)
	// and deletes it AND its attached storage in one shot
	// (DeleteServerAndStorages) — UpCloud storage is a SEPARATE billable
	// resource, so deleting only the server leaks the disk. The slot returns to
	// Speculative. Idempotent: an already-gone server/storage is treated as
	// success.
	DeleteServer(ctx context.Context, uuid string) error

	// DescribeManaged returns every BigFleet-managed server in the zone (servers
	// carrying the bigfleet-managed=true label), running or stopped, so a
	// provider with no persisted store can still rebuild inventory. A
	// tagged-but-stopped server still owns its slot.
	DescribeManaged(ctx context.Context) ([]serverInstance, error)

	// EnsureRunning powers a stopped server back on and waits for it to reach
	// 'started', returning the refreshed substrate view. A tracked server can be
	// stopped out-of-band (operator action, a billing event, a crash), so
	// Configure and Drain MUST call this BEFORE doing their SSH work — otherwise
	// the transition hangs against a stopped host until its timeout fires and the
	// machine lands in FAILED. A no-op for an already-started server.
	EnsureRunning(ctx context.Context, srv serverInstance) (serverInstance, error)

	// ApplyBootstrap binds a running server to a cluster and delivers the opaque
	// bootstrap blob (real impl: SSH to the host with the host key VERIFIED
	// against the fingerprint pinned at Create, write the blob, run the bootstrap
	// hook + set a bigfleet-cluster label). The blob is the kubelet join data —
	// never parse it. The caller has already ensured the server is running.
	ApplyBootstrap(ctx context.Context, srv serverInstance, clusterID string, bootstrap []byte) error

	// DrainNode cordons and drains the kubelet off a running server, honouring
	// the grace period, and removes its cluster binding — leaving the server
	// running but unbound (Idle). Real impl: SSH (kubectl cordon/drain) over the
	// same verified-host-key channel. The caller has already ensured the server
	// is running.
	DrainNode(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error

	// DescribePlanCapacities resolves the hardware capacity (cores + memory) of
	// the given UpCloud plan names via the Plans API, for Machine.allocatable.
	// Plans UpCloud does not return are simply absent from the result (the caller
	// falls back to the pinned table).
	DescribePlanCapacities(ctx context.Context, plans []string) (map[string]planCapacity, error)
}

// serverSpec is the launch request handed to CreateServer.
type serverSpec struct {
	MachineID string
	Plan      string // UpCloud plan name, e.g. 1xCPU-1GB, 2xCPU-4GB, DEV-1xCPU-1GB
	Zone      string // UpCloud zone id, e.g. fi-hel1, de-fra1, uk-lon1
	Template  string // OS template storage UUID to clone (e.g. an Ubuntu 24.04 cloud-init template)
	// IdempotencyToken is the kit's per-operation id. The fake uses it to model
	// idempotent create (a repeated token returns the existing server rather than
	// launching a second one); the real client folds it into the server title /
	// hostname so a retried launch maps to the same server.
	IdempotencyToken string
	// BaseUserData is the generic, cluster-agnostic pre-binding bootstrap baked
	// in at launch (installs the on-host agent / bootstrap hook only — NEVER the
	// secret-bearing blob). The cluster-specific bootstrap arrives later over the
	// verified SSH channel, because UpCloud user-data is consumed by cloud-init
	// only at first boot.
	BaseUserData []byte
}

// serverInstance is the substrate view of one UpCloud cloud server, free of any
// upcloud-go-api types so the backend never sees the SDK.
type serverInstance struct {
	UUID       string // UpCloud server UUID — the HostRef.ref
	MachineID  string // bigfleet-machine-id label
	Plan       string // UpCloud plan name
	Zone       string // UpCloud zone id
	PublicIPv4 string // for SSH-based Configure/Drain
	ClusterID  string // bigfleet-cluster label, empty when unbound
	HostKeyFP  string // pinned SSH host-key fingerprint (bigfleet-host-key-fp label)
	// Running reports whether the server is in the 'started' state (as opposed to
	// stopped / maintenance / error). A stopped server still owns its slot; the
	// next Configure/Drain re-powers it via EnsureRunning.
	Running bool
}

// planCapacity is the real hardware capacity of an UpCloud plan, used to
// populate Machine.allocatable. Memory is held in MiB — so a plan whose memory
// is not a whole GiB resolves exactly instead of truncating to 0 GiB.
type planCapacity struct {
	Cores  int
	MemMiB int64
}
