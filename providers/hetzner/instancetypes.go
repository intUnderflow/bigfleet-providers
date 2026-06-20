package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
)

// gib converts a whole number of GiB to MiB, for the pinned table literals.
func gib(n int64) int64 { return n * 1024 }

// allocatable renders the capacity as a Kubernetes-style resource map
// (deterministic strings). Memory renders as Gi when it is a whole number of
// GiB, else Mi, so both the pinned table and the live ServerType API round-trip
// without precision loss.
func (c serverCapacity) allocatable() map[string]string {
	mem := fmt.Sprintf("%dMi", c.MemMiB)
	if c.MemMiB%1024 == 0 {
		mem = fmt.Sprintf("%dGi", c.MemMiB/1024)
	}
	return map[string]string{
		"cpu":    strconv.Itoa(c.VCPU),
		"memory": mem,
	}
}

// serverTypeTable is a pinned fallback snapshot of common Hetzner Cloud server
// types (vCPU + RAM). The resolver seeds its cache from this, so the provider
// produces correct allocatable offline (the fake backend, credential-free
// conformance) and survives a ServerType API outage; live Hetzner data overlays
// it for authoritative, complete coverage of whatever types an operator offers.
//
// Sourced from the Hetzner Cloud catalogue. Shared-vCPU lines: cx (Intel/AMD,
// current gen), cpx (AMD, dedicated-ish shared), cax (Ampere Arm64). Dedicated
// vCPU: ccx (AMD). RAM is the on-box GiB.
var serverTypeTable = map[string]serverCapacity{
	// CX (shared Intel/AMD, current generation).
	"cx22": {2, gib(4)},
	"cx32": {4, gib(8)},
	"cx42": {8, gib(16)},
	"cx52": {16, gib(32)},
	// CPX (shared AMD).
	"cpx11": {2, gib(2)},
	"cpx21": {3, gib(4)},
	"cpx31": {4, gib(8)},
	"cpx41": {8, gib(16)},
	"cpx51": {16, gib(32)},
	// CAX (shared Ampere Arm64).
	"cax11": {2, gib(4)},
	"cax21": {4, gib(8)},
	"cax31": {8, gib(16)},
	"cax41": {16, gib(32)},
	// CCX (dedicated AMD).
	"ccx13": {2, gib(8)},
	"ccx23": {4, gib(16)},
	"ccx33": {8, gib(32)},
	"ccx43": {16, gib(64)},
	"ccx53": {32, gib(128)},
	"ccx63": {48, gib(192)},
}

// serverTypeResolver maps a server type to its hardware capacity for
// Machine.allocatable. Reads are lock-guarded and never block on the network
// (safe on the List/seed hot path); authoritative Hetzner data is fetched
// out-of-band by resolve() (at startup) and overlaid on the pinned fallback
// table. A type that is neither pinned nor resolved yields nil, which the
// engine treats as allocatable == resources.
type serverTypeResolver struct {
	client hcloudClient
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]serverCapacity
}

func newServerTypeResolver(client hcloudClient, logger *slog.Logger) *serverTypeResolver {
	cache := make(map[string]serverCapacity, len(serverTypeTable))
	for k, v := range serverTypeTable {
		cache[k] = v
	}
	return &serverTypeResolver{client: client, logger: logger, cache: cache}
}

// allocatable returns the resource map for a server type, or nil if the type is
// unknown. Read-only and non-blocking — safe on the List/seed hot path.
func (r *serverTypeResolver) allocatable(serverType string) map[string]string {
	r.mu.RLock()
	c, ok := r.cache[serverType]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	return c.allocatable()
}

// resolve fetches authoritative capacity from the Hetzner ServerType API for the
// given types and overlays it on the cache. Best-effort: call at startup. Server
// type specs are immutable, so one resolution per process lifetime is enough.
// Returns the number of types it could not resolve (each still covered by the
// pinned table if present). Never call on the List hot path.
func (r *serverTypeResolver) resolve(ctx context.Context, serverTypes []string) int {
	want := dedupeNonEmpty(serverTypes)
	if len(want) == 0 {
		return 0
	}
	caps, err := r.client.DescribeServerTypeCapacities(ctx, want)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("server-type resolve failed; using pinned table",
				"types", len(want), "err", err)
		}
		return len(want)
	}
	r.mu.Lock()
	for t, c := range caps {
		r.cache[t] = c
	}
	r.mu.Unlock()
	missing := 0
	for _, t := range want {
		if _, ok := caps[t]; !ok {
			missing++
		}
	}
	return missing
}

// dedupeNonEmpty returns the distinct non-empty strings, sorted for determinism
// (stable batching + test assertions).
func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
