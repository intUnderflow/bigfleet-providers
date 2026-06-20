package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// offering is one shape of capacity this provider is allowed to provision: a
// Droplet size in a region at a capacity type, up to Count slots (the quota the
// shard may Create against). Resources is the per-replica request shape the
// offering serves (Machine.resources — distinct from allocatable, which comes
// from the size's hardware). Offerings are the cloud analogue of a "free pool":
// each open slot is a Speculative Machine the shard can actuate.
type offering struct {
	Size      string            `json:"size"`          // DigitalOcean size slug, e.g. s-2vcpu-4gb
	Region    string            `json:"region"`        // DigitalOcean region slug, e.g. nyc3
	Capacity  string            `json:"capacity_type"` // on_demand (DigitalOcean Droplets are on-demand only)
	Count     int               `json:"count"`         // number of Speculative slots
	Resources map[string]string `json:"resources,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

func (o offering) capacityType() (providerkit.CapacityType, error) {
	switch strings.ToLower(o.Capacity) {
	case "on_demand", "on-demand", "ondemand", "":
		return providerkit.CapacityOnDemand, nil
	case "spot":
		// DigitalOcean has no spot/preemptible Droplet product. Reject it loudly
		// rather than silently mis-declaring interruption_probability — a spot
		// offering with the honest ~0 probability would fail the SPOT field-shape
		// rule, and a non-zero one would lie about a product that does not exist.
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not offered by DigitalOcean (on-demand Droplets only)", o.Capacity)
	case "reserved", "bare_metal", "bare-metal", "metal":
		// This binary serves DigitalOcean Droplets, which are regular on-demand
		// instances. Accepting "reserved"/"bare_metal" would mis-declare
		// capacity_type (and, for bare_metal, force price_per_hour=0) while still
		// launching normal on-demand Droplets — skewing the shard's cost ranking
		// and idle-release policy.
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not a DigitalOcean Droplet substrate (on-demand only)", o.Capacity)
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
// DigitalOcean sizes and regions. Used when no --offerings file is given —
// enough for a conformance/certification run to have Speculative slots to walk,
// and a sensible dev default. Real deployments supply --offerings.
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
	return []offering{
		{Size: "s-2vcpu-4gb", Region: regionA, Capacity: "on_demand", Count: counts[0], Resources: res},
		{Size: "s-4vcpu-8gb", Region: regionA, Capacity: "on_demand", Count: counts[1], Resources: res},
		{Size: "s-2vcpu-4gb", Region: regionB, Capacity: "on_demand", Count: counts[2], Resources: res},
		{Size: "s-4vcpu-8gb", Region: regionB, Capacity: "on_demand", Count: counts[3], Resources: res},
	}
}
