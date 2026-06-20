package main

import "context"

// doClient is the entire DigitalOcean substrate surface the [digitaloceanBackend]
// drives. It is deliberately tiny and substrate-shaped (not BigFleet-shaped) —
// providerkit owns every cross-cutting concern (fencing, idempotency, async
// dispatch, transition timeouts, shard_metadata, field-shape), so this is the
// only place DigitalOcean appears.
//
// Two implementations ship:
//   - doReal (doreal.go) wraps godo + the on-host agent control channel — the
//     production client.
//   - doFake (dofake.go) is an in-memory simulator that backs unit tests and
//     credential-free conformance / certification runs.
//
// Every method is scoped to one DigitalOcean region, fixed at construction (one
// provider process per region, per the author guide).
type doClient interface {
	// CreateDroplet launches exactly one Droplet and returns its substrate
	// identity. It tags the Droplet with the BigFleet machine id so
	// DescribeManaged can recover inventory after a restart, and bakes the
	// generic, pre-binding agent bootstrap into the Droplet's user_data (which
	// is read-only after first boot — the cluster-specific blob is delivered
	// later by ApplyBootstrap).
	CreateDroplet(ctx context.Context, spec dropletSpec) (dropletInstance, error)

	// DeleteDroplet deletes the Droplet with the given numeric id. The slot
	// returns to Speculative.
	DeleteDroplet(ctx context.Context, dropletID string) error

	// DescribeManaged returns every BigFleet-managed Droplet in the region
	// (Droplets carrying the bigfleet-managed tag), so a provider with no
	// persisted store can still rebuild inventory.
	DescribeManaged(ctx context.Context) ([]dropletInstance, error)

	// ApplyBootstrap binds a running Droplet to a cluster and delivers the
	// opaque bootstrap blob to it over the on-host agent's
	// mutually-authenticated TLS channel (NOT via user_data, which is read-only
	// after first boot). The blob is the kubelet join data — never parse it.
	ApplyBootstrap(ctx context.Context, drv dropletInstance, clusterID string, bootstrap []byte) error

	// DrainNode cordons and drains the kubelet off a running Droplet, honouring
	// the grace period, and removes its cluster binding — leaving the Droplet
	// running but unbound (Idle). Driven over the same agent control channel.
	DrainNode(ctx context.Context, drv dropletInstance, gracePeriodSeconds int64) error

	// PriceUSD returns the published on-demand hourly price for a size slug, in
	// USD/hour. DigitalOcean prices a size identically across regions, so no
	// region argument is needed.
	PriceUSD(ctx context.Context, sizeSlug string) (float64, error)

	// DescribeSizeCapacities resolves the hardware capacity (vCPU + memory) of
	// the given size slugs via the DigitalOcean Sizes API, for
	// Machine.allocatable. Slugs DigitalOcean does not return are simply absent
	// from the result (the caller falls back to the pinned table).
	DescribeSizeCapacities(ctx context.Context, sizeSlugs []string) (map[string]sizeCapacity, error)
}

// dropletSpec is the launch request handed to CreateDroplet.
type dropletSpec struct {
	MachineID string
	Size      string // DigitalOcean size slug, e.g. s-2vcpu-4gb, c-4
	Region    string // DigitalOcean region slug, e.g. nyc3, sfo3, ams3
	Image     string // base image / snapshot slug or id (ships the on-host agent)
	// IdempotencyToken is the kit's per-operation id. The fake uses it to model
	// idempotent create (a repeated token returns the existing Droplet rather
	// than launching a second one); the real client folds it into the Droplet
	// name so a retried launch maps to the same Droplet.
	IdempotencyToken string
	// BaseUserData is the generic, cluster-agnostic pre-binding bootstrap baked
	// in at launch. The cluster-specific bootstrap arrives later over the agent
	// channel, because a Droplet's user_data is read-only after first boot.
	BaseUserData []byte
}

// dropletInstance is the substrate view of one DigitalOcean Droplet, free of any
// godo types so the backend never sees the SDK.
type dropletInstance struct {
	DropletID  string // DigitalOcean numeric Droplet id, as a string
	MachineID  string // bigfleet-machine-id tag
	Size       string
	Region     string
	PublicIPv4 string // for the agent control channel
	ClusterID  string // bigfleet-cluster tag, empty when unbound
	// Active reports whether the Droplet is in a live state (new / active), as
	// opposed to off / archive / deleting.
	Active bool
}

// sizeCapacity is the real hardware capacity of a DigitalOcean size, used to
// populate Machine.allocatable. Memory is held in MiB — so a size whose memory
// is not a whole GiB resolves exactly instead of truncating to 0 GiB.
type sizeCapacity struct {
	VCPU   int
	MemMiB int64
}
