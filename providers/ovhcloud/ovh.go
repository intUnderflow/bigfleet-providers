package main

import "context"

// ovhClient is the entire OVHcloud Public Cloud (OpenStack) substrate surface
// the [ovhBackend] drives. It is deliberately tiny and substrate-shaped (not
// BigFleet-shaped) — providerkit owns every cross-cutting concern, so this is
// the only place OpenStack appears.
//
// Two implementations ship:
//   - ovhReal (openstack.go) wraps gophercloud/v2 + SSH — the production client.
//   - ovhFake (ovhfake.go) is an in-memory simulator that backs unit tests and
//     credential-free conformance runs.
//
// Every method is scoped to one OVHcloud Public Cloud project in one OpenStack
// region, fixed at construction (one provider process per region, per the
// author guide).
type ovhClient interface {
	// CreateServer launches exactly one Nova server and returns its substrate
	// identity. It stamps the server's metadata with the BigFleet machine id so
	// DescribeManaged can recover inventory after a restart.
	CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error)

	// DeleteServer deletes the server with the given OpenStack server UUID. The
	// slot returns to Speculative.
	DeleteServer(ctx context.Context, serverID string) error

	// StartServer powers on a SHUTOFF/stopped server and returns once it is
	// running again. Used to heal a recovered-but-powered-off instance during
	// Create, so a stopped server is never bound (Configure'd) while down.
	StartServer(ctx context.Context, serverID string) error

	// DescribeManaged returns every BigFleet-managed server in the project
	// (servers carrying the bigfleet-managed=true metadata), so a provider with
	// no persisted store can still rebuild inventory.
	DescribeManaged(ctx context.Context) ([]serverInstance, error)

	// ApplyBootstrap binds a running server to a cluster and delivers the opaque
	// bootstrap blob (real impl: SSH to the host, write the blob, run the
	// bootstrap hook + a bigfleet-cluster metadata key). The blob is the kubelet
	// join data — never parse it.
	ApplyBootstrap(ctx context.Context, srv serverInstance, clusterID string, bootstrap []byte) error

	// DrainNode cordons and drains the kubelet off a running server, honouring
	// the grace period, and removes its cluster binding — leaving the server
	// running but unbound (Idle). Real impl: SSH (kubectl cordon/drain).
	DrainNode(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error

	// DescribeFlavorCapacities resolves the hardware capacity (vCPU + memory) of
	// the given OpenStack flavors via the Nova flavors API, for
	// Machine.allocatable. Flavors OpenStack does not return are simply absent
	// from the result (the caller falls back to the pinned table).
	DescribeFlavorCapacities(ctx context.Context, flavors []string) (map[string]flavorCapacity, error)
}

// serverSpec is the launch request handed to CreateServer.
type serverSpec struct {
	MachineID string
	Flavor    string // OpenStack flavor name, e.g. b2-7, c2-15
	Region    string // OVH region, e.g. GRA, SBG, BHS (informational; the client is region-scoped)
	Image     string // base OS / pre-baked image name or id
	// IdempotencyToken is the kit's per-operation id. The fake uses it to model
	// idempotent create (a repeated token returns the existing server rather
	// than launching a second one); the real client folds it into the server
	// name so a retried launch maps to the same server.
	IdempotencyToken string
	// BaseUserData is the pre-binding bootstrap baked in at launch (a generic,
	// cluster-agnostic node bootstrap). The cluster-specific bootstrap arrives
	// later via ApplyBootstrap, because OpenStack user_data is consumed by
	// cloud-init only at first boot and cannot re-bootstrap a running instance.
	BaseUserData []byte
}

// serverInstance is the substrate view of one OpenStack server, free of any
// gophercloud types so the backend never sees the SDK.
type serverInstance struct {
	ServerID   string // OpenStack server UUID
	MachineID  string // bigfleet-machine-id metadata
	Flavor     string
	Region     string
	PublicIPv4 string // SSH target for Configure/Drain: floating address if present, else the fixed (private) address
	ClusterID  string // bigfleet-cluster metadata, empty when unbound
	HostKeyFP  string // pinned SSH host-key fingerprint (bigfleet-host-key-fp metadata)
	// Running reports whether the server is in a live state (ACTIVE / BUILD),
	// as opposed to SHUTOFF / DELETED / ERROR.
	Running bool
}

// flavorCapacity is the real hardware capacity of an OpenStack flavor, used to
// populate Machine.allocatable. Memory is held in MiB — so a flavor whose
// memory is not a whole GiB resolves exactly instead of truncating to 0 GiB.
type flavorCapacity struct {
	VCPU   int
	MemMiB int64
}
