package main

import (
	"context"
	"fmt"
	"sync"
)

// scwFake is an in-memory scwClient. It is NOT a production artifact — it backs
// unit tests and credential-free certification runs (`--scaleway-backend=fake`,
// or `auto` with no credentials). It models just enough Scaleway behaviour for
// the lifecycle: create returns a synthetic server UUID, delete removes it,
// describe lists the live ones, and bind/drain toggle the cluster tag.
type scwFake struct {
	mu      sync.Mutex
	servers map[string]*serverInstance // keyed by server id
	byToken map[string]string          // idempotency token -> server id
	volumes map[string]string          // volume id -> owning server id ("" = orphan)
	seq     int
	volSeq  int
	// priceUSD is the deterministic hourly price the simulator reports, so
	// certification and tests are reproducible.
	priceUSD float64
}

func newSCWFake() *scwFake {
	return &scwFake{
		servers:  make(map[string]*serverInstance),
		byToken:  make(map[string]string),
		volumes:  make(map[string]string),
		priceUSD: 0.0098,
	}
}

func (f *scwFake) CreateServer(_ context.Context, spec serverSpec) (serverInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Model create idempotency: a repeated token returns the existing server
	// instead of provisioning a second one.
	if spec.IdempotencyToken != "" {
		if id, ok := f.byToken[spec.IdempotencyToken]; ok {
			if srv, ok := f.servers[id]; ok {
				return *srv, nil
			}
		}
	}
	f.seq++
	// A UUID-shaped synthetic id (the real Scaleway server id is a UUID). The
	// fake's ids only need to be unique and stable within a process run.
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq)
	srv := &serverInstance{
		ServerID:       id,
		MachineID:      spec.MachineID,
		CommercialType: spec.CommercialType,
		Zone:           spec.Zone,
		Running:        true,
	}
	f.servers[id] = srv
	// Model the implicitly-created, managed-tagged boot volume attached to the
	// server, so ReapOrphanVolumes has something to sweep once it is orphaned.
	f.volSeq++
	volID := fmt.Sprintf("11111111-0000-4000-8000-%012d", f.volSeq)
	f.volumes[volID] = id
	if spec.IdempotencyToken != "" {
		f.byToken[spec.IdempotencyToken] = id
	}
	return *srv, nil
}

func (f *scwFake) DeleteServer(_ context.Context, serverID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Idempotent, matching the real client (a 404 on an already-gone server is
	// success), so a Delete after an out-of-band deletion never spuriously fails.
	delete(f.servers, serverID)
	// A normal Delete also removes the server's attached volumes inline (the real
	// client deletes the detached boot volume), so there is no leak on this path.
	for vid, owner := range f.volumes {
		if owner == serverID {
			delete(f.volumes, vid)
		}
	}
	return nil
}

// orphanServer models an out-of-band server deletion that leaves the boot volume
// behind (the server vanishes but its managed volume does not), so a test can
// exercise ReapOrphanVolumes.
func (f *scwFake) orphanServer(serverID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.servers, serverID)
	for vid, owner := range f.volumes {
		if owner == serverID {
			f.volumes[vid] = "" // detached, now orphaned
		}
	}
}

// ReapOrphanVolumes deletes managed volumes whose owning server is gone,
// modelling the real client's two-plane orphan sweep.
func (f *scwFake) ReapOrphanVolumes(_ context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reaped := 0
	for vid, owner := range f.volumes {
		if _, alive := f.servers[owner]; owner == "" || !alive {
			delete(f.volumes, vid)
			reaped++
		}
	}
	return reaped, nil
}

func (f *scwFake) DescribeManaged(_ context.Context) ([]serverInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]serverInstance, 0, len(f.servers))
	for _, srv := range f.servers {
		out = append(out, *srv)
	}
	return out, nil
}

// stop marks a server stopped, modelling a recovered-stopped Idle host (used by
// tests to exercise the power-on-before-Configure path).
func (f *scwFake) stop(serverID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[serverID]; ok {
		s.Running = false
	}
}

func (f *scwFake) EnsureRunning(_ context.Context, serverID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[serverID]
	if !ok {
		return fmt.Errorf("scwfake: ensure-running unknown server %q", serverID)
	}
	s.Running = true
	return nil
}

func (f *scwFake) ApplyBootstrap(_ context.Context, srv serverInstance, clusterID string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[srv.ServerID]
	if !ok {
		return fmt.Errorf("scwfake: configure unknown server %q", srv.ServerID)
	}
	// A stopped server's agent can't poll — Configure must have powered it on
	// first (EnsureRunning). Enforce it so the regression test is meaningful.
	if !s.Running {
		return fmt.Errorf("scwfake: configure on stopped server %q (EnsureRunning not called)", srv.ServerID)
	}
	s.ClusterID = clusterID
	return nil
}

func (f *scwFake) DrainNode(_ context.Context, srv serverInstance, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[srv.ServerID]
	if !ok {
		return fmt.Errorf("scwfake: drain unknown server %q", srv.ServerID)
	}
	s.ClusterID = ""
	return nil
}

func (f *scwFake) PriceUSD(_ context.Context, _, _ string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.priceUSD, nil
}

// DescribeCommercialTypeCapacities resolves capacities from the pinned table, so
// the simulator (and credential-free certification) exercises the resolve path
// deterministically. Types absent from the table are omitted, exactly as the real
// catalogue omits a type unavailable in the project.
func (f *scwFake) DescribeCommercialTypeCapacities(_ context.Context, commercialTypes []string) (map[string]commercialCapacity, error) {
	out := make(map[string]commercialCapacity, len(commercialTypes))
	for _, t := range commercialTypes {
		if c, ok := commercialTypeTable[t]; ok {
			out[t] = c
		}
	}
	return out, nil
}

var _ scwClient = (*scwFake)(nil)
