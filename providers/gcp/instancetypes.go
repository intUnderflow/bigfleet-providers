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
// GiB, else Mi, so both the pinned table and the live MachineTypes API
// round-trip without precision loss.
func (c machineCapacity) allocatable() map[string]string {
	mem := fmt.Sprintf("%dMi", c.MemMiB)
	if c.MemMiB%1024 == 0 {
		mem = fmt.Sprintf("%dGi", c.MemMiB/1024)
	}
	return map[string]string{
		"cpu":    strconv.Itoa(c.VCPU),
		"memory": mem,
	}
}

// machineTypeTable is a pinned fallback snapshot of common GCE machine types
// (vCPU + RAM). The resolver seeds its cache from this, so the provider
// produces correct allocatable offline (the fake backend, credential-free
// certification) and survives a MachineTypes API outage; live GCE data overlays
// it for authoritative, complete coverage of whatever types an operator offers.
//
// Sourced from the GCE machine-families catalogue. e2 (cost-optimised), n2
// (general purpose, Intel), n2d (AMD), c2 (compute-optimised), c3 (current-gen
// general purpose), m1 (memory-optimised). Memory is the published on-box GiB.
var machineTypeTable = map[string]machineCapacity{
	// E2 (cost-optimised, shared/standard).
	"e2-standard-2": {2, gib(8)}, "e2-standard-4": {4, gib(16)},
	"e2-standard-8": {8, gib(32)}, "e2-standard-16": {16, gib(64)},
	// N2 (general purpose, Intel Cascade/Ice Lake).
	"n2-standard-2": {2, gib(8)}, "n2-standard-4": {4, gib(16)},
	"n2-standard-8": {8, gib(32)}, "n2-standard-16": {16, gib(64)},
	"n2-standard-32": {32, gib(128)},
	"n2-highmem-2":   {2, gib(16)}, "n2-highmem-4": {4, gib(32)},
	"n2-highmem-8": {8, gib(64)},
	// N2D (general purpose, AMD EPYC).
	"n2d-standard-4": {4, gib(16)}, "n2d-standard-8": {8, gib(32)},
	"n2d-standard-16": {16, gib(64)},
	// C2 (compute-optimised).
	"c2-standard-4": {4, gib(16)}, "c2-standard-8": {8, gib(32)},
	"c2-standard-16": {16, gib(64)}, "c2-standard-30": {30, gib(120)},
	// C3 (current-gen general purpose).
	"c3-standard-4": {4, gib(16)}, "c3-standard-8": {8, gib(32)},
	"c3-standard-22": {22, gib(88)}, "c3-highmem-22": {22, gib(176)},
	// M1 (memory-optimised).
	"m1-megamem-96": {96, gib(1433)},
	// A2 / G2 (accelerator-optimised — carry a label too, see offering.go).
	"a2-highgpu-1g": {12, gib(85)}, "g2-standard-4": {4, gib(16)},
}

// machineTypeResolver maps a machine type to its hardware capacity for
// Machine.allocatable. Reads are lock-guarded and never block on the network
// (safe on the List/seed hot path); authoritative GCE data is fetched
// out-of-band by resolve() (at startup) and overlaid on the pinned fallback
// table. A type that is neither pinned nor resolved yields nil, which the engine
// treats as allocatable == resources.
type machineTypeResolver struct {
	client gceClient
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]machineCapacity
}

func newMachineTypeResolver(client gceClient, logger *slog.Logger) *machineTypeResolver {
	cache := make(map[string]machineCapacity, len(machineTypeTable))
	for k, v := range machineTypeTable {
		cache[k] = v
	}
	return &machineTypeResolver{client: client, logger: logger, cache: cache}
}

// allocatable returns the resource map for a machine type, or nil if the type is
// unknown. Read-only and non-blocking — safe on the List/seed hot path.
func (r *machineTypeResolver) allocatable(machineType string) map[string]string {
	r.mu.RLock()
	c, ok := r.cache[machineType]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	return c.allocatable()
}

// resolve fetches authoritative capacity from the GCE MachineTypes API for the
// given (type, zone) refs and overlays it on the cache. Best-effort: call at
// startup. Machine-type specs are immutable, so one resolution per process
// lifetime is enough. Returns the number of distinct types it could not resolve
// (each still covered by the pinned table if present). Never call on the List
// hot path.
func (r *machineTypeResolver) resolve(ctx context.Context, refs []machineTypeRef) int {
	refs = dedupeRefs(refs)
	if len(refs) == 0 {
		return 0
	}
	caps, err := r.client.DescribeMachineTypeCapacities(ctx, refs)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("machine-type resolve failed; using pinned table",
				"types", len(refs), "err", err)
		}
		return len(refs)
	}
	r.mu.Lock()
	for t, c := range caps {
		r.cache[t] = c
	}
	r.mu.Unlock()
	missing := 0
	for _, ref := range refs {
		if _, ok := caps[ref.MachineType]; !ok {
			missing++
		}
	}
	return missing
}

// dedupeRefs returns the distinct refs by machine type (one representative zone
// per type; specs are identical across a region's zones), sorted for
// determinism.
func dedupeRefs(in []machineTypeRef) []machineTypeRef {
	seen := make(map[string]struct{}, len(in))
	var out []machineTypeRef
	for _, ref := range in {
		if ref.MachineType == "" {
			continue
		}
		if _, ok := seen[ref.MachineType]; ok {
			continue
		}
		seen[ref.MachineType] = struct{}{}
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MachineType < out[j].MachineType })
	return out
}
