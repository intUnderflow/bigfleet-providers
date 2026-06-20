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
// GiB, else Mi, so both the pinned table and the live Resource SKUs API
// round-trip without precision loss.
func (c vmCapacity) allocatable() map[string]string {
	mem := fmt.Sprintf("%dMi", c.MemMiB)
	if c.MemMiB%1024 == 0 {
		mem = fmt.Sprintf("%dGi", c.MemMiB/1024)
	}
	return map[string]string{
		"cpu":    strconv.Itoa(c.VCPU),
		"memory": mem,
	}
}

// vmSizeTable is a pinned fallback snapshot of common Azure VM sizes (vCPU +
// RAM). The resolver seeds its cache from this, so the provider produces correct
// allocatable offline (the fake backend, credential-free conformance) and
// survives a Resource SKUs API outage; live Azure data overlays it for
// authoritative, complete coverage of whatever sizes an operator offers.
//
// Sourced from the Azure VM sizes catalogue. Dsv5 (general purpose, Intel),
// Dasv5 (general purpose, AMD), Fsv2 (compute optimised), Esv5 (memory
// optimised), NCadsA100v4 (GPU). RAM is the on-box GiB.
var vmSizeTable = map[string]vmCapacity{
	// Dsv5 — general purpose (Intel Ice Lake).
	"Standard_D2s_v5":  {2, gib(8)},
	"Standard_D4s_v5":  {4, gib(16)},
	"Standard_D8s_v5":  {8, gib(32)},
	"Standard_D16s_v5": {16, gib(64)},
	"Standard_D32s_v5": {32, gib(128)},
	"Standard_D48s_v5": {48, gib(192)},
	"Standard_D64s_v5": {64, gib(256)},
	// Dasv5 — general purpose (AMD).
	"Standard_D2as_v5":  {2, gib(8)},
	"Standard_D4as_v5":  {4, gib(16)},
	"Standard_D8as_v5":  {8, gib(32)},
	"Standard_D16as_v5": {16, gib(64)},
	"Standard_D32as_v5": {32, gib(128)},
	// Fsv2 — compute optimised.
	"Standard_F2s_v2":  {2, gib(4)},
	"Standard_F4s_v2":  {4, gib(8)},
	"Standard_F8s_v2":  {8, gib(16)},
	"Standard_F16s_v2": {16, gib(32)},
	"Standard_F32s_v2": {32, gib(64)},
	"Standard_F48s_v2": {48, gib(96)},
	"Standard_F64s_v2": {64, gib(128)},
	// Esv5 — memory optimised.
	"Standard_E2s_v5":  {2, gib(16)},
	"Standard_E4s_v5":  {4, gib(32)},
	"Standard_E8s_v5":  {8, gib(64)},
	"Standard_E16s_v5": {16, gib(128)},
	"Standard_E32s_v5": {32, gib(256)},
	// NCadsA100v4 — GPU (NVIDIA A100); accelerator types carry a label too.
	"Standard_NC24ads_A100_v4": {24, gib(220)},
	"Standard_NC48ads_A100_v4": {48, gib(440)},
	"Standard_NC96ads_A100_v4": {96, gib(880)},
}

// vmSizeResolver maps a VM size to its hardware capacity for Machine.allocatable.
// Reads are lock-guarded and never block on the network (safe on the List/seed
// hot path); authoritative Azure data is fetched out-of-band by resolve() (at
// startup) and overlaid on the pinned fallback table. A size that is neither
// pinned nor resolved yields nil, which the engine treats as
// allocatable == resources.
type vmSizeResolver struct {
	client azureClient
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]vmCapacity
}

func newVMSizeResolver(client azureClient, logger *slog.Logger) *vmSizeResolver {
	cache := make(map[string]vmCapacity, len(vmSizeTable))
	for k, v := range vmSizeTable {
		cache[k] = v
	}
	return &vmSizeResolver{client: client, logger: logger, cache: cache}
}

// allocatable returns the resource map for a VM size, or nil if the size is
// unknown. Read-only and non-blocking — safe on the List/seed hot path.
func (r *vmSizeResolver) allocatable(vmSize string) map[string]string {
	r.mu.RLock()
	c, ok := r.cache[vmSize]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	return c.allocatable()
}

// resolve fetches authoritative capacity from the Resource SKUs API for the
// given sizes and overlays it on the cache. Best-effort: call at startup. VM
// size specs are immutable, so one resolution per process lifetime is enough.
// Returns the number of sizes it could not resolve (each still covered by the
// pinned table if present). Never call on the List hot path.
func (r *vmSizeResolver) resolve(ctx context.Context, vmSizes []string) int {
	want := dedupeNonEmpty(vmSizes)
	if len(want) == 0 {
		return 0
	}
	caps, err := r.client.DescribeVMSizeCapacities(ctx, want)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("vm-size resolve failed; using pinned table",
				"sizes", len(want), "err", err)
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
