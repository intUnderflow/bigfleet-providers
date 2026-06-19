package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
)

// instanceCapacity is the real hardware capacity of an EC2 instance type, used
// to populate Machine.allocatable (ADR-0022: the aggregate the engine's deficit
// math compares against demand; density = floor(allocatable / resources)).
// Memory is held in MiB — the authoritative unit AWS reports
// (MemoryInfo.SizeInMiB) — so a type whose memory is not a whole GiB (e.g.
// t3.nano at 512 MiB) resolves exactly instead of truncating to 0 GiB.
type instanceCapacity struct {
	VCPU   int
	MemMiB int64
}

// gib converts a whole number of GiB to MiB, for the pinned table literals.
func gib(n int64) int64 { return n * 1024 }

// allocatable renders the capacity as a Kubernetes-style resource map
// (deterministic strings). Memory renders as Gi when it is a whole number of
// GiB, else Mi, so both the pinned table and DescribeInstanceTypes round-trip
// without precision loss.
func (c instanceCapacity) allocatable() map[string]string {
	mem := fmt.Sprintf("%dMi", c.MemMiB)
	if c.MemMiB%1024 == 0 {
		mem = fmt.Sprintf("%dGi", c.MemMiB/1024)
	}
	return map[string]string{
		"cpu":    strconv.Itoa(c.VCPU),
		"memory": mem,
	}
}

// instanceTypeTable is a pinned fallback snapshot of common instance types. The
// resolver seeds its cache from this, so the provider produces correct
// allocatable offline (the fake backend, credential-free conformance) and
// survives a DescribeInstanceTypes outage; live AWS data overlays it for
// authoritative, complete coverage of whatever types an operator offers.
var instanceTypeTable = map[string]instanceCapacity{
	// General purpose (m6i / m7g).
	"m6i.large":   {2, gib(8)},
	"m6i.xlarge":  {4, gib(16)},
	"m6i.2xlarge": {8, gib(32)},
	"m6i.4xlarge": {16, gib(64)},
	"m6i.8xlarge": {32, gib(128)},
	"m7g.large":   {2, gib(8)},
	"m7g.xlarge":  {4, gib(16)},
	"m7g.2xlarge": {8, gib(32)},
	"m7g.4xlarge": {16, gib(64)},
	// Compute optimised (c6i / c7g).
	"c6i.large":   {2, gib(4)},
	"c6i.xlarge":  {4, gib(8)},
	"c6i.2xlarge": {8, gib(16)},
	"c6i.4xlarge": {16, gib(32)},
	"c7g.large":   {2, gib(4)},
	"c7g.xlarge":  {4, gib(8)},
	"c7g.2xlarge": {8, gib(16)},
	"c7g.4xlarge": {16, gib(32)},
	// Memory optimised (r6i).
	"r6i.large":   {2, gib(16)},
	"r6i.xlarge":  {4, gib(32)},
	"r6i.2xlarge": {8, gib(64)},
	"r6i.4xlarge": {16, gib(128)},
	// GPU (g5) — accelerator types carry a label too (see offering.go).
	"g5.xlarge":   {4, gib(16)},
	"g5.2xlarge":  {8, gib(32)},
	"g5.4xlarge":  {16, gib(64)},
	"g5.12xlarge": {48, gib(192)},
}

// instanceTypeResolver maps an instance type to its hardware capacity for
// Machine.allocatable. Reads are lock-guarded and never block on the network
// (safe on the List/seed hot path); authoritative AWS data is fetched
// out-of-band by resolve() (at startup) and overlaid on the pinned fallback
// table. A type that is neither pinned nor resolved yields nil, which the
// engine treats as allocatable == resources.
type instanceTypeResolver struct {
	ec2    ec2Client
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]instanceCapacity
}

func newInstanceTypeResolver(ec2 ec2Client, logger *slog.Logger) *instanceTypeResolver {
	cache := make(map[string]instanceCapacity, len(instanceTypeTable))
	for k, v := range instanceTypeTable {
		cache[k] = v
	}
	return &instanceTypeResolver{ec2: ec2, logger: logger, cache: cache}
}

// allocatable returns the resource map for an instance type, or nil if the type
// is unknown. Read-only and non-blocking — safe on the List/seed hot path.
func (r *instanceTypeResolver) allocatable(instanceType string) map[string]string {
	r.mu.RLock()
	c, ok := r.cache[instanceType]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	return c.allocatable()
}

// resolve fetches authoritative capacity from ec2:DescribeInstanceTypes for the
// given types and overlays it on the cache. Best-effort: call at startup. EC2
// instance-type specs are immutable, so one resolution per process lifetime is
// enough. Returns the number of types it could not resolve (each still covered
// by the pinned table if present). Never call on the List hot path.
func (r *instanceTypeResolver) resolve(ctx context.Context, instanceTypes []string) int {
	want := dedupeNonEmpty(instanceTypes)
	if len(want) == 0 {
		return 0
	}
	caps, err := r.ec2.DescribeInstanceCapacities(ctx, want)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("instance-type resolve failed; using pinned table",
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

// dedupeNonEmpty returns the distinct non-empty strings, sorted for
// determinism (stable DescribeInstanceTypes batching + test assertions).
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
