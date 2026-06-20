package main

import "context"

// hcloudClient is the entire Hetzner Cloud substrate surface the
// [hetznerBackend] drives. It is deliberately tiny and substrate-shaped (not
// BigFleet-shaped) — providerkit owns every cross-cutting concern, so this is
// the only place Hetzner appears.
//
// Two implementations ship:
//   - hcloudReal (hcloudreal.go) wraps hcloud-go + SSH — the production client.
//   - hcloudFake (hcloudfake.go) is an in-memory simulator that backs unit
//     tests and credential-free conformance runs.
//
// Every method is scoped to one Hetzner Cloud project, fixed at construction
// (one provider process per project/region, per the author guide).
type hcloudClient interface {
	// CreateServer launches exactly one server and returns its substrate
	// identity. It labels the server with the BigFleet machine id so
	// DescribeManaged can recover inventory after a restart.
	CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error)

	// DeleteServer deletes the server with the given Hetzner server id. The slot
	// returns to Speculative.
	DeleteServer(ctx context.Context, serverID string) error

	// DescribeManaged returns every BigFleet-managed server in the project
	// (servers carrying the bigfleet-managed=true label), so a provider with no
	// persisted store can still rebuild inventory.
	DescribeManaged(ctx context.Context) ([]serverInstance, error)

	// ApplyBootstrap binds a running server to a cluster and delivers the opaque
	// bootstrap blob (real impl: SSH to the host, write the blob, run the
	// bootstrap hook + a bigfleet-cluster label). The blob is the kubelet join
	// data — never parse it.
	ApplyBootstrap(ctx context.Context, srv serverInstance, clusterID string, bootstrap []byte) error

	// DrainNode cordons and drains the kubelet off a running server, honouring
	// the grace period, and removes its cluster binding — leaving the server
	// running but unbound (Idle). Real impl: SSH (kubectl cordon/drain).
	DrainNode(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error

	// PriceUSD returns the most recent on-demand hourly price for serverType in
	// the given location, in USD/hour (converted from Hetzner's EUR pricing).
	PriceUSD(ctx context.Context, serverType, location string) (float64, error)

	// DescribeServerTypeCapacities resolves the hardware capacity (vCPU + memory)
	// of the given server types via the Hetzner ServerType API, for
	// Machine.allocatable. Types Hetzner does not return are simply absent from
	// the result (the caller falls back to the pinned table).
	DescribeServerTypeCapacities(ctx context.Context, serverTypes []string) (map[string]serverCapacity, error)
}

// serverSpec is the launch request handed to CreateServer.
type serverSpec struct {
	MachineID  string
	ServerType string // Hetzner server type name, e.g. cx22, cpx41, ccx33
	Location   string // Hetzner location, e.g. nbg1, fsn1, hel1, ash, hil
	Image      string // base OS / pre-baked image name or id
	// IdempotencyToken is the kit's per-operation id. The fake uses it to model
	// idempotent create (a repeated token returns the existing server rather
	// than launching a second one); the real client folds it into the server
	// name so a retried launch maps to the same server.
	IdempotencyToken string
	// BaseUserData is the pre-binding bootstrap baked in at launch (a generic,
	// cluster-agnostic node bootstrap). The cluster-specific bootstrap arrives
	// later via ApplyBootstrap, because Hetzner Cloud user-data is immutable
	// post-launch.
	BaseUserData []byte
}

// serverInstance is the substrate view of one Hetzner Cloud server, free of any
// hcloud-go types so the backend never sees the SDK.
type serverInstance struct {
	ServerID   string // Hetzner numeric server id, as a string
	MachineID  string // bigfleet-machine-id label
	ServerType string
	Location   string
	PublicIPv4 string // for SSH-based Configure/Drain
	ClusterID  string // bigfleet-cluster label, empty when unbound
	HostKeyFP  string // pinned SSH host-key fingerprint (bigfleet-host-key-fp label)
	// Running reports whether the server is in a live state (initializing /
	// running / starting), as opposed to off / deleting.
	Running bool
}

// serverCapacity is the real hardware capacity of a Hetzner server type, used
// to populate Machine.allocatable. Memory is held in MiB — so a type whose
// memory is not a whole GiB resolves exactly instead of truncating to 0 GiB.
type serverCapacity struct {
	VCPU   int
	MemMiB int64
}
