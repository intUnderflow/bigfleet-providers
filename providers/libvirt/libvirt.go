package main

import "context"

// libvirtClient is the entire libvirt substrate surface the [libvirtBackend]
// drives. It is deliberately small and substrate-shaped (not BigFleet-shaped) —
// providerkit owns every cross-cutting concern, so this is the only place
// libvirt appears.
//
// Two implementations ship:
//   - libvirtReal (libvirtreal.go) wraps the pure-Go go-libvirt client — the
//     production client, one libvirt connection per host/zone.
//   - libvirtFake (libvirtfake.go) is an in-memory simulator that backs unit
//     tests and credential-free conformance / certification runs.
//
// A "machine" is a libvirt domain (VM). Every method is scoped to the set of
// libvirt hosts this provider process manages (one connection per zone).
type libvirtClient interface {
	// CreateDomain defines and starts exactly one domain on the host named by
	// spec.Zone, building its disk from the base image and attaching an initial
	// (pre-binding) cloud-init NoCloud datasource. It returns the domain's
	// substrate identity. The domain is tagged with the BigFleet machine id (in
	// its libvirt metadata) so DescribeManaged can recover inventory after a
	// restart.
	CreateDomain(ctx context.Context, spec domainSpec) (domainInstance, error)

	// DeleteDomain destroys and undefines the domain (removing managed save /
	// NVRAM) and deletes its overlay disk + cloud-init volume, keeping the
	// golden base image. The slot returns to Speculative. Idempotent: deleting
	// an already-gone domain succeeds.
	DeleteDomain(ctx context.Context, zone, domainName string) error

	// DescribeManaged returns every BigFleet-managed domain across all hosts
	// (domains carrying the bigfleet metadata), so a provider with no persisted
	// store can still rebuild inventory.
	DescribeManaged(ctx context.Context) ([]domainInstance, error)

	// EnsureRunning powers a domain on if it is not already running and returns
	// once it is active, so Configure/Drain never drive the guest agent against a
	// stopped host. A domain that went Idle then shut off out of band (a guest
	// poweroff trips <on_poweroff>destroy</on_poweroff>; autostart only fires on
	// host/libvirtd boot) would otherwise loop on guest-ping until the transition
	// times out and FAILs, while its disk keeps billing. Idempotent: a domain that
	// is already running is a no-op.
	EnsureRunning(ctx context.Context, dom domainInstance) error

	// ApplyBootstrap binds a running domain to a cluster and delivers the opaque
	// bootstrap blob by writing it into the guest and running the in-image
	// bootstrap hook via the qemu guest agent (guest-exec), waiting for the hook
	// to complete. The blob is the kubelet join data — never parse it.
	ApplyBootstrap(ctx context.Context, dom domainInstance, clusterID string, bootstrap []byte) error

	// DrainNode gracefully drains the kubelet off a running domain, honouring the
	// grace period, and clears its cluster binding — leaving the domain running
	// but unbound (Idle).
	DrainNode(ctx context.Context, dom domainInstance, gracePeriodSeconds int64) error

	// Close releases every libvirt connection. Called once at shutdown.
	Close() error
}

// domainSpec is the launch request handed to CreateDomain.
type domainSpec struct {
	MachineID    string
	InstanceType string // catalog name, e.g. kvm.small
	Zone         string // the libvirt host to place the domain on
	VCPUs        int    // resolved from the instance-type catalog
	MemoryMiB    int64  // resolved from the instance-type catalog
	// IdempotencyToken is the kit's per-operation id. The real client folds it
	// into the domain name so a retried launch maps to the same domain; the fake
	// uses it to model idempotent create.
	IdempotencyToken string
	// BaseUserData is the pre-binding cloud-init user-data baked into the initial
	// NoCloud datasource at define time (a generic, cluster-agnostic node
	// bootstrap). The cluster-specific bootstrap arrives later via ApplyBootstrap.
	BaseUserData []byte
}

// domainInstance is the substrate view of one libvirt domain, free of any
// go-libvirt types so the backend never sees the SDK.
type domainInstance struct {
	Zone       string // the libvirt host the domain lives on
	DomainName string // libvirt domain name
	UUID       string // libvirt domain UUID (stable identity)
	MachineID  string // bigfleet machine id, from domain metadata
	ClusterID  string // bigfleet cluster binding, empty when unbound
	// Running reports whether the domain is in a live state (running / booting),
	// as opposed to shut off / undefined.
	Running bool
}

// hostRef is the substrate host id stamped on Machine.host.Ref: "<zone>/<domain>".
func (d domainInstance) hostRef() string { return d.Zone + "/" + d.DomainName }
