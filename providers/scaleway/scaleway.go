package main

import "context"

// scwClient is the entire Scaleway substrate surface the [scalewayBackend]
// drives. It is deliberately small and substrate-shaped (not BigFleet-shaped):
// providerkit owns every cross-cutting concern (fencing, idempotency, async
// dispatch, transition timeouts, shard_metadata, field-shape), so this is the
// only place Scaleway appears.
//
// One client serves one substrate (Instances OR Elastic Metal) in one
// zone/region, fixed at construction — one provider process per
// region/backend pair, per the author guide. Three implementations ship:
//
//   - scwInstances (scwinstances.go) wraps the Instances API — the primary,
//     cloud (ON_DEMAND) path: a deletable VM.
//   - scwBaremetal (scwbaremetal.go) wraps the Elastic Metal / baremetal API —
//     the BARE_METAL path: a free-pool server that is never torn down.
//   - scwFake (scwfake.go) is an in-memory simulator that backs unit tests and
//     credential-free certification runs.
type scwClient interface {
	// CreateServer provisions exactly one server and returns its substrate
	// identity. It tags the server with the BigFleet machine id so
	// DescribeManaged can recover inventory after a restart. For the Instances
	// backend this is CreateServer + poweron + wait-for-running; for Elastic
	// Metal it is CreateServer + InstallServer + wait-for-install.
	CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error)

	// DeleteServer tears down the server with the given Scaleway server id. Only
	// the Instances backend implements a meaningful teardown; the Elastic Metal
	// backend's DeleteServer is never invoked (the kit answers Delete with
	// codes.Unimplemented because that backend type omits Deleter).
	DeleteServer(ctx context.Context, serverID string) error

	// DescribeManaged returns every BigFleet-managed server on this substrate
	// (servers carrying the bigfleet-managed=true tag), so a provider with no
	// persisted store can still rebuild inventory.
	DescribeManaged(ctx context.Context) ([]serverInstance, error)

	// ApplyBootstrap binds a running server to a cluster and delivers the opaque
	// bootstrap blob. The blob is the kubelet join data — never parse it. The
	// real Instances client publishes the blob for the on-host agent to fetch
	// over a mutually-authenticated TLS channel; the Elastic Metal client
	// supplies it as install-time user-data.
	ApplyBootstrap(ctx context.Context, srv serverInstance, clusterID string, bootstrap []byte) error

	// DrainNode cordons and drains the kubelet off a running server, honouring
	// the grace period, and removes its cluster binding — leaving the server
	// running but unbound (Idle).
	DrainNode(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error

	// PriceUSD returns the most recent hourly price for commercialType in this
	// zone, in USD/hour (converted from Scaleway's EUR pricing). The Elastic
	// Metal client returns 0 (owned hardware, already paid for).
	PriceUSD(ctx context.Context, commercialType, zone string) (float64, error)

	// DescribeCommercialTypeCapacities resolves the hardware capacity (vCPU +
	// memory, and GPUs where applicable) of the given commercial types for
	// Machine.allocatable. Types the substrate does not return are simply absent
	// from the result (the caller falls back to the pinned table).
	DescribeCommercialTypeCapacities(ctx context.Context, commercialTypes []string) (map[string]commercialCapacity, error)
}

// serverSpec is the launch request handed to CreateServer.
type serverSpec struct {
	MachineID      string
	CommercialType string // Scaleway commercial type, e.g. DEV1-S, GP1-XS, EM-A210R-HDD
	Zone           string // Scaleway zone, e.g. fr-par-1, nl-ams-1
	Image          string // base image / OS label or id
	// IdempotencyToken is the kit's per-operation id. The fake uses it to model
	// idempotent create (a repeated token returns the existing server rather
	// than launching a second one); the real clients fold it into the server
	// name so a retried launch maps to the same server.
	IdempotencyToken string
	// BaseUserData is the generic pre-binding bootstrap baked in at first boot (a
	// cluster-agnostic node bootstrap that installs the on-host agent). The
	// cluster-specific bootstrap arrives later via ApplyBootstrap, because
	// Scaleway cloud-init user-data is consumed only at first boot.
	BaseUserData []byte
}

// serverInstance is the substrate view of one Scaleway server, free of any
// scaleway-sdk-go types so the backend never sees the SDK.
type serverInstance struct {
	ServerID       string // Scaleway server UUID
	MachineID      string // bigfleet-machine-id tag
	CommercialType string
	Zone           string
	PublicIPv4     string // for the agent-fetch / drain control channel
	ClusterID      string // bigfleet-cluster tag, empty when unbound
	// Running reports whether the server is in a live state (starting /
	// running), as opposed to stopped / deleting / installing-failed.
	Running bool
}

// commercialCapacity is the real hardware capacity of a Scaleway commercial
// type, used to populate Machine.allocatable. Memory is held in MiB so a type
// whose memory is not a whole GiB resolves exactly instead of truncating; GPUs
// is the nvidia.com/gpu count (0 for non-GPU types).
type commercialCapacity struct {
	VCPU   int
	MemMiB int64
	GPUs   int
}
