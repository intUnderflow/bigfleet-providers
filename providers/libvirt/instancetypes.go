package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// vmCapacity is the hardware shape of an instance type: the vCPU count and the
// on-box memory the libvirt domain is given. It populates Machine.allocatable
// (the full per-machine capacity) and drives the domain XML (vCPU / memory).
//
// allocatable is the substrate's full hardware capacity — NOT the per-replica
// request shape (that is the offering's Resources). Density =
// floor(allocatable / resources), so these two must never be set equal.
type vmCapacity struct {
	VCPU   int
	MemMiB int64
}

// gib converts a whole number of GiB to MiB, for the catalog literals.
func gib(n int64) int64 { return n * 1024 }

// allocatable renders the capacity as a Kubernetes-style resource map
// (deterministic strings). Memory renders as Gi when it is a whole number of
// GiB, else Mi, so the value round-trips without precision loss.
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

// instanceCatalog is the libvirt instance-type catalog: a small, named set of
// VM flavors, each mapping to a domain's vCPU/memory and to Machine.allocatable.
// Unlike a cloud catalog there is no upstream API to read it from, so it is
// declared in config (or this built-in default). An operator can override it
// with --instance-types pointing at a JSON map of name -> {vcpu, memory_mib}.
//
// The default mirrors a typical KVM offering: a few shared-everything sizes
// from small to large, so a conformance run and a dev deployment both have a
// representative spread of instance types.
type instanceCatalog struct {
	types map[string]vmCapacity
}

// defaultInstanceTypes is the built-in flavor catalog (name -> hardware shape).
// Sizes double cleanly so density math stays legible in docs and tests.
var defaultInstanceTypes = map[string]vmCapacity{
	"kvm.small":  {VCPU: 2, MemMiB: gib(4)},
	"kvm.medium": {VCPU: 4, MemMiB: gib(8)},
	"kvm.large":  {VCPU: 8, MemMiB: gib(16)},
	"kvm.xlarge": {VCPU: 16, MemMiB: gib(32)},
}

func newInstanceCatalog(types map[string]vmCapacity) *instanceCatalog {
	if len(types) == 0 {
		types = defaultInstanceTypes
	}
	cp := make(map[string]vmCapacity, len(types))
	for k, v := range types {
		cp[k] = v
	}
	return &instanceCatalog{types: cp}
}

// capacity returns the hardware shape for an instance type, and whether it is
// known. Read-only and non-blocking — safe on the List/seed hot path.
func (c *instanceCatalog) capacity(instanceType string) (vmCapacity, bool) {
	v, ok := c.types[instanceType]
	return v, ok
}

// allocatable returns the resource map for an instance type, or nil if the type
// is unknown (the kit then treats allocatable == resources).
func (c *instanceCatalog) allocatable(instanceType string) map[string]string {
	v, ok := c.types[instanceType]
	if !ok {
		return nil
	}
	return v.allocatable()
}

// has reports whether the catalog defines the instance type.
func (c *instanceCatalog) has(instanceType string) bool {
	_, ok := c.types[instanceType]
	return ok
}

// names returns the catalog's instance-type names, sorted (for deterministic
// default offerings and test assertions).
func (c *instanceCatalog) names() []string {
	out := make([]string, 0, len(c.types))
	for k := range c.types {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseInstanceType parses a "vcpu/memory" spec like "4/8Gi" into a vmCapacity,
// used by --instance-types entries supplied as compact strings on the CLI. The
// JSON form ({"vcpu":4,"memory_mib":8192}) is handled in config.go.
func parseInstanceType(spec string) (vmCapacity, error) {
	parts := strings.SplitN(spec, "/", 2)
	if len(parts) != 2 {
		return vmCapacity{}, fmt.Errorf("instance type spec %q must be vcpu/memory (e.g. 4/8Gi)", spec)
	}
	vcpu, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || vcpu <= 0 {
		return vmCapacity{}, fmt.Errorf("instance type spec %q: bad vcpu", spec)
	}
	mib, err := parseMemMiB(strings.TrimSpace(parts[1]))
	if err != nil {
		return vmCapacity{}, fmt.Errorf("instance type spec %q: %w", spec, err)
	}
	return vmCapacity{VCPU: vcpu, MemMiB: mib}, nil
}

// parseMemMiB parses a memory quantity (Gi/Mi suffix, or a bare MiB number)
// into MiB.
func parseMemMiB(s string) (int64, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasSuffix(s, "Gi"):
		n, err := strconv.ParseInt(strings.TrimSuffix(s, "Gi"), 10, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("bad memory %q", s)
		}
		return gib(n), nil
	case strings.HasSuffix(s, "Mi"):
		n, err := strconv.ParseInt(strings.TrimSuffix(s, "Mi"), 10, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("bad memory %q", s)
		}
		return n, nil
	default:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("bad memory %q (want Gi/Mi suffix or a MiB number)", s)
		}
		return n, nil
	}
}
