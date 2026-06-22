package main

import (
	"context"
	"fmt"
	"sync"
)

// upcloudFake is an in-memory upcloudClient. It is NOT a production artifact — it
// backs unit tests and credential-free conformance / certification runs
// (`--upcloud-backend=fake`, or `auto` with no credentials). It models just
// enough UpCloud behaviour for the lifecycle: create returns a synthetic server
// UUID (with an attached storage so the leak-free Delete path is exercised),
// delete removes the server AND its storage, describe lists them, bind/drain
// toggle the cluster label, and — critically — a server can be STOPPED
// OUT-OF-BAND so the EnsureRunning-before-Configure/Drain contract (§4.6) is
// covered by real tests.
type upcloudFake struct {
	mu       sync.Mutex
	seq      int
	servers  map[string]*serverInstance // keyed by server UUID
	storages map[string]string          // server UUID -> attached storage UUID (the leak-free Delete must clear this)
	byToken  map[string]string          // idempotency token -> server UUID
	// priceFactor scales the pinned EUR table to synthesize a deterministic LIVE
	// price per plan, so a credential-free run exercises the live-refresh path and
	// tests can assert the refresher overlays the table. Defaults to 1.0 (live ==
	// pinned baseline); a test sets a distinct factor to prove the live value flows.
	priceFactor float64
}

func newUpcloudFake() *upcloudFake {
	return &upcloudFake{
		servers:     make(map[string]*serverInstance),
		storages:    make(map[string]string),
		byToken:     make(map[string]string),
		priceFactor: 1.0,
	}
}

func (f *upcloudFake) CreateServer(_ context.Context, spec serverSpec) (serverInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Model create idempotency: a repeated operation token returns the existing
	// server instead of launching a second one (the substrate-level guard the real
	// client gets from a stable title/hostname + pre-create lookup).
	if spec.IdempotencyToken != "" {
		if uuid, ok := f.byToken[spec.IdempotencyToken]; ok {
			if srv, ok := f.servers[uuid]; ok {
				return *srv, nil
			}
		}
	}
	f.seq++
	uuid := fmt.Sprintf("00b8c4e0-0000-4000-8000-%012d", f.seq)
	srv := &serverInstance{
		UUID:       uuid,
		MachineID:  spec.MachineID,
		Plan:       spec.Plan,
		Zone:       spec.Zone,
		PublicIPv4: fmt.Sprintf("94.237.0.%d", f.seq%250+1),
		HostKeyFP:  fmt.Sprintf("fakehostkey%012d", f.seq),
		Running:    true,
	}
	f.servers[uuid] = srv
	f.storages[uuid] = fmt.Sprintf("01b8c4e0-0000-4000-8000-%012d", f.seq)
	if spec.IdempotencyToken != "" {
		f.byToken[spec.IdempotencyToken] = uuid
	}
	return *srv, nil
}

// DeleteServer deletes the server AND its attached storage (the leak-free
// DeleteServerAndStorages path). Idempotent, matching the real client: deleting
// an unknown / already-gone server succeeds, so a Delete after an out-of-band
// deletion never spuriously fails the machine.
func (f *upcloudFake) DeleteServer(_ context.Context, uuid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.servers, uuid)
	delete(f.storages, uuid) // the storage goes with the server — no leaked disk
	return nil
}

func (f *upcloudFake) DescribeManaged(_ context.Context) ([]serverInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]serverInstance, 0, len(f.servers))
	for _, srv := range f.servers {
		out = append(out, *srv)
	}
	return out, nil
}

// EnsureRunning powers a stopped server back on. A no-op for an already-started
// server. This is the fake half of the §4.6 contract: a server stopped
// out-of-band (via stopOutOfBand) is transparently re-powered before Configure /
// Drain do their work.
func (f *upcloudFake) EnsureRunning(_ context.Context, srv serverInstance) (serverInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[srv.UUID]
	if !ok {
		return serverInstance{}, fmt.Errorf("upcloudfake: ensure running unknown server %q", srv.UUID)
	}
	s.Running = true
	return *s, nil
}

func (f *upcloudFake) ApplyBootstrap(_ context.Context, srv serverInstance, clusterID string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[srv.UUID]
	if !ok {
		return fmt.Errorf("upcloudfake: configure unknown server %q", srv.UUID)
	}
	if !s.Running {
		// The backend must EnsureRunning first; a stopped server here is a bug.
		return fmt.Errorf("upcloudfake: configure against stopped server %q (EnsureRunning not called)", srv.UUID)
	}
	s.ClusterID = clusterID
	return nil
}

func (f *upcloudFake) DrainNode(_ context.Context, srv serverInstance, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[srv.UUID]
	if !ok {
		return fmt.Errorf("upcloudfake: drain unknown server %q", srv.UUID)
	}
	if !s.Running {
		return fmt.Errorf("upcloudfake: drain against stopped server %q (EnsureRunning not called)", srv.UUID)
	}
	s.ClusterID = ""
	return nil
}

// DescribePlanCapacities resolves capacities from the pinned table, so the
// simulator (and credential-free conformance) exercises the resolve path
// deterministically. Plans absent from the table are omitted, exactly as the
// real Plans API omits an unknown plan.
func (f *upcloudFake) DescribePlanCapacities(_ context.Context, plans []string) (map[string]planCapacity, error) {
	out := make(map[string]planCapacity, len(plans))
	for _, p := range plans {
		if c, ok := planTable[p]; ok {
			out[p] = c
		}
	}
	return out, nil
}

// DescribePlanPrices synthesizes a deterministic live price (EUR/hour) for each
// requested plan from the pinned table scaled by priceFactor, so the simulator
// (and credential-free conformance) exercises the live-refresh path
// reproducibly. Plans absent from the table are omitted, exactly as the real
// /price endpoint omits a plan it does not price.
func (f *upcloudFake) DescribePlanPrices(_ context.Context, plans []string) (map[string]float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	factor := f.priceFactor
	if factor <= 0 {
		factor = 1.0
	}
	out := make(map[string]float64, len(plans))
	for _, p := range plans {
		if eur, ok := onDemandEURHourly[p]; ok {
			out[p] = eur * factor
		}
	}
	return out, nil
}

// stopOutOfBand models an operator (or a billing event / crash) stopping a
// tracked server behind the provider's back. Test-only — it is how the §4.6
// EnsureRunning regression tests create a stopped tracked server.
func (f *upcloudFake) stopOutOfBand(uuid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[uuid]; ok {
		s.Running = false
	}
}

var _ upcloudClient = (*upcloudFake)(nil)
