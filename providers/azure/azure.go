package main

import "context"

// azureClient is the entire Azure substrate surface the [azureBackend] drives.
// It is deliberately tiny and substrate-shaped (not BigFleet-shaped) —
// providerkit owns every cross-cutting concern, so this is the only place Azure
// appears.
//
// Two implementations ship:
//   - azureReal (azurereal.go) wraps azure-sdk-for-go (armcompute + armnetwork +
//     azidentity) — the production client.
//   - azureFake (azurefake.go) is an in-memory simulator that backs unit tests
//     and credential-free conformance runs.
//
// Every method is scoped to one Azure subscription + resource group + location,
// fixed at construction (one provider process per region, per the author guide).
type azureClient interface {
	// CreateVM provisions exactly one Virtual Machine (and its NIC) and returns
	// its substrate identity. It tags the VM with the BigFleet machine id so
	// DescribeManaged can recover inventory after a restart.
	CreateVM(ctx context.Context, spec vmSpec) (vmInstance, error)

	// DeleteVM deletes the VM (and its NIC/OS disk) with the given Azure
	// resource id. The slot returns to Speculative.
	DeleteVM(ctx context.Context, resourceID string) error

	// DescribeManaged returns every BigFleet-managed VM in the resource group
	// (VMs carrying the bigfleet-managed=true tag), so a provider with no
	// persisted store can still rebuild inventory.
	DescribeManaged(ctx context.Context) ([]vmInstance, error)

	// ApplyBootstrap binds a running VM to a cluster and delivers the opaque
	// bootstrap blob (real impl: a CustomScript VM extension that writes and runs
	// the blob, plus a bigfleet-cluster tag). The blob is the kubelet join data —
	// never parse it.
	ApplyBootstrap(ctx context.Context, vm vmInstance, clusterID string, bootstrap []byte) error

	// DrainNode cordons and drains the kubelet off a running VM, honouring the
	// grace period, and removes its cluster binding — leaving the VM running but
	// unbound (Idle). Real impl: a CustomScript extension running the drain hook.
	DrainNode(ctx context.Context, vm vmInstance, gracePeriodSeconds int64) error

	// SpotPriceUSD returns the current Spot price for vmSize in the location, in
	// USD/hour (Azure Retail Prices API, Spot meter). Non-Spot callers never hit
	// this.
	SpotPriceUSD(ctx context.Context, vmSize string) (float64, error)

	// DescribeVMSizeCapacities resolves the hardware capacity (vCPU + memory) of
	// the given VM sizes via the Resource SKUs API, for Machine.allocatable.
	// Sizes Azure does not return are simply absent from the result (the caller
	// falls back to the pinned table).
	DescribeVMSizeCapacities(ctx context.Context, vmSizes []string) (map[string]vmCapacity, error)
}

// vmSpec is the launch request handed to CreateVM.
type vmSpec struct {
	MachineID string
	VMSize    string // Azure VM size, e.g. Standard_D4s_v5
	Zone      string // BigFleet zone, e.g. eastus-1 (the bare zone number is derived from it)
	Spot      bool   // launch as a Spot VM (priority=Spot, evictionPolicy=Delete, maxPrice=-1)
	// Capacity is the canonical capacity-type string ("on_demand" | "spot" |
	// "reserved" | "bare_metal"), stamped as a bigfleet-capacity tag so the
	// capacity type is recoverable from Azure alone (DescribeManaged), not just
	// guessed from the VM priority.
	Capacity string
	// IdempotencyToken is the kit's per-operation id, folded into the VM name so a
	// retried create maps to the same VM rather than double-provisioning.
	IdempotencyToken string
	// BaseUserData is the pre-binding bootstrap baked into the VM's customData at
	// create (a generic, cluster-agnostic node bootstrap). The cluster-specific
	// bootstrap arrives later via ApplyBootstrap, because customData is consumed
	// by cloud-init only at first boot.
	BaseUserData []byte
}

// vmInstance is the substrate view of one Azure VM, free of any azure-sdk types
// so the backend never sees the SDK.
type vmInstance struct {
	ResourceID string // /subscriptions/.../virtualMachines/<name>
	Name       string // the VM resource name
	MachineID  string // bigfleet-machine-id tag
	VMSize     string
	Zone       string // BigFleet zone (e.g. eastus-1)
	Spot       bool
	PrivateIP  string
	ClusterID  string // bigfleet-cluster tag, empty when unbound
	Capacity   string // bigfleet-capacity tag (canonical capacity string)
	// Running reports whether the VM is in a live provisioning/power state
	// (creating / running / stopped-but-allocated), as opposed to
	// deleting / deallocated-and-gone.
	Running bool
}

// vmCapacity is the real hardware capacity of an Azure VM size, used to populate
// Machine.allocatable. Memory is held in MiB — so a size whose memory is not a
// whole GiB resolves exactly instead of truncating to 0 GiB.
type vmCapacity struct {
	VCPU   int
	MemMiB int64
}
