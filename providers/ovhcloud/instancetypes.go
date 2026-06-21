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
// GiB, else Mi, so both the pinned table and the live flavors API round-trip
// without precision loss.
func (c flavorCapacity) allocatable() map[string]string {
	mem := fmt.Sprintf("%dMi", c.MemMiB)
	if c.MemMiB%1024 == 0 {
		mem = fmt.Sprintf("%dGi", c.MemMiB/1024)
	}
	return map[string]string{
		"cpu":    strconv.Itoa(c.VCPU),
		"memory": mem,
	}
}

// flavorTable is a pinned fallback snapshot of common OVH Public Cloud flavors
// (vCPU + RAM). The resolver seeds its cache from this, so the provider produces
// correct allocatable offline (the fake backend, credential-free conformance)
// and survives a Nova flavors API outage; live OpenStack data overlays it for
// authoritative, complete coverage of whatever flavors an operator offers.
//
// Sourced from the OVH Public Cloud catalogue. The flavor name encodes the RAM
// in GiB for the b2/c2/r2 families (b2-7 = 7 GiB), but vCPU counts and the newer
// families do not follow a single rule, so the table is explicit. RAM is the
// on-box GiB.
var flavorTable = map[string]flavorCapacity{
	// Discovery (shared) — d2.
	"d2-2": {1, gib(2)}, "d2-4": {2, gib(4)}, "d2-8": {4, gib(8)},
	// Discovery (shared) — b3.
	"b3-8": {2, gib(8)}, "b3-16": {4, gib(16)}, "b3-32": {8, gib(32)}, "b3-64": {16, gib(64)},
	// General Purpose — b2.
	"b2-7": {2, gib(7)}, "b2-15": {4, gib(15)}, "b2-30": {8, gib(30)},
	"b2-60": {16, gib(60)}, "b2-120": {32, gib(120)},
	// CPU-optimised — c2.
	"c2-7": {2, gib(7)}, "c2-15": {4, gib(15)}, "c2-30": {8, gib(30)},
	"c2-60": {16, gib(60)}, "c2-120": {32, gib(120)},
	// CPU-optimised — c3.
	"c3-4": {1, gib(4)}, "c3-8": {2, gib(8)}, "c3-16": {4, gib(16)}, "c3-32": {8, gib(32)},
	// RAM-optimised — r2.
	"r2-15": {2, gib(15)}, "r2-30": {4, gib(30)}, "r2-60": {8, gib(60)},
	"r2-120": {16, gib(120)}, "r2-240": {32, gib(240)},
	// RAM-optimised — r3.
	"r3-16": {2, gib(16)}, "r3-32": {4, gib(32)}, "r3-64": {8, gib(64)}, "r3-128": {16, gib(128)},
	// GPU — t1 (V100), t2 (V100S), a10 (A10), l4 (L4). Accelerator types carry a label too.
	"t1-45": {8, gib(45)}, "t1-90": {16, gib(90)}, "t1-180": {32, gib(180)},
	"t2-45": {8, gib(45)}, "t2-90": {16, gib(90)}, "t2-180": {32, gib(180)},
	"a10-45": {8, gib(45)}, "a10-90": {16, gib(90)}, "l4-90": {22, gib(90)},
}

// flavorResolver maps a flavor name to its hardware capacity for
// Machine.allocatable. Reads are lock-guarded and never block on the network
// (safe on the List/seed hot path); authoritative OpenStack data is fetched
// out-of-band by resolve() (at startup) and overlaid on the pinned fallback
// table. A flavor that is neither pinned nor resolved yields nil, which the
// engine treats as allocatable == resources.
type flavorResolver struct {
	client ovhClient
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]flavorCapacity
}

func newFlavorResolver(client ovhClient, logger *slog.Logger) *flavorResolver {
	cache := make(map[string]flavorCapacity, len(flavorTable))
	for k, v := range flavorTable {
		cache[k] = v
	}
	return &flavorResolver{client: client, logger: logger, cache: cache}
}

// allocatable returns the resource map for a flavor, or nil if the flavor is
// unknown. Read-only and non-blocking — safe on the List/seed hot path.
func (r *flavorResolver) allocatable(flavor string) map[string]string {
	r.mu.RLock()
	c, ok := r.cache[flavor]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	return c.allocatable()
}

// resolve fetches authoritative capacity from the Nova flavors API for the given
// flavors and overlays it on the cache. Best-effort: call at startup. Flavor
// specs are immutable, so one resolution per process lifetime is enough. Returns
// the number of flavors it could not resolve (each still covered by the pinned
// table if present). Never call on the List hot path.
func (r *flavorResolver) resolve(ctx context.Context, flavors []string) int {
	want := dedupeNonEmpty(flavors)
	if len(want) == 0 {
		return 0
	}
	caps, err := r.client.DescribeFlavorCapacities(ctx, want)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("flavor resolve failed; using pinned table",
				"flavors", len(want), "err", err)
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
