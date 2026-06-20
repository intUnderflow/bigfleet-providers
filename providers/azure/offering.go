package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// offering is one shape of capacity this provider is allowed to provision: a VM
// size in a zone at a capacity type, up to Count slots (the quota the shard may
// Create against). Resources is the per-replica request shape the offering
// serves (Machine.resources — distinct from allocatable, which comes from the VM
// size's hardware). Offerings are the cloud analogue of a "free pool": each open
// slot is a Speculative Machine the shard can actuate.
type offering struct {
	VMSize    string            `json:"vm_size"`
	Zone      string            `json:"zone"`
	Capacity  string            `json:"capacity_type"` // on_demand | spot | reserved
	Count     int               `json:"count"`
	Resources map[string]string `json:"resources,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
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
		// This binary serves standalone Azure Virtual Machines, which are always
		// billed (Spot, pay-as-you-go, or reservation-backed). Accepting
		// "bare_metal" would mis-declare capacity_type and force price_per_hour=0
		// while still creating a normal, billed VM — skewing the shard's cost
		// ranking and idle-release policy. Azure has no free bare-metal pool here.
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not an Azure VM substrate (VMs are always billed: on_demand | spot | reserved)", o.Capacity)
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

// defaultOfferings spreads seedCount slots across a representative mix of Azure
// VM sizes and zones, including a SPOT bucket so the interruption-probability
// path is exercised. Used when no --offerings file is given — enough for a
// conformance run to have Speculative slots to walk, and a sensible dev default.
// Real deployments supply --offerings.
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
		{VMSize: "Standard_D4s_v5", Zone: zoneA, Capacity: "on_demand", Count: counts[0], Resources: res},
		{VMSize: "Standard_F8s_v2", Zone: zoneA, Capacity: "spot", Count: counts[1], Resources: res},
		{VMSize: "Standard_D4s_v5", Zone: zoneB, Capacity: "on_demand", Count: counts[2], Resources: res},
		{VMSize: "Standard_F8s_v2", Zone: zoneB, Capacity: "spot", Count: counts[3], Resources: res},
	}
}

// acceleratorLabel adds an accelerator-type label for GPU VM families, so the
// shard can satisfy accelerator node-selectors without re-deriving from the VM
// size. (vm_size / zone / capacity_type stay top-level; labels carry only the
// extras.)
func acceleratorLabel(vmSize string) (string, bool) {
	u := strings.ToUpper(vmSize)
	switch {
	case strings.Contains(u, "_A100"), strings.HasPrefix(u, "STANDARD_NC24ADS_A100"):
		return "nvidia-a100", true
	case strings.Contains(u, "_H100"), strings.Contains(u, "ND_H100"):
		return "nvidia-h100", true
	case strings.HasPrefix(u, "STANDARD_NC"), strings.HasPrefix(u, "STANDARD_ND"), strings.HasPrefix(u, "STANDARD_NV"):
		return "nvidia", true
	default:
		return "", false
	}
}
