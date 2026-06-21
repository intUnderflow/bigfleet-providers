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
// GiB, else Mi, so both the pinned table and the live Sizes API round-trip
// without precision loss.
func (c sizeCapacity) allocatable() map[string]string {
	mem := fmt.Sprintf("%dMi", c.MemMiB)
	if c.MemMiB%1024 == 0 {
		mem = fmt.Sprintf("%dGi", c.MemMiB/1024)
	}
	return map[string]string{
		"cpu":    strconv.Itoa(c.VCPU),
		"memory": mem,
	}
}

// sizeTable is a pinned fallback snapshot of common DigitalOcean sizes (vCPU +
// RAM). The resolver seeds its cache from this, so the provider produces correct
// allocatable offline (the fake backend, credential-free conformance/certification)
// and survives a Sizes API outage; live DigitalOcean data overlays it for
// authoritative, complete coverage of whatever sizes an operator offers.
//
// Sourced from the DigitalOcean Droplet catalogue. Basic shared-CPU (s-*),
// General Purpose (g-*), CPU-Optimized (c-*), Memory-Optimized (m-*).
var sizeTable = map[string]sizeCapacity{
	// Basic shared-CPU.
	"s-1vcpu-1gb":  {1, gib(1)},
	"s-1vcpu-2gb":  {1, gib(2)},
	"s-2vcpu-2gb":  {2, gib(2)},
	"s-2vcpu-4gb":  {2, gib(4)},
	"s-4vcpu-8gb":  {4, gib(8)},
	"s-8vcpu-16gb": {8, gib(16)},
	// General Purpose.
	"g-2vcpu-8gb":   {2, gib(8)},
	"g-4vcpu-16gb":  {4, gib(16)},
	"g-8vcpu-32gb":  {8, gib(32)},
	"g-16vcpu-64gb": {16, gib(64)},
	// CPU-Optimized.
	"c-2":  {2, gib(4)},
	"c-4":  {4, gib(8)},
	"c-8":  {8, gib(16)},
	"c-16": {16, gib(32)},
	// Memory-Optimized.
	"m-2vcpu-16gb":   {2, gib(16)},
	"m-4vcpu-32gb":   {4, gib(32)},
	"m-8vcpu-64gb":   {8, gib(64)},
	"m-16vcpu-128gb": {16, gib(128)},
}

// sizeResolver maps a size slug to its hardware capacity for Machine.allocatable.
// Reads are lock-guarded and never block on the network (safe on the List/seed
// hot path); authoritative DigitalOcean data is fetched out-of-band by resolve()
// (at startup) and overlaid on the pinned fallback table. A size that is neither
// pinned nor resolved yields nil, which the engine treats as allocatable ==
// resources.
type sizeResolver struct {
	client doClient
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]sizeCapacity
}

func newSizeResolver(client doClient, logger *slog.Logger) *sizeResolver {
	cache := make(map[string]sizeCapacity, len(sizeTable))
	for k, v := range sizeTable {
		cache[k] = v
	}
	return &sizeResolver{client: client, logger: logger, cache: cache}
}

// allocatable returns the resource map for a size, or nil if the size is
// unknown. Read-only and non-blocking — safe on the List/seed hot path.
func (r *sizeResolver) allocatable(sizeSlug string) map[string]string {
	r.mu.RLock()
	c, ok := r.cache[sizeSlug]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	return c.allocatable()
}

// resolve fetches authoritative capacity from the DigitalOcean Sizes API for the
// given slugs and overlays it on the cache. Best-effort: call at startup. Size
// specs are immutable, so one resolution per process lifetime is enough. Returns
// the number of slugs it could not resolve (each still covered by the pinned
// table if present). Never call on the List hot path.
func (r *sizeResolver) resolve(ctx context.Context, sizeSlugs []string) int {
	want := dedupeNonEmpty(sizeSlugs)
	if len(want) == 0 {
		return 0
	}
	caps, err := r.client.DescribeSizeCapacities(ctx, want)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("size resolve failed; using pinned table", "sizes", len(want), "err", err)
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
