package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// offering is one shape of capacity this provider is allowed to provision: an
// UpCloud plan in a zone at a capacity type, up to Count slots (the quota the
// shard may Create against). Resources is the per-replica request shape the
// offering serves (Machine.resources — distinct from allocatable, which comes
// from the plan's hardware). Offerings are the cloud analogue of a "free pool":
// each open slot is a Speculative Machine the shard can actuate.
type offering struct {
	Plan      string            `json:"plan"`          // UpCloud plan name, e.g. 2xCPU-4GB
	Zone      string            `json:"zone"`          // UpCloud zone id, e.g. fi-hel1
	Capacity  string            `json:"capacity_type"` // on_demand (UpCloud is on-demand only)
	Count     int               `json:"count"`         // number of Speculative slots
	Resources map[string]string `json:"resources,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	// PriceUSDPerHour optionally overrides the pinned price table with an
	// operator-declared USD/hour figure for this offering. Zero falls back to the
	// pinned plan table converted at the configured EUR->USD rate.
	PriceUSDPerHour float64 `json:"price_usd_per_hour,omitempty"`
}

func (o offering) capacityType() (providerkit.CapacityType, error) {
	switch strings.ToLower(o.Capacity) {
	case "on_demand", "on-demand", "ondemand", "":
		return providerkit.CapacityOnDemand, nil
	case "spot":
		// UpCloud has no spot/preemptible product. Reject it loudly rather than
		// silently mis-declaring interruption_probability — a spot offering with the
		// honest ~0 probability would fail the SPOT field-shape rule, and a non-zero
		// one would lie about a product that does not exist.
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not offered by UpCloud (on-demand cloud servers only)", o.Capacity)
	case "reserved", "bare_metal", "bare-metal", "metal":
		// This binary serves UpCloud cloud servers, which are regular on-demand
		// instances. Accepting "reserved"/"bare_metal" would mis-declare
		// capacity_type (and, for bare_metal, force price_per_hour=0) while still
		// launching normal on-demand servers — skewing the shard's cost ranking and
		// idle-release policy.
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not an UpCloud substrate (on-demand cloud servers only)", o.Capacity)
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
// UpCloud plans and zones. Used when no --offerings file is given — enough for a
// conformance / certification run to have Speculative slots to walk, and a
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
		{Plan: "2xCPU-4GB", Zone: zoneA, Capacity: "on_demand", Count: counts[0], Resources: res},
		{Plan: "4xCPU-8GB", Zone: zoneA, Capacity: "on_demand", Count: counts[1], Resources: res},
		{Plan: "2xCPU-4GB", Zone: zoneB, Capacity: "on_demand", Count: counts[2], Resources: res},
		{Plan: "4xCPU-8GB", Zone: zoneB, Capacity: "on_demand", Count: counts[3], Resources: res},
	}
}
