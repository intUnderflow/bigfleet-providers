package main

import (
	"fmt"
	"sort"
	"strconv"
)

// vmCapacity is the hardware shape of an instance type: the vCPU count, the
// memory the clone is given, and the source template VMID to clone. It populates
// Machine.allocatable (the full per-machine capacity) and the clone's cores /
// memory.
//
// allocatable is the substrate's full hardware capacity — NOT the per-replica
// request shape (that is the offering's Resources). Density =
// floor(allocatable / resources), so these two must never be set equal.
type vmCapacity struct {
	VCPU         int
	MemMiB       int64
	TemplateVMID int // the golden template this instance type clones from
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

// instanceCatalog is the Proxmox instance-type catalog: a small, named set of VM
// flavors, each mapping to a clone's vCPU/memory, its source template VMID, and
// Machine.allocatable. Unlike a cloud catalog there is no upstream API to read
// it from, so it is declared in config (or this built-in default). An operator
// overrides it with --instance-types pointing at a JSON map of
// name -> {vcpu, memory_mib, template_vmid}.
type instanceCatalog struct {
	types map[string]vmCapacity
}

// defaultTemplateVMID is the template every default catalog entry clones from
// when no per-type template is configured. An operator points it at their
// prepared template (qemu-guest-agent + kubelet installed) via --template-vmid
// or an --instance-types file.
const defaultTemplateVMID = 9000

// newDefaultInstanceTypes builds the built-in flavor catalog (name -> hardware
// shape), all cloning from the given template VMID. Sizes double cleanly so
// density math stays legible in docs and tests.
func newDefaultInstanceTypes(templateVMID int) map[string]vmCapacity {
	return map[string]vmCapacity{
		"pve.small":  {VCPU: 2, MemMiB: gib(4), TemplateVMID: templateVMID},
		"pve.medium": {VCPU: 4, MemMiB: gib(8), TemplateVMID: templateVMID},
		"pve.large":  {VCPU: 8, MemMiB: gib(16), TemplateVMID: templateVMID},
		"pve.xlarge": {VCPU: 16, MemMiB: gib(32), TemplateVMID: templateVMID},
	}
}

func newInstanceCatalog(types map[string]vmCapacity, defaultTemplate int) *instanceCatalog {
	if len(types) == 0 {
		types = newDefaultInstanceTypes(defaultTemplate)
	}
	cp := make(map[string]vmCapacity, len(types))
	for k, v := range types {
		if v.TemplateVMID == 0 {
			v.TemplateVMID = defaultTemplate
		}
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
