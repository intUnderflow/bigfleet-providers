package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// offering is one shape of capacity this provider may provision: an instance
// type on a Proxmox node (zone), up to Count slots (the quota the shard may
// Create against). Resources is the per-replica request shape the offering
// serves (Machine.resources — distinct from allocatable, which is the instance
// type's full hardware). Each open slot is a Speculative Machine the shard can
// actuate into a real VM.
type offering struct {
	InstanceType string            `json:"instance_type"`
	Zone         string            `json:"zone"` // the Proxmox node this slot lands on
	Capacity     string            `json:"capacity_type,omitempty"`
	Count        int               `json:"count"`
	Resources    map[string]string `json:"resources,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// capacityType maps the offering's declared capacity to a kit CapacityType.
// Proxmox VMs are clone-on-demand and destroy-on-Delete, so the only honest
// choice is on_demand (which enables the cloud conformance profile). A
// self-hosted cluster has no preemption market (so spot is rejected) and no
// reservation/commitment billing (so reserved is rejected); modelling a
// bare_metal free pool that never receives Delete would also misrepresent these
// deletable VMs, so it is rejected too.
func (o offering) capacityType() (providerkit.CapacityType, error) {
	switch strings.ToLower(o.Capacity) {
	case "on_demand", "on-demand", "ondemand", "":
		return providerkit.CapacityOnDemand, nil
	case "spot":
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not meaningful for Proxmox (a self-hosted cluster has no preemption market); use on_demand", o.Capacity)
	case "reserved":
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not meaningful for Proxmox (no reservation/commitment billing); use on_demand", o.Capacity)
	case "bare_metal", "bare-metal", "metal":
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not meaningful for Proxmox (these are deletable clones, not a fixed free pool); use on_demand", o.Capacity)
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
// instance types on the configured nodes (zones). Used when no --offerings file
// is given — enough for a conformance run to have Speculative slots to walk, and
// a sensible dev default. Real deployments supply --offerings.
//
// types is the instance-type catalog the slots draw from; nodeA/nodeB are the
// two Proxmox nodes (zones) to spread across.
func defaultOfferings(seedCount int, nodeA, nodeB string, types []string) []offering {
	if seedCount <= 0 {
		seedCount = 16
	}
	if len(types) == 0 {
		types = []string{"pve.small", "pve.large"}
	}
	// Pick two representative sizes (small + a larger one) so the spread covers
	// distinct densities; fall back to whatever the catalog has.
	small := types[0]
	large := types[len(types)-1]

	// Four buckets across two sizes × two nodes; distribute seedCount evenly.
	base := seedCount / 4
	rem := seedCount % 4
	counts := [4]int{base, base, base, base}
	for i := 0; i < rem; i++ {
		counts[i]++
	}
	res := map[string]string{"cpu": "1", "memory": "2Gi"}
	candidates := []offering{
		{InstanceType: small, Zone: nodeA, Capacity: "on_demand", Count: counts[0], Resources: res},
		{InstanceType: large, Zone: nodeA, Capacity: "on_demand", Count: counts[1], Resources: res},
		{InstanceType: small, Zone: nodeB, Capacity: "on_demand", Count: counts[2], Resources: res},
		{InstanceType: large, Zone: nodeB, Capacity: "on_demand", Count: counts[3], Resources: res},
	}
	// Drop empty buckets so a small --seed-count (< 4) never emits a count==0
	// offering (which the backend rejects). With seedCount >= 1 at least one
	// bucket is always populated.
	out := make([]offering, 0, len(candidates))
	for _, off := range candidates {
		if off.Count > 0 {
			out = append(out, off)
		}
	}
	return out
}
