package main

import (
	"fmt"
	"sort"
	"strconv"
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
