package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
)

// gib converts a whole number of GiB to MiB, for the pinned-table literals.
func gib(n int64) int64 { return n * 1024 }

// allocatable renders the capacity as a Kubernetes-style resource map
// (deterministic strings). Memory renders as Gi when it is a whole number of
// GiB, else Mi, so both the pinned table and the live catalogue round-trip
// without precision loss. A GPU count, when present, is exposed as
// nvidia.com/gpu so the shard can satisfy GPU requests.
func (c commercialCapacity) allocatable() map[string]string {
	mem := fmt.Sprintf("%dMi", c.MemMiB)
	if c.MemMiB%1024 == 0 {
		mem = fmt.Sprintf("%dGi", c.MemMiB/1024)
	}
	out := map[string]string{
		"cpu":    strconv.Itoa(c.VCPU),
		"memory": mem,
	}
	if c.GPUs > 0 {
		out["nvidia.com/gpu"] = strconv.Itoa(c.GPUs)
	}
	return out
}

// commercialTypeTable is a pinned fallback snapshot of common Scaleway commercial
// types (vCPU + RAM + GPU). The resolver seeds its cache from this, so the
// provider produces correct allocatable offline (the fake backend,
// credential-free certification) and survives a catalogue API outage; live
// Scaleway data overlays it for authoritative coverage of whatever types an
// operator offers.
//
// Sourced from the Scaleway product catalogue. Instances: DEV1 (development),
// GP1 (general purpose), PLAY2/PRO2 (current shared/dedicated), COPARM1 (Ampere
// Arm64), RENDER/H100/L4 (GPU). Elastic Metal: EM-* server types.
var commercialTypeTable = map[string]commercialCapacity{
	// DEV1.
	"DEV1-S": {2, gib(2), 0}, "DEV1-M": {3, gib(4), 0}, "DEV1-L": {4, gib(8), 0}, "DEV1-XL": {4, gib(12), 0},
	// GP1 (AMD EPYC).
	"GP1-XS": {4, gib(16), 0}, "GP1-S": {8, gib(32), 0}, "GP1-M": {16, gib(64), 0},
	"GP1-L": {32, gib(128), 0}, "GP1-XL": {48, gib(256), 0},
	// PLAY2 / PRO2.
	"PLAY2-PICO": {1, gib(2), 0}, "PLAY2-NANO": {2, gib(4), 0}, "PLAY2-MICRO": {4, gib(8), 0},
	"PRO2-XXS": {2, gib(8), 0}, "PRO2-XS": {4, gib(16), 0}, "PRO2-S": {8, gib(32), 0},
	"PRO2-M": {16, gib(64), 0}, "PRO2-L": {32, gib(128), 0},
	// COPARM1 (Ampere Arm64).
	"COPARM1-2C-8G": {2, gib(8), 0}, "COPARM1-4C-16G": {4, gib(16), 0},
	"COPARM1-8C-32G": {8, gib(32), 0}, "COPARM1-16C-64G": {16, gib(64), 0},
	// GPU.
	"RENDER-S": {10, gib(45), 1}, "H100-1-80G": {24, gib(240), 1}, "L4-1-24G": {8, gib(48), 1},
	// Elastic Metal (representative).
	"EM-A210R-HDD": {4, gib(16), 0}, "EM-B112X-SSD": {8, gib(32), 0},
	"EM-B212X-SSD": {16, gib(64), 0}, "EM-I210E-NVME": {16, gib(256), 0},
}

// commercialTypeResolver maps a commercial type to its hardware capacity for
// Machine.allocatable. Reads are lock-guarded and never block on the network
// (safe on the List/seed hot path); authoritative Scaleway data is fetched
// out-of-band by resolve() (at startup) and overlaid on the pinned fallback
// table. A type that is neither pinned nor resolved yields nil, which the engine
// treats as allocatable == resources.
type commercialTypeResolver struct {
	client scwClient
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]commercialCapacity
}

func newCommercialTypeResolver(client scwClient, logger *slog.Logger) *commercialTypeResolver {
	cache := make(map[string]commercialCapacity, len(commercialTypeTable))
	for k, v := range commercialTypeTable {
		cache[k] = v
	}
	return &commercialTypeResolver{client: client, logger: logger, cache: cache}
}

// allocatable returns the resource map for a commercial type, or nil if the type
// is unknown. Read-only and non-blocking — safe on the List/seed hot path.
func (r *commercialTypeResolver) allocatable(commercialType string) map[string]string {
	r.mu.RLock()
	c, ok := r.cache[commercialType]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	return c.allocatable()
}

// resolve fetches authoritative capacity from the Scaleway catalogue for the
// given types and overlays it on the cache. Best-effort: call at startup.
// Commercial-type specs are immutable, so one resolution per process lifetime is
// enough. Returns the number of types it could not resolve (each still covered by
// the pinned table if present). Never call on the List hot path.
func (r *commercialTypeResolver) resolve(ctx context.Context, commercialTypes []string) int {
	want := dedupeNonEmpty(commercialTypes)
	if len(want) == 0 {
		return 0
	}
	caps, err := r.client.DescribeCommercialTypeCapacities(ctx, want)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("commercial-type resolve failed; using pinned table",
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

// dedupeNonEmpty returns the distinct non-empty strings, sorted for determinism.
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
