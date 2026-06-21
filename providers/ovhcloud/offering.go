package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// offering is one shape of capacity this provider is allowed to provision: a
// flavor in a region at a capacity type, up to Count slots (the quota the shard
// may Create against). Resources is the per-replica request shape the offering
// serves (Machine.resources — distinct from allocatable, which comes from the
// flavor's hardware). Offerings are the cloud analogue of a "free pool": each
// open slot is a Speculative Machine the shard can actuate.
type offering struct {
	Flavor    string            `json:"flavor"`
	Region    string            `json:"region"`
	Capacity  string            `json:"capacity_type"` // on_demand (OVH Public Cloud is on-demand only)
	Count     int               `json:"count"`
	Resources map[string]string `json:"resources,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

func (o offering) capacityType() (providerkit.CapacityType, error) {
	switch strings.ToLower(o.Capacity) {
	case "on_demand", "on-demand", "ondemand", "":
		return providerkit.CapacityOnDemand, nil
	case "spot":
		// OVHcloud has no spot/preemptible market today. Reject it loudly rather
		// than silently mis-declaring interruption_probability (a SPOT machine
		// must declare a real >0 probability the provider cannot honestly offer).
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not offered by OVH Public Cloud (on-demand only; no spot market)", o.Capacity)
	case "reserved":
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not modelled by this provider (OVH Public Cloud instances are on-demand)", o.Capacity)
	case "bare_metal", "bare-metal", "metal":
		// This binary serves the OVH Public Cloud (OpenStack) substrate, which
		// only ever creates regular on-demand instances. Accepting "bare_metal"
		// would mis-declare capacity_type (and force price_per_hour=0) while still
		// launching normal instances. Bare metal is the separate Dedicated Servers
		// substrate (the OVH API), not this provider.
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is the separate OVH Dedicated Servers substrate, not Public Cloud", o.Capacity)
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

// defaultOfferings spreads seedCount slots across a representative mix of OVH
// Public Cloud flavors and regions. Used when no --offerings file is given —
// enough for a conformance run to have Speculative slots to walk, and a sensible
// dev default. Real deployments supply --offerings.
func defaultOfferings(seedCount int, regionA, regionB string) []offering {
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
	// Four distinct (flavor, region) pairs, so the slot ids stay unique even when
	// regionA == regionB (the one-process-per-region case: --region set, no
	// --region-b). Distinct flavors per bucket avoid a (flavor, region) collision.
	return []offering{
		{Flavor: "b2-7", Region: regionA, Capacity: "on_demand", Count: counts[0], Resources: res},
		{Flavor: "c2-15", Region: regionA, Capacity: "on_demand", Count: counts[1], Resources: res},
		{Flavor: "c2-7", Region: regionB, Capacity: "on_demand", Count: counts[2], Resources: res},
		{Flavor: "b2-15", Region: regionB, Capacity: "on_demand", Count: counts[3], Resources: res},
	}
}

// gpuLabel adds an accelerator-type label for OVH GPU flavor families, so the
// shard can satisfy accelerator node-selectors without re-deriving from the
// flavor name. (flavor / region / capacity_type stay top-level; labels carry
// only the extras.) The label value is the actual NVIDIA model the family
// carries, per the OVH Public Cloud GPU catalogue: t1 = V100, t2 = V100S,
// a10 = A10, a100 = A100, l4 = L4, l40s = L40S. Order the a100/l40s cases
// before a10/l4 is unnecessary (the trailing '-' makes the prefixes disjoint),
// but kept explicit for clarity.
func gpuLabel(flavor string) (string, bool) {
	switch {
	case strings.HasPrefix(flavor, "t1-"):
		return "nvidia-v100", true
	case strings.HasPrefix(flavor, "t2-"):
		return "nvidia-v100s", true
	case strings.HasPrefix(flavor, "a100-"):
		return "nvidia-a100", true
	case strings.HasPrefix(flavor, "a10-"):
		return "nvidia-a10", true
	case strings.HasPrefix(flavor, "l40s-"):
		return "nvidia-l40s", true
	case strings.HasPrefix(flavor, "l4-"):
		return "nvidia-l4", true
	default:
		return "", false
	}
}
