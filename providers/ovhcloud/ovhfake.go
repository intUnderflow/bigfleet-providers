package main

import (
	"context"
	"fmt"
	"sync"
)

// ovhFake is an in-memory ovhClient. It is NOT a production artifact — it backs
// unit tests and credential-free conformance runs (`--ovh-backend=fake`, or
// `auto` with no `--region`). It models just enough OpenStack behaviour for the
// lifecycle: create returns a synthetic server UUID, delete removes it, describe
// lists the live ones, and bind/drain toggle the cluster metadata.
type ovhFake struct {
	mu      sync.Mutex
	seq     int
	servers map[string]*serverInstance // keyed by server UUID
	byToken map[string]string          // idempotency token -> server UUID
}

func newOVHFake() *ovhFake {
	return &ovhFake{
		servers: make(map[string]*serverInstance),
		byToken: make(map[string]string),
	}
}

func (f *ovhFake) CreateServer(_ context.Context, spec serverSpec) (serverInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Model create idempotency: a repeated token returns the existing server
	// instead of launching a second one. NOTE: Nova itself has no idempotency
	// token, so the REAL client (openstack.go) achieves this with a name-based
	// DescribeManaged pre-check before servers.Create — this fake's token map is
	// only a convenience for the in-memory lifecycle, not a model of Nova.
	if spec.IdempotencyToken != "" {
		if id, ok := f.byToken[spec.IdempotencyToken]; ok {
			if srv, ok := f.servers[id]; ok {
				return *srv, nil
			}
		}
	}
	f.seq++
	id := fmt.Sprintf("fake-%08d-0000-0000-0000-000000000000", f.seq)
	srv := &serverInstance{
		ServerID:   id,
		MachineID:  spec.MachineID,
		Flavor:     spec.Flavor,
		Region:     spec.Region,
		PublicIPv4: fmt.Sprintf("203.0.113.%d", f.seq%250+1),
		Running:    true,
	}
	f.servers[id] = srv
	if spec.IdempotencyToken != "" {
		f.byToken[spec.IdempotencyToken] = id
	}
	return *srv, nil
}

func (f *ovhFake) DeleteServer(_ context.Context, serverID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Idempotent, matching the real client (a 404 on Get → nil): deleting an
	// unknown / already-gone server succeeds, so a Delete after an out-of-band
	// deletion never spuriously fails the machine.
	delete(f.servers, serverID)
	return nil
}

func (f *ovhFake) StartServer(_ context.Context, serverID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[serverID]
	if !ok {
		return fmt.Errorf("ovhfake: start unknown server %q", serverID)
	}
	s.Running = true
	return nil
}

func (f *ovhFake) DescribeManaged(_ context.Context) ([]serverInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]serverInstance, 0, len(f.servers))
	for _, srv := range f.servers {
		out = append(out, *srv)
	}
	return out, nil
}

func (f *ovhFake) ApplyBootstrap(_ context.Context, srv serverInstance, clusterID string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[srv.ServerID]
	if !ok {
		return fmt.Errorf("ovhfake: configure unknown server %q", srv.ServerID)
	}
	s.ClusterID = clusterID
	return nil
}

func (f *ovhFake) DrainNode(_ context.Context, srv serverInstance, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[srv.ServerID]
	if !ok {
		return fmt.Errorf("ovhfake: drain unknown server %q", srv.ServerID)
	}
	s.ClusterID = ""
	return nil
}

// DescribeFlavorCapacities resolves capacities from the pinned table, so the
// simulator (and credential-free conformance) exercises the resolve path
// deterministically. Flavors absent from the table are omitted, exactly as the
// real Nova flavors API omits a flavor unavailable in the region.
func (f *ovhFake) DescribeFlavorCapacities(_ context.Context, flavors []string) (map[string]flavorCapacity, error) {
	out := make(map[string]flavorCapacity, len(flavors))
	for _, t := range flavors {
		if c, ok := flavorTable[t]; ok {
			out[t] = c
		}
	}
	return out, nil
}

var _ ovhClient = (*ovhFake)(nil)
