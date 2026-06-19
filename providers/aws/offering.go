package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/intUnderflow/bigfleet-providers/internal/providerkit"
)

// offering is one shape of capacity this provider is allowed to provision: an
// instance type in a zone at a capacity type, up to Count slots (the quota the
// shard may Create against). Resources is the per-replica request shape the
// offering serves (Machine.resources — distinct from allocatable, which comes
// from the instance type). Offerings are the cloud analogue of a "free pool":
// each open slot is a Speculative Machine the shard can actuate.
type offering struct {
	InstanceType string            `json:"instance_type"`
	Zone         string            `json:"zone"`
	Capacity     string            `json:"capacity_type"` // on_demand | spot | reserved | bare_metal
	Count        int               `json:"count"`
	Resources    map[string]string `json:"resources,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

func (o offering) capacityType() (providerkit.CapacityType, error) {
	switch strings.ToLower(o.Capacity) {
	case "on_demand", "on-demand", "ondemand", "":
		return providerkit.CapacityOnDemand, nil
	case "spot":
		return providerkit.CapacitySpot, nil
	case "reserved":
		return providerkit.CapacityReserved, nil
	case "bare_metal", "bare-metal", "metal":
		return providerkit.CapacityBareMetal, nil
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
// on-demand and spot types/zones. Used when no --offerings file is given —
// enough for a conformance run to have Speculative slots to walk, and a
// sensible dev default. Real deployments supply --offerings.
func defaultOfferings(seedCount int, zoneA, zoneB string) []offering {
	if seedCount <= 0 {
		seedCount = 16
	}
	// Four buckets; distribute seedCount as evenly as possible.
	base := seedCount / 4
	rem := seedCount % 4
	counts := [4]int{base, base, base, base}
	for i := 0; i < rem; i++ {
		counts[i]++
	}
	return []offering{
		{InstanceType: "m6i.large", Zone: zoneA, Capacity: "on_demand", Count: counts[0],
			Resources: map[string]string{"cpu": "1", "memory": "2Gi"}},
		{InstanceType: "c7g.xlarge", Zone: zoneA, Capacity: "spot", Count: counts[1],
			Resources: map[string]string{"cpu": "1", "memory": "2Gi"}},
		{InstanceType: "m6i.large", Zone: zoneB, Capacity: "on_demand", Count: counts[2],
			Resources: map[string]string{"cpu": "1", "memory": "2Gi"}},
		{InstanceType: "c7g.xlarge", Zone: zoneB, Capacity: "spot", Count: counts[3],
			Resources: map[string]string{"cpu": "1", "memory": "2Gi"}},
	}
}

// acceleratorLabel adds an accelerator-type label for GPU families, so the
// shard can satisfy accelerator node-selectors without re-deriving from the
// instance type. (instance_type / zone / capacity_type stay top-level; labels
// carry only the extras.)
func acceleratorLabel(instanceType string) (string, bool) {
	switch {
	case strings.HasPrefix(instanceType, "g5."), strings.HasPrefix(instanceType, "g6."):
		return "nvidia-a10g", true
	case strings.HasPrefix(instanceType, "p4"), strings.HasPrefix(instanceType, "p5"):
		return "nvidia-a100", true
	default:
		return "", false
	}
}
