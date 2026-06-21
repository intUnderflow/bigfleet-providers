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
// GiB, else Mi, so both the pinned table and the live Plans API round-trip
// without precision loss.
func (c planCapacity) allocatable() map[string]string {
	mem := fmt.Sprintf("%dMi", c.MemMiB)
	if c.MemMiB%1024 == 0 {
		mem = fmt.Sprintf("%dGi", c.MemMiB/1024)
	}
	return map[string]string{
		"cpu":    strconv.Itoa(c.Cores),
		"memory": mem,
	}
}

// planTable is a pinned fallback snapshot of common UpCloud plans (cores + RAM).
// The resolver seeds its cache from this, so the provider produces correct
// allocatable offline (the fake backend, credential-free conformance /
// certification) and survives a Plans API outage; live UpCloud data overlays it
// for authoritative, complete coverage of whatever plans an operator offers.
//
// Sourced from the UpCloud plan catalogue. DEV (burstable) and general-purpose
// lines. UpCloud's memory_amount is in MiB (e.g. 1024 for the 1GB plan).
var planTable = map[string]planCapacity{
	// DEV / burstable.
	"DEV-1xCPU-1GB": {1, gib(1)},
	"DEV-1xCPU-2GB": {1, gib(2)},
	"DEV-1xCPU-4GB": {1, gib(4)},
	"DEV-2xCPU-4GB": {2, gib(4)},
	"DEV-2xCPU-8GB": {2, gib(8)},
	// General purpose.
	"1xCPU-1GB":    {1, gib(1)},
	"1xCPU-2GB":    {1, gib(2)},
	"2xCPU-4GB":    {2, gib(4)},
	"4xCPU-8GB":    {4, gib(8)},
	"6xCPU-16GB":   {6, gib(16)},
	"8xCPU-32GB":   {8, gib(32)},
	"12xCPU-48GB":  {12, gib(48)},
	"16xCPU-64GB":  {16, gib(64)},
	"20xCPU-96GB":  {20, gib(96)},
	"20xCPU-128GB": {20, gib(128)},
}

// planResolver maps a plan name to its hardware capacity for Machine.allocatable.
// Reads are lock-guarded and never block on the network (safe on the List/seed
// hot path); authoritative UpCloud data is fetched out-of-band by resolve() (at
// startup) and overlaid on the pinned fallback table. A plan that is neither
// pinned nor resolved yields nil, which the engine treats as allocatable ==
// resources.
type planResolver struct {
	client upcloudClient
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]planCapacity
}

func newPlanResolver(client upcloudClient, logger *slog.Logger) *planResolver {
	cache := make(map[string]planCapacity, len(planTable))
	for k, v := range planTable {
		cache[k] = v
	}
	return &planResolver{client: client, logger: logger, cache: cache}
}

// allocatable returns the resource map for a plan, or nil if the plan is
// unknown. Read-only and non-blocking — safe on the List/seed hot path.
func (r *planResolver) allocatable(plan string) map[string]string {
	r.mu.RLock()
	c, ok := r.cache[plan]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	return c.allocatable()
}

// resolve fetches authoritative capacity from the UpCloud Plans API for the
// given plans and overlays it on the cache. Best-effort: call at startup. Plan
// specs are immutable, so one resolution per process lifetime is enough. Returns
// the number of plans it could not resolve (each still covered by the pinned
// table if present). Never call on the List hot path.
func (r *planResolver) resolve(ctx context.Context, plans []string) int {
	want := dedupeNonEmpty(plans)
	if len(want) == 0 {
		return 0
	}
	caps, err := r.client.DescribePlanCapacities(ctx, want)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("plan resolve failed; using pinned table", "plans", len(want), "err", err)
		}
		return len(want)
	}
	r.mu.Lock()
	for p, c := range caps {
		r.cache[p] = c
	}
	r.mu.Unlock()
	missing := 0
	for _, p := range want {
		if _, ok := caps[p]; !ok {
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
