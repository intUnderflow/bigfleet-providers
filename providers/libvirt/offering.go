package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// offering is one shape of capacity this provider may provision: an instance
// type on a libvirt host (zone) at a capacity type, up to Count slots (the quota
// the shard may Create against). Resources is the per-replica request shape the
// offering serves (Machine.resources — distinct from allocatable, which is the
// instance type's full hardware). Each open slot is a Speculative Machine the
// shard can actuate into a real VM.
type offering struct {
	InstanceType string            `json:"instance_type"`
	Zone         string            `json:"zone"` // the libvirt host this slot lands on
	Capacity     string            `json:"capacity_type"`
	Count        int               `json:"count"`
	Resources    map[string]string `json:"resources,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// capacityType maps the offering's declared capacity to a kit CapacityType.
// libvirt VMs are local, always-reachable hardware, so the honest choices are
// on_demand (a churning pool where Delete tears the VM down — the default, and
// what enables the cloud conformance profile) or bare_metal (a fixed free pool
// that never receives Delete). spot is rejected: a single libvirt host has no
// preemption market, so declaring it would force a fictional interruption
// probability.
func (o offering) capacityType() (providerkit.CapacityType, error) {
	switch strings.ToLower(o.Capacity) {
	case "on_demand", "on-demand", "ondemand", "":
		return providerkit.CapacityOnDemand, nil
	case "bare_metal", "bare-metal", "metal":
		return providerkit.CapacityBareMetal, nil
	case "reserved":
		return providerkit.CapacityReserved, nil
	case "spot":
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not meaningful for libvirt (a local host has no preemption market); use on_demand or bare_metal", o.Capacity)
	default:
		return providerkit.CapacityUnspecified, fmt.Errorf("unknown capacity_type %q", o.Capacity)
	}
}

// loadOfferings reads offerings from a JSON file (an array of offering).
func loadOfferings(path string) ([]offering, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read offerings %s: %w", path, err)
	}
	var offs []offering
	if err := json.Unmarshal(data, &offs); err != nil {
		return nil, fmt.Errorf("parse offerings %s: %w", path, err)
	}
	if len(offs) == 0 {
		return nil, fmt.Errorf("offerings %s is empty", path)
	}
	return offs, nil
}

// defaultOfferings spreads seedCount slots across a representative mix of
// instance types on the configured hosts (zones). Used when no --offerings file
// is given — enough for a conformance run to have Speculative slots to walk, and
// a sensible dev default. Real deployments supply --offerings.
//
// capacity is the pool's capacity type (on_demand by default); types is the
// instance-type catalog the slots draw from.
func defaultOfferings(seedCount int, hostA, hostB, capacity string, types []string) []offering {
	if seedCount <= 0 {
		seedCount = 16
	}
	if len(types) == 0 {
		types = []string{"kvm.small", "kvm.large"}
	}
	// Pick two representative sizes (small + a larger one) so the spread covers
	// distinct densities; fall back to whatever the catalog has.
	small := types[0]
	large := types[len(types)-1]

	// Four buckets across two sizes × two hosts; distribute seedCount evenly.
	base := seedCount / 4
	rem := seedCount % 4
	counts := [4]int{base, base, base, base}
	for i := 0; i < rem; i++ {
		counts[i]++
	}
	res := map[string]string{"cpu": "1", "memory": "2Gi"}
	return []offering{
		{InstanceType: small, Zone: hostA, Capacity: capacity, Count: counts[0], Resources: res},
		{InstanceType: large, Zone: hostA, Capacity: capacity, Count: counts[1], Resources: res},
		{InstanceType: small, Zone: hostB, Capacity: capacity, Count: counts[2], Resources: res},
		{InstanceType: large, Zone: hostB, Capacity: capacity, Count: counts[3], Resources: res},
	}
}
