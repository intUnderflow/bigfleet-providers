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
		"cpu":    strconv.Itoa(c.VCPU),
		"memory": mem,
	}
}

// planTable is a pinned fallback snapshot of common Latitude.sh plans (vCPU +
// RAM). The resolver seeds its cache from this, so the provider produces correct
// allocatable offline (the fake backend, credential-free conformance) and
// survives a Plans API outage; live Latitude data overlays it for authoritative,
// complete coverage of whatever plans an operator offers.
//
// vCPU is the plan's total logical cores (cpu.cores × cpu.count); memory is the
// on-box GiB. These are bare-metal boxes, so the per-plan capacity is large and
// the density payoff (floor(allocatable / resources)) is the whole point.
var planTable = map[string]planCapacity{
	// Compute (c-series, x86).
	"c2-small-x86":  {4, gib(32)},
	"c2-medium-x86": {8, gib(64)},
	"c2-large-x86":  {16, gib(128)},
	"c3-small-x86":  {8, gib(32)},
	"c3-medium-x86": {16, gib(64)},
	"c3-large-x86":  {24, gib(128)},
	"c3-xlarge-x86": {48, gib(256)},
	// Storage (s-series).
	"s2-small-x86": {8, gib(64)},
	"s3-large-x86": {24, gib(128)},
	// Memory (m-series).
	"m3-large-x86":   {32, gib(256)},
	"m4-metal-large": {64, gib(512)},
	// GPU (g-series).
	"g3-large-x86":  {32, gib(256)},
	"g3-xlarge-x86": {48, gib(512)},
	"g4-xlarge-x86": {64, gib(1024)},
}

// planResolver maps a plan slug to its hardware capacity for Machine.allocatable.
// Reads are lock-guarded and never block on the network (safe on the List/seed
// hot path); authoritative Latitude data is fetched out-of-band by resolve() (at
// startup) and overlaid on the pinned fallback table. A plan that is neither
// pinned nor resolved yields nil, which the engine treats as allocatable ==
// resources.
type planResolver struct {
	client latitudeClient
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]planCapacity
}

func newPlanResolver(client latitudeClient, logger *slog.Logger) *planResolver {
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

// resolve fetches authoritative capacity from the Latitude Plans API for the
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
			r.logger.Warn("plan resolve failed; using pinned table",
				"plans", len(want), "err", err)
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
