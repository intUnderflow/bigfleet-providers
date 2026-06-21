package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// offering is one shape of capacity this provider is allowed to provision: a
// machine type in a zone at a capacity type, up to Count slots (the quota the
// shard may Create against). Resources is the per-replica request shape the
// offering serves (Machine.resources — distinct from allocatable, which comes
// from the machine type's hardware). Offerings are the cloud analogue of a
// "free pool": each open slot is a Speculative Machine the shard can actuate.
type offering struct {
	MachineType string            `json:"machine_type"`
	Zone        string            `json:"zone"`
	Capacity    string            `json:"capacity_type"` // on_demand | spot | reserved
	Count       int               `json:"count"`
	Resources   map[string]string `json:"resources,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

func (o offering) capacityType() (providerkit.CapacityType, error) {
	switch strings.ToLower(o.Capacity) {
	case "on_demand", "on-demand", "ondemand", "":
		return providerkit.CapacityOnDemand, nil
	case "spot":
		return providerkit.CapacitySpot, nil
	case "reserved":
		// A committed-use / capacity-reservation slot. The provider still
		// launches a regular on-demand instance against the reservation, so the
		// substrate call is the same; only the cost category differs.
		return providerkit.CapacityReserved, nil
	case "bare_metal", "bare-metal", "metal":
		// This binary serves the GCE substrate, which only ever creates regular
		// VMs. Accepting "bare_metal" would mis-declare capacity_type (and force
		// price_per_hour=0) while still launching a normal VM — skewing the
		// shard's cost ranking and idle-release policy.
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not a GCE substrate (VMs are on_demand/spot/reserved)", o.Capacity)
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
// on-demand and spot GCE machine types and zones. Used when no --offerings file
// is given — enough for a certification run to have Speculative slots to walk
// (including SPOT, so the SPOT interruption-probability invariant fires), and a
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
	res := map[string]string{"cpu": "1", "memory": "2Gi"}
	return []offering{
		{MachineType: "n2-standard-4", Zone: zoneA, Capacity: "on_demand", Count: counts[0], Resources: res},
		{MachineType: "n2-standard-4", Zone: zoneA, Capacity: "spot", Count: counts[1], Resources: res},
		{MachineType: "n2-standard-8", Zone: zoneB, Capacity: "on_demand", Count: counts[2], Resources: res},
		{MachineType: "c2-standard-8", Zone: zoneB, Capacity: "spot", Count: counts[3], Resources: res},
	}
}

// acceleratorLabel adds an accelerator-type label for GPU families (a2 / g2),
// so the shard can satisfy accelerator node-selectors without re-deriving from
// the machine type. (machine_type / zone / capacity_type stay top-level; labels
// carry only the extras.)
func acceleratorLabel(machineType string) (string, bool) {
	switch {
	case strings.HasPrefix(machineType, "a2-"):
		return "nvidia-a100", true
	case strings.HasPrefix(machineType, "a3-"):
		return "nvidia-h100", true
	case strings.HasPrefix(machineType, "g2-"):
		return "nvidia-l4", true
	default:
		return "", false
	}
}
