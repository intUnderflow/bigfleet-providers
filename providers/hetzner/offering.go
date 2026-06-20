package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// offering is one shape of capacity this provider is allowed to provision: a
// server type in a location at a capacity type, up to Count slots (the quota the
// shard may Create against). Resources is the per-replica request shape the
// offering serves (Machine.resources — distinct from allocatable, which comes
// from the server type's hardware). Offerings are the cloud analogue of a "free
// pool": each open slot is a Speculative Machine the shard can actuate.
type offering struct {
	ServerType string            `json:"server_type"`
	Location   string            `json:"location"`
	Capacity   string            `json:"capacity_type"` // on_demand (Hetzner Cloud is on-demand only)
	Count      int               `json:"count"`
	Resources  map[string]string `json:"resources,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

func (o offering) capacityType() (providerkit.CapacityType, error) {
	switch strings.ToLower(o.Capacity) {
	case "on_demand", "on-demand", "ondemand", "":
		return providerkit.CapacityOnDemand, nil
	case "spot":
		// Hetzner Cloud has no spot tier today. Reject it loudly rather than
		// silently mis-declaring interruption_probability.
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not offered by Hetzner Cloud (on-demand only)", o.Capacity)
	case "reserved", "bare_metal", "bare-metal", "metal":
		// This binary serves the Hetzner CLOUD substrate, which only ever creates
		// regular on-demand servers. Accepting "reserved"/"bare_metal" would
		// mis-declare capacity_type (and, for bare_metal, force price_per_hour=0)
		// while still launching normal on-demand servers — skewing the shard's
		// cost ranking and idle-release policy. Bare metal is the separate Robot
		// substrate, not this provider.
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not a Hetzner Cloud substrate (on-demand only; bare-metal is the separate Robot provider)", o.Capacity)
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
// Hetzner Cloud server types and locations. Used when no --offerings file is
// given — enough for a conformance run to have Speculative slots to walk, and a
// sensible dev default. Real deployments supply --offerings.
func defaultOfferings(seedCount int, locA, locB string) []offering {
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
		{ServerType: "cx22", Location: locA, Capacity: "on_demand", Count: counts[0], Resources: res},
		{ServerType: "cpx41", Location: locA, Capacity: "on_demand", Count: counts[1], Resources: res},
		{ServerType: "cx22", Location: locB, Capacity: "on_demand", Count: counts[2], Resources: res},
		{ServerType: "cpx41", Location: locB, Capacity: "on_demand", Count: counts[3], Resources: res},
	}
}

// archLabel adds a cpu-architecture label for Arm64 server families (cax), so
// the shard can satisfy kubernetes.io/arch node-selectors without re-deriving
// from the server type. (server_type / location / capacity_type stay top-level;
// labels carry only the extras.)
func archLabel(serverType string) (string, bool) {
	if strings.HasPrefix(serverType, "cax") {
		return "arm64", true
	}
	return "", false
}
