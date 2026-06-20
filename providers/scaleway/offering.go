package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// offering is one shape of capacity this provider is allowed to provision: a
// commercial type in a zone at a capacity type, up to Count slots (the quota the
// shard may Create against). Resources is the per-replica request shape the
// offering serves (Machine.resources — distinct from allocatable, which comes
// from the commercial type's hardware). Each open slot is a Speculative Machine
// the shard can actuate.
type offering struct {
	CommercialType string            `json:"commercial_type"`
	Zone           string            `json:"zone"`
	Capacity       string            `json:"capacity_type"` // on_demand (Instances) | bare_metal (Elastic Metal)
	Count          int               `json:"count"`
	Resources      map[string]string `json:"resources,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
}

// capacityType resolves the offering's declared capacity_type. Scaleway has no
// spot/preemptible market, so SPOT is rejected loudly rather than silently
// mis-declaring interruption_probability. Which of {on_demand, bare_metal} is
// valid depends on the running backend mode; backendCapacity enforces that the
// offering matches the process's substrate.
func (o offering) capacityType() (providerkit.CapacityType, error) {
	switch strings.ToLower(o.Capacity) {
	case "on_demand", "on-demand", "ondemand", "":
		return providerkit.CapacityOnDemand, nil
	case "bare_metal", "bare-metal", "metal", "baremetal":
		return providerkit.CapacityBareMetal, nil
	case "spot":
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not offered by Scaleway (no spot/preemptible market; interruption_probability is ~0)", o.Capacity)
	case "reserved":
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not a Scaleway substrate (use on_demand for Instances or bare_metal for Elastic Metal)", o.Capacity)
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

// defaultInstanceOfferings spreads seedCount slots across a representative mix of
// Scaleway Instances commercial types and zones. Used when no --offerings file is
// given for the Instances backend — enough for a certification run to have
// Speculative slots to walk, and a sensible dev default. Real deployments supply
// --offerings.
func defaultInstanceOfferings(seedCount int, zoneA, zoneB string) []offering {
	if seedCount <= 0 {
		seedCount = 16
	}
	base := seedCount / 4
	rem := seedCount % 4
	counts := [4]int{base, base, base, base}
	for i := 0; i < rem; i++ {
		counts[i]++
	}
	res := map[string]string{"cpu": "1", "memory": "2Gi"}
	return []offering{
		{CommercialType: "DEV1-S", Zone: zoneA, Capacity: "on_demand", Count: counts[0], Resources: res},
		{CommercialType: "GP1-XS", Zone: zoneA, Capacity: "on_demand", Count: counts[1], Resources: res},
		{CommercialType: "DEV1-S", Zone: zoneB, Capacity: "on_demand", Count: counts[2], Resources: res},
		{CommercialType: "GP1-XS", Zone: zoneB, Capacity: "on_demand", Count: counts[3], Resources: res},
	}
}

// defaultBaremetalOfferings is the Elastic Metal analogue of
// defaultInstanceOfferings: a small mix of Elastic Metal server types, all
// bare_metal.
func defaultBaremetalOfferings(seedCount int, zoneA, zoneB string) []offering {
	if seedCount <= 0 {
		seedCount = 8
	}
	base := seedCount / 2
	rem := seedCount % 2
	counts := [2]int{base + rem, base}
	res := map[string]string{"cpu": "2", "memory": "4Gi"}
	return []offering{
		{CommercialType: "EM-A210R-HDD", Zone: zoneA, Capacity: "bare_metal", Count: counts[0], Resources: res},
		{CommercialType: "EM-B112X-SSD", Zone: zoneB, Capacity: "bare_metal", Count: counts[1], Resources: res},
	}
}

// archLabel adds a cpu-architecture label for Arm64 families, so the shard can
// satisfy kubernetes.io/arch node-selectors without re-deriving from the
// commercial type. (commercial_type / zone / capacity_type stay top-level;
// labels carry only the extras.)
func archLabel(commercialType string) (string, bool) {
	// Scaleway's Ampere Arm64 Instances line is COPARM1-*.
	if strings.HasPrefix(strings.ToUpper(commercialType), "COPARM") {
		return "arm64", true
	}
	return "", false
}

// gpuLabel reports the accelerator-type label for GPU commercial types, so the
// shard can satisfy accelerator selectors. Scaleway's GPU Instances are the
// RENDER-* and H100-* / L4-* lines.
func gpuLabel(commercialType string) (string, bool) {
	up := strings.ToUpper(commercialType)
	switch {
	case strings.HasPrefix(up, "RENDER"):
		return "nvidia-tesla-p100", true
	case strings.HasPrefix(up, "H100"):
		return "nvidia-h100", true
	case strings.HasPrefix(up, "L4"):
		return "nvidia-l4", true
	default:
		return "", false
	}
}
