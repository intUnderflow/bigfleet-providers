package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// offering is one shape of capacity this provider is allowed to provision: a
// Latitude plan in a site at a capacity type, up to Count slots (the quota the
// shard may Create against). Resources is the per-replica request shape the
// offering serves (Machine.resources — distinct from allocatable, which comes
// from the plan's hardware). Offerings are the cloud analogue of a "free pool":
// each open slot is a Speculative Machine the shard can actuate.
type offering struct {
	Plan      string            `json:"plan"`          // Latitude plan slug, e.g. c2-small-x86
	Site      string            `json:"site"`          // Latitude site slug, e.g. ASH, NYC, LON
	Capacity  string            `json:"capacity_type"` // on_demand (Latitude is on-demand bare metal)
	Count     int               `json:"count"`
	Resources map[string]string `json:"resources,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

func (o offering) capacityType() (providerkit.CapacityType, error) {
	switch strings.ToLower(o.Capacity) {
	case "on_demand", "on-demand", "ondemand", "":
		// Latitude.sh is an on-demand bare-metal cloud with a real Delete. The
		// capacity type is ON_DEMAND (not BARE_METAL): since M73 the shard only
		// emits Delete for ON_DEMAND/SPOT, so BARE_METAL would stop it reclaiming
		// deployed servers and leak money. See docs/index.md.
		return providerkit.CapacityOnDemand, nil
	case "spot":
		// Latitude has no spot/preemptible tier. Reject it loudly rather than
		// silently mis-declaring interruption_probability.
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not offered by Latitude.sh (on-demand only)", o.Capacity)
	case "bare_metal", "bare-metal", "metal", "reserved":
		// Latitude IS physical hardware, but the lifecycle is on-demand with a real
		// Delete, so the capacity type is ON_DEMAND, not BARE_METAL. Accepting
		// "bare_metal" here would set capacity_type=BARE_METAL and stop the shard
		// ever issuing Delete (M73), leaking every deployed server.
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q would suppress the shard's Delete (M73) and leak servers; Latitude is on_demand (a real Delete deprovisions the box)", o.Capacity)
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
// Latitude plans and sites. Used when no --offerings file is given — enough for
// a conformance run to have Speculative slots to walk, and a sensible dev
// default. Real deployments supply --offerings.
func defaultOfferings(seedCount int, siteA, siteB string) []offering {
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
	// A small per-replica request shape so a bare-metal plan packs many replicas
	// (density = floor(allocatable / resources) >> 1).
	res := map[string]string{"cpu": "1", "memory": "2Gi"}
	return []offering{
		{Plan: "c2-small-x86", Site: siteA, Capacity: "on_demand", Count: counts[0], Resources: res},
		{Plan: "c3-large-x86", Site: siteA, Capacity: "on_demand", Count: counts[1], Resources: res},
		{Plan: "c2-small-x86", Site: siteB, Capacity: "on_demand", Count: counts[2], Resources: res},
		{Plan: "c3-large-x86", Site: siteB, Capacity: "on_demand", Count: counts[3], Resources: res},
	}
}

// acceleratorLabel adds an accelerator-type label for GPU plan families, so the
// shard can satisfy accelerator node-selectors without re-deriving from the plan
// slug. (plan / site / capacity_type stay top-level; labels carry only extras.)
func acceleratorLabel(plan string) (string, bool) {
	p := strings.ToLower(plan)
	switch {
	case strings.Contains(p, "h100"):
		return "nvidia-h100", true
	case strings.Contains(p, "l40s"):
		return "nvidia-l40s", true
	case strings.Contains(p, "a100"):
		return "nvidia-a100", true
	case strings.HasPrefix(p, "g3"), strings.HasPrefix(p, "g4"):
		// Latitude g-series are GPU plans; mark them generically when no specific
		// accelerator is recognised in the slug.
		return "gpu", true
	}
	return "", false
}
