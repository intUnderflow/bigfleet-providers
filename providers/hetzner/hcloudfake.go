package main

import (
	"context"
	"fmt"
	"sync"
)

// hcloudFake is an in-memory hcloudClient. It is NOT a production artifact — it
// backs unit tests and credential-free conformance runs (`--hetzner-backend=fake`,
// or `auto` with no token). It models just enough Hetzner Cloud behaviour for the
// lifecycle: create returns a synthetic server id, delete removes it, describe
// lists the live ones, and bind/drain toggle the cluster label.
type hcloudFake struct {
	mu      sync.Mutex
	seq     int
	servers map[string]*serverInstance // keyed by server id
	byToken map[string]string          // idempotency token -> server id
	// priceUSD is the deterministic hourly price the simulator reports, so
	// conformance and tests are reproducible.
	priceUSD float64
}

func newHCloudFake() *hcloudFake {
	return &hcloudFake{
		servers:  make(map[string]*serverInstance),
		byToken:  make(map[string]string),
		priceUSD: 0.0065,
	}
}

func (f *hcloudFake) CreateServer(_ context.Context, spec serverSpec) (serverInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Model create idempotency: a repeated token returns the existing server
	// instead of launching a second one.
	if spec.IdempotencyToken != "" {
		if id, ok := f.byToken[spec.IdempotencyToken]; ok {
			if srv, ok := f.servers[id]; ok {
				return *srv, nil
			}
		}
	}
	f.seq++
	id := fmt.Sprintf("%d", 1000000+f.seq)
	srv := &serverInstance{
		ServerID:   id,
		MachineID:  spec.MachineID,
		ServerType: spec.ServerType,
		Location:   spec.Location,
		PublicIPv4: fmt.Sprintf("203.0.113.%d", f.seq%250+1),
		Running:    true,
	}
	f.servers[id] = srv
	if spec.IdempotencyToken != "" {
		f.byToken[spec.IdempotencyToken] = id
	}
	return *srv, nil
}

func (f *hcloudFake) DeleteServer(_ context.Context, serverID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Idempotent, matching the real client (GetByID nil → nil): deleting an
	// unknown / already-gone server succeeds, so a Delete after an out-of-band
	// deletion never spuriously fails the machine.
	delete(f.servers, serverID)
	return nil
}

func (f *hcloudFake) DescribeManaged(_ context.Context) ([]serverInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]serverInstance, 0, len(f.servers))
	for _, srv := range f.servers {
		out = append(out, *srv)
	}
	return out, nil
}

func (f *hcloudFake) ApplyBootstrap(_ context.Context, srv serverInstance, clusterID string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[srv.ServerID]
	if !ok {
		return fmt.Errorf("hcloudfake: configure unknown server %q", srv.ServerID)
	}
	s.ClusterID = clusterID
	return nil
}

func (f *hcloudFake) DrainNode(_ context.Context, srv serverInstance, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[srv.ServerID]
	if !ok {
		return fmt.Errorf("hcloudfake: drain unknown server %q", srv.ServerID)
	}
	s.ClusterID = ""
	return nil
}

func (f *hcloudFake) PriceUSD(_ context.Context, _, _ string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.priceUSD, nil
}

// DescribeServerTypeCapacities resolves capacities from the pinned table, so the
// simulator (and credential-free conformance) exercises the resolve path
// deterministically. Types absent from the table are omitted, exactly as the
// real ServerType API omits a type unavailable in the project.
func (f *hcloudFake) DescribeServerTypeCapacities(_ context.Context, serverTypes []string) (map[string]serverCapacity, error) {
	out := make(map[string]serverCapacity, len(serverTypes))
	for _, t := range serverTypes {
		if c, ok := serverTypeTable[t]; ok {
			out[t] = c
		}
	}
	return out, nil
}

var _ hcloudClient = (*hcloudFake)(nil)
