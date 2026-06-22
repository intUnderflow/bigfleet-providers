package main

import (
	"context"
	"fmt"
	"sync"
)

// azureFake is an in-memory azureClient. It is NOT a production artifact — it
// backs unit tests and credential-free conformance runs (`--azure-backend=fake`,
// or `auto` with no `--location`). It models just enough Azure behaviour for the
// lifecycle: create returns a synthetic resource id, delete removes it, describe
// lists the live ones, and bind/drain toggle the cluster tag.
type azureFake struct {
	mu      sync.Mutex
	seq     int
	vms     map[string]*vmInstance // keyed by resource id
	byToken map[string]string      // idempotency token -> resource id
	// spotUSD is the deterministic Spot price the simulator reports, so
	// conformance and tests are reproducible.
	spotUSD float64
	// onDemandUSD is the deterministic on-demand price reported for any VM size
	// the seed table does not cover, so the live on-demand refresh path is
	// exercised credential-free and reproducibly.
	onDemandUSD float64
}

func newAzureFake() *azureFake {
	return &azureFake{
		vms:         make(map[string]*vmInstance),
		byToken:     make(map[string]string),
		spotUSD:     0.0412,
		onDemandUSD: 0.10,
	}
}

func (f *azureFake) CreateVM(_ context.Context, spec vmSpec) (vmInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Model create idempotency: a repeated token returns the existing VM instead
	// of provisioning a second one.
	if spec.IdempotencyToken != "" {
		if id, ok := f.byToken[spec.IdempotencyToken]; ok {
			if vm, ok := f.vms[id]; ok {
				return *vm, nil
			}
		}
	}
	f.seq++
	name := fmt.Sprintf("bf-fake-%08d", f.seq)
	id := fmt.Sprintf("/subscriptions/fake/resourceGroups/fake/providers/Microsoft.Compute/virtualMachines/%s", name)
	vm := &vmInstance{
		ResourceID: id,
		Name:       name,
		MachineID:  spec.MachineID,
		VMSize:     spec.VMSize,
		Zone:       spec.Zone,
		Spot:       spec.Spot,
		Capacity:   spec.Capacity,
		PrivateIP:  fmt.Sprintf("10.0.0.%d", f.seq%250+1),
		Running:    true,
	}
	f.vms[id] = vm
	if spec.IdempotencyToken != "" {
		f.byToken[spec.IdempotencyToken] = id
	}
	return *vm, nil
}

func (f *azureFake) DeleteVM(_ context.Context, resourceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Idempotent, matching the real client (a 404 on delete is success): deleting
	// an unknown / already-gone VM succeeds, so a Delete after an out-of-band
	// deletion (or a Spot eviction) never spuriously fails the machine.
	delete(f.vms, resourceID)
	return nil
}

func (f *azureFake) DescribeManaged(_ context.Context) ([]vmInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]vmInstance, 0, len(f.vms))
	for _, vm := range f.vms {
		out = append(out, *vm)
	}
	return out, nil
}

func (f *azureFake) StartVM(_ context.Context, resourceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.vms[resourceID]; ok {
		v.Running = true
	}
	return nil
}

func (f *azureFake) ApplyBootstrap(_ context.Context, vm vmInstance, clusterID string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vms[vm.ResourceID]
	if !ok {
		return fmt.Errorf("azurefake: configure unknown vm %q", vm.ResourceID)
	}
	v.ClusterID = clusterID
	return nil
}

func (f *azureFake) DrainNode(_ context.Context, vm vmInstance, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vms[vm.ResourceID]
	if !ok {
		return fmt.Errorf("azurefake: drain unknown vm %q", vm.ResourceID)
	}
	v.ClusterID = ""
	return nil
}

func (f *azureFake) SpotPriceUSD(_ context.Context, _ string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spotUSD, nil
}

// OnDemandPriceUSD reports a deterministic pay-as-you-go price so the live
// on-demand refresh runs credential-free. It mirrors the pinned eastus seed for
// known sizes (keeping fake/conformance pricing stable and size-differentiated)
// and a fixed default for anything else.
func (f *azureFake) OnDemandPriceUSD(_ context.Context, vmSize string) (float64, error) {
	if v, ok := onDemandEastUS[vmSize]; ok {
		return v, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.onDemandUSD, nil
}

// DescribeVMSizeCapacities resolves capacities from the pinned table, so the
// simulator (and credential-free conformance) exercises the resolve path
// deterministically. Sizes absent from the table are omitted, exactly as the
// real Resource SKUs API omits a size unavailable in the region.
func (f *azureFake) DescribeVMSizeCapacities(_ context.Context, vmSizes []string) (map[string]vmCapacity, error) {
	out := make(map[string]vmCapacity, len(vmSizes))
	for _, t := range vmSizes {
		if c, ok := vmSizeTable[t]; ok {
			out[t] = c
		}
	}
	return out, nil
}

var _ azureClient = (*azureFake)(nil)
