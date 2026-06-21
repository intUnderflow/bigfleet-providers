package main

import (
	"context"
	"fmt"
	"sync"
)

// doFake is an in-memory doClient. It is NOT a production artifact — it backs
// unit tests and credential-free conformance / certification runs
// (--do-backend=fake, or `auto` with no token). It models just enough
// DigitalOcean behaviour for the lifecycle: create returns a synthetic Droplet
// id, delete removes it, describe lists the live ones, and bind/drain toggle the
// cluster tag.
type doFake struct {
	mu       sync.Mutex
	seq      int
	droplets map[string]*dropletInstance // keyed by droplet id
	names    map[string]string           // droplet id -> derived name
	// priceUSD is the deterministic hourly price the simulator reports, so
	// conformance and tests are reproducible.
	priceUSD float64
}

func newDOFake() *doFake {
	return &doFake{
		droplets: make(map[string]*dropletInstance),
		names:    make(map[string]string),
		priceUSD: 0.03571,
	}
}

func (f *doFake) CreateDroplet(_ context.Context, spec dropletSpec) (dropletInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Model the substrate faithfully: the DigitalOcean API has no client
	// idempotency token and allows duplicate names, so idempotency must come from
	// the client doing a PRE-CREATE LOOKUP — the same mechanism doReal uses. Reuse
	// an existing managed Droplet with the same derived name for this machine; only
	// create a new one otherwise. (A magic idempotency-token map here would model a
	// capability the real substrate lacks and hide a double-provision regression.)
	name := dropletName(spec)
	for id, n := range f.names {
		if n != name {
			continue
		}
		if drv, ok := f.droplets[id]; ok && drv.MachineID == spec.MachineID {
			return *drv, nil
		}
	}
	f.seq++
	id := fmt.Sprintf("%d", 300000000+f.seq)
	drv := &dropletInstance{
		DropletID:  id,
		MachineID:  spec.MachineID,
		Size:       spec.Size,
		Region:     spec.Region,
		PublicIPv4: fmt.Sprintf("198.51.100.%d", f.seq%250+1),
		Active:     true,
	}
	f.droplets[id] = drv
	f.names[id] = name
	return *drv, nil
}

func (f *doFake) DeleteDroplet(_ context.Context, dropletID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Idempotent, matching the real client (a 404 on an already-gone Droplet is
	// treated as success), so a Delete after an out-of-band deletion never
	// spuriously fails the machine.
	delete(f.droplets, dropletID)
	delete(f.names, dropletID)
	return nil
}

func (f *doFake) DescribeManaged(_ context.Context) ([]dropletInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dropletInstance, 0, len(f.droplets))
	for _, drv := range f.droplets {
		out = append(out, *drv)
	}
	return out, nil
}

func (f *doFake) ApplyBootstrap(_ context.Context, drv dropletInstance, clusterID string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.droplets[drv.DropletID]
	if !ok {
		return fmt.Errorf("dofake: configure unknown droplet %q", drv.DropletID)
	}
	d.ClusterID = clusterID
	return nil
}

func (f *doFake) DrainNode(_ context.Context, drv dropletInstance, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.droplets[drv.DropletID]
	if !ok {
		return fmt.Errorf("dofake: drain unknown droplet %q", drv.DropletID)
	}
	d.ClusterID = ""
	return nil
}

func (f *doFake) PriceUSD(_ context.Context, _ string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.priceUSD, nil
}

// DescribeSizeCapacities resolves capacities from the pinned table, so the
// simulator (and credential-free conformance) exercises the resolve path
// deterministically. Sizes absent from the table are omitted, exactly as the
// real Sizes API omits a size unavailable in the region.
func (f *doFake) DescribeSizeCapacities(_ context.Context, sizeSlugs []string) (map[string]sizeCapacity, error) {
	out := make(map[string]sizeCapacity, len(sizeSlugs))
	for _, t := range sizeSlugs {
		if c, ok := sizeTable[t]; ok {
			out[t] = c
		}
	}
	return out, nil
}

var _ doClient = (*doFake)(nil)
