package main

import (
	"context"
	"fmt"
	"sync"
)

// latitudeFake is an in-memory latitudeClient. It is NOT a production artifact —
// it backs unit tests and credential-free conformance runs
// (`--latitude-backend=fake`, or `auto` with no token). It models just enough
// Latitude.sh behaviour for the lifecycle: deploy returns a synthetic server id,
// delete removes it, describe lists the live ones, bind/drain toggle the cluster
// binding, and power state is tracked so EnsureRunning can be exercised.
//
// Crucially, ApplyBootstrap and DrainNode REQUIRE the server to be powered on:
// this is what lets a test prove the backend EnsureRunning (powers a stopped
// server on) BEFORE delivering the bootstrap / draining — a stopped server would
// otherwise fail the operation.
type latitudeFake struct {
	mu      sync.Mutex
	seq     int
	servers map[string]*serverInstance // keyed by server id
	byToken map[string]string          // idempotency token -> server id
	// priceUSD is the deterministic hourly price the simulator reports, so
	// conformance and tests are reproducible.
	priceUSD float64
}

func newLatitudeFake() *latitudeFake {
	return &latitudeFake{
		servers:  make(map[string]*serverInstance),
		byToken:  make(map[string]string),
		priceUSD: 0.30,
	}
}

func (f *latitudeFake) CreateServer(_ context.Context, spec serverSpec) (serverInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Model deploy idempotency: a repeated token adopts the existing server
	// instead of deploying a second one. If that server was powered off
	// (recovered-stopped), re-power it rather than leaving it down.
	if spec.IdempotencyToken != "" {
		if id, ok := f.byToken[spec.IdempotencyToken]; ok {
			if srv, ok := f.servers[id]; ok {
				srv.PoweredOn = true
				srv.Running = true
				return *srv, nil
			}
		}
	}
	f.seq++
	id := fmt.Sprintf("sv_%08d", f.seq)
	srv := &serverInstance{
		ServerID:   id,
		MachineID:  spec.MachineID,
		Plan:       spec.Plan,
		Site:       spec.Site,
		PublicIPv4: fmt.Sprintf("198.51.100.%d", f.seq%250+1),
		Running:    true,
		PoweredOn:  true,
	}
	f.servers[id] = srv
	if spec.IdempotencyToken != "" {
		f.byToken[spec.IdempotencyToken] = id
	}
	return *srv, nil
}

func (f *latitudeFake) DeleteServer(_ context.Context, serverID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Idempotent, matching the real client (404 → nil): deprovisioning an unknown
	// / already-gone server succeeds, so a Delete after an out-of-band deletion
	// never spuriously fails the machine.
	delete(f.servers, serverID)
	return nil
}

func (f *latitudeFake) DescribeManaged(_ context.Context) ([]serverInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]serverInstance, 0, len(f.servers))
	for _, srv := range f.servers {
		out = append(out, *srv)
	}
	return out, nil
}

func (f *latitudeFake) GetServer(_ context.Context, serverID string) (serverInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	srv, ok := f.servers[serverID]
	if !ok {
		return serverInstance{}, fmt.Errorf("latitudefake: unknown server %q", serverID)
	}
	return *srv, nil
}

func (f *latitudeFake) PowerOn(_ context.Context, serverID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	srv, ok := f.servers[serverID]
	if !ok {
		return fmt.Errorf("latitudefake: power on unknown server %q", serverID)
	}
	srv.PoweredOn = true
	srv.Running = true
	return nil
}

func (f *latitudeFake) ApplyBootstrap(_ context.Context, srv serverInstance, clusterID string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[srv.ServerID]
	if !ok {
		return fmt.Errorf("latitudefake: configure unknown server %q", srv.ServerID)
	}
	// A stopped server cannot receive the SSH bootstrap. The backend must
	// EnsureRunning first; if it didn't, surface that as a failure (which is what
	// the EnsureRunning regression test relies on).
	if !s.PoweredOn {
		return fmt.Errorf("latitudefake: cannot deliver bootstrap to powered-off server %q (EnsureRunning was skipped)", srv.ServerID)
	}
	s.ClusterID = clusterID
	return nil
}

func (f *latitudeFake) DrainNode(_ context.Context, srv serverInstance, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[srv.ServerID]
	if !ok {
		return fmt.Errorf("latitudefake: drain unknown server %q", srv.ServerID)
	}
	if !s.PoweredOn {
		return fmt.Errorf("latitudefake: cannot drain powered-off server %q (EnsureRunning was skipped)", srv.ServerID)
	}
	s.ClusterID = ""
	return nil
}

func (f *latitudeFake) PriceUSD(_ context.Context, _, _ string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.priceUSD, nil
}

// DescribePlanCapacities resolves capacities from the pinned table, so the
// simulator (and credential-free conformance) exercises the resolve path
// deterministically. Plans absent from the table are omitted, exactly as the
// real Plans API omits a plan unavailable to the project.
func (f *latitudeFake) DescribePlanCapacities(_ context.Context, plans []string) (map[string]planCapacity, error) {
	out := make(map[string]planCapacity, len(plans))
	for _, p := range plans {
		if c, ok := planTable[p]; ok {
			out[p] = c
		}
	}
	return out, nil
}

// setPowerState is a test hook: mark a tracked server powered off (simulating an
// out-of-band power-off) so EnsureRunning behaviour can be asserted.
func (f *latitudeFake) setPowerState(serverID string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if srv, ok := f.servers[serverID]; ok {
		srv.PoweredOn = on
	}
}

var _ latitudeClient = (*latitudeFake)(nil)
