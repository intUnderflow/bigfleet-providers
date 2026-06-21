package main

import "context"

// latitudeClient is the entire Latitude.sh substrate surface the
// [latitudeBackend] drives. It is deliberately small and substrate-shaped (not
// BigFleet-shaped) — providerkit owns every cross-cutting concern, so this is
// the only place Latitude.sh appears.
//
// Two implementations ship:
//   - latitudeReal (latitudereal.go) wraps latitudesh-go-sdk + SSH — the
//     production client.
//   - latitudeFake (latitudefake.go) is an in-memory simulator that backs unit
//     tests and credential-free conformance runs.
//
// Every method is scoped to one Latitude.sh project, fixed at construction (one
// provider process per site/region, per the author guide).
//
// Latitude.sh is an on-demand bare-metal cloud: CreateServer deploys a real
// physical server (minutes, not seconds) and DeleteServer deprovisions it. A
// tagged server can also be powered OFF out-of-band, so Configure/Drain must
// EnsureRunning (PowerOn + wait) before they touch it — GetServer/PowerOn exist
// for exactly that.
type latitudeClient interface {
	// CreateServer deploys exactly one bare-metal server and returns its
	// substrate identity. It tags the server with the BigFleet machine id so
	// DescribeManaged can recover inventory after a restart. The deploy is
	// substrate-idempotent on spec.IdempotencyToken: a retried create with the
	// same token must adopt the existing server, never deploy a second one.
	CreateServer(ctx context.Context, spec serverSpec) (serverInstance, error)

	// DeleteServer deprovisions the server with the given Latitude server id, and
	// any resources this provider attached to it (idempotent: an already-gone
	// server is success). The slot returns to Speculative.
	DeleteServer(ctx context.Context, serverID string) error

	// DescribeManaged returns every BigFleet-managed server in the project
	// (servers carrying the bigfleet-managed tag), so a provider with no
	// persisted store can still rebuild inventory.
	DescribeManaged(ctx context.Context) ([]serverInstance, error)

	// GetServer returns the current substrate view of one server by id (power
	// state, primary IPv4, tags). Used by EnsureRunning and resolveHost.
	GetServer(ctx context.Context, serverID string) (serverInstance, error)

	// PowerOn powers a server on (idempotent: a server already on is success).
	// EnsureRunning calls it, then polls GetServer until the server reports
	// powered on.
	PowerOn(ctx context.Context, serverID string) error

	// ApplyBootstrap binds an already-running server to a cluster and delivers
	// the opaque bootstrap blob (real impl: SSH to the host on the pinned host
	// key, write the blob, run the bootstrap hook + a bigfleet-cluster tag). The
	// blob is the kubelet join data — never parse it.
	ApplyBootstrap(ctx context.Context, srv serverInstance, clusterID string, bootstrap []byte) error

	// DrainNode cordons and drains the kubelet off an already-running server,
	// honouring the grace period, and removes its cluster binding — leaving the
	// server running but unbound (Idle). Real impl: SSH (kubectl cordon/drain).
	DrainNode(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error

	// PriceUSD returns the most recent on-demand hourly price for plan in the
	// given site, in USD/hour (Latitude publishes prices in USD directly).
	PriceUSD(ctx context.Context, plan, site string) (float64, error)

	// DescribePlanCapacities resolves the hardware capacity (vCPU + memory) of
	// the given plan slugs via the Plans API, for Machine.allocatable. Plans
	// Latitude does not return are simply absent from the result (the caller
	// falls back to the pinned table).
	DescribePlanCapacities(ctx context.Context, plans []string) (map[string]planCapacity, error)
}

// serverSpec is the deploy request handed to CreateServer.
type serverSpec struct {
	MachineID string
	Plan      string // Latitude plan slug, e.g. c2-small-x86, g3-xlarge-x86
	Site      string // Latitude site slug, e.g. ASH, NYC, LON, FRA
	// OperatingSystem is the OS slug deployed at first boot, e.g.
	// ubuntu_22_04_x64_lts.
	OperatingSystem string
	// IdempotencyToken is the kit's per-operation id. The fake uses it to model
	// idempotent deploy (a repeated token returns the existing server rather than
	// deploying a second one); the real client folds it into a deploy tag /
	// hostname so a retried deploy adopts the same server.
	IdempotencyToken string
	// BaseUserData is the generic, cluster-agnostic pre-binding bootstrap baked
	// in at deploy as Latitude UserData. The cluster-specific bootstrap (which
	// carries the JOIN SECRET) is delivered later via ApplyBootstrap over the
	// pinned-host-key SSH channel, NEVER via user_data (first-boot-only, stored
	// by Latitude).
	BaseUserData []byte
}

// serverInstance is the substrate view of one Latitude.sh server, free of any
// SDK types so the backend never sees the generated client.
type serverInstance struct {
	ServerID   string // Latitude server id, e.g. sv_W6Q2D9xGqKLpr
	MachineID  string // bigfleet-machine-id tag
	Plan       string // plan slug -> Machine.instance_type
	Site       string // site slug -> Machine.zone
	PublicIPv4 string // for SSH-based Configure/Drain
	ClusterID  string // bigfleet-cluster tag, empty when unbound
	HostKeyFP  string // pinned SSH host-key fingerprint (bigfleet-host-key-fp tag)
	// Running reports whether the server exists in a live (non-deleting) state.
	Running bool
	// PoweredOn reports whether the server is powered on AND reachable. A tagged
	// server the kit tracks may be powered off out-of-band; Configure/Drain
	// EnsureRunning before they touch it.
	PoweredOn bool
}

// planCapacity is the real hardware capacity of a Latitude plan, used to
// populate Machine.allocatable. Memory is held in MiB so a plan whose memory is
// not a whole GiB resolves exactly instead of truncating.
type planCapacity struct {
	VCPU   int
	MemMiB int64
}
