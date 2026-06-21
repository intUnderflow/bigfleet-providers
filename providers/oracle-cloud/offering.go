package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// offering is one shape of capacity this provider is allowed to provision: an
// OCI compute shape in an availability domain at a capacity type, up to Count
// slots (the quota the shard may Create against). Resources is the per-replica
// request shape the offering serves (Machine.resources — distinct from
// allocatable, which is the shape's hardware capacity).
//
// For FLEXIBLE shapes (name ends ".Flex") the OCPU/memory are not pinned by the
// shape name, so the operator declares them here (OCPUs / MemoryGB); they drive
// both the launch ShapeConfig and Machine.allocatable. For fixed shapes they are
// taken from the pinned shape table and these fields are ignored.
type offering struct {
	Shape              string            `json:"shape"`
	AvailabilityDomain string            `json:"availability_domain"`
	Capacity           string            `json:"capacity_type"` // on_demand | spot | bare_metal
	Count              int               `json:"count"`
	OCPUs              float64           `json:"ocpus,omitempty"`     // flexible shapes only
	MemoryGB           float64           `json:"memory_gb,omitempty"` // flexible shapes only
	Resources          map[string]string `json:"resources,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
}

func (o offering) capacityType() (providerkit.CapacityType, error) {
	switch strings.ToLower(o.Capacity) {
	case "on_demand", "on-demand", "ondemand", "":
		// Map by the DECLARED capacity, not the shape prefix: OCI bare metal is
		// hourly-billed unless reserved, so a BM.* shape declared on-demand is
		// genuine on-demand capacity (real price, idle-releasable). Declare
		// capacity_type=bare_metal explicitly for a fixed/free-pool BM lane.
		return providerkit.CapacityOnDemand, nil
	case "spot", "preemptible":
		if isBareMetalShape(o.Shape) {
			return providerkit.CapacityUnspecified, fmt.Errorf("bare-metal shape %q cannot be preemptible", o.Shape)
		}
		return providerkit.CapacitySpot, nil
	case "bare_metal", "bare-metal", "metal":
		return providerkit.CapacityBareMetal, nil
	case "reserved":
		// OCI capacity reservations are modelled as on-demand here (the shard's
		// idle-hold for reserved differs, but this provider launches them as
		// ordinary on-demand instances); reject rather than silently mis-declare.
		return providerkit.CapacityUnspecified, fmt.Errorf("capacity_type %q is not modelled by this provider (use on_demand, spot, or bare_metal)", o.Capacity)
	default:
		return providerkit.CapacityUnspecified, fmt.Errorf("unknown capacity_type %q", o.Capacity)
	}
}

// isBareMetalShape reports whether a shape name denotes an OCI bare-metal shape.
func isBareMetalShape(shape string) bool {
	return strings.HasPrefix(shape, "BM.")
}

// isFlexShape reports whether a shape is an OCI flexible shape (OCPU/memory set
// at launch time rather than pinned by the shape name).
func isFlexShape(shape string) bool {
	return strings.HasSuffix(shape, ".Flex")
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

// defaultOfferings spreads seedCount slots across a representative mix of OCI
// shapes (on-demand + preemptible) and availability domains. Used when no
// --offerings file is given — enough for a conformance run to have Speculative
// slots to walk (and to exercise the SPOT interruption invariant), and a
// sensible dev default. Real deployments supply --offerings.
func defaultOfferings(seedCount int, adA, adB string) []offering {
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
		{Shape: "VM.Standard.E5.Flex", AvailabilityDomain: adA, Capacity: "on_demand", Count: counts[0], OCPUs: 2, MemoryGB: 16, Resources: res},
		{Shape: "VM.Standard.E5.Flex", AvailabilityDomain: adA, Capacity: "spot", Count: counts[1], OCPUs: 2, MemoryGB: 16, Resources: res},
		{Shape: "VM.Standard.A1.Flex", AvailabilityDomain: adB, Capacity: "on_demand", Count: counts[2], OCPUs: 2, MemoryGB: 12, Resources: res},
		{Shape: "VM.Standard.E5.Flex", AvailabilityDomain: adB, Capacity: "spot", Count: counts[3], OCPUs: 2, MemoryGB: 16, Resources: res},
	}
}

// shapeLabels adds extra match labels beyond the well-known top-level fields:
// the cpu architecture for Ampere (Arm64) shapes, and an accelerator type for
// GPU shapes. (shape / availability_domain / capacity_type stay top-level; labels
// carry only the extras.)
func shapeLabels(off offering) map[string]string {
	labels := cloneMap(off.Labels)
	add := func(k, v string) {
		if labels == nil {
			labels = map[string]string{}
		}
		labels[k] = v
	}
	if arch, ok := archLabel(off.Shape); ok {
		add("kubernetes.io/arch", arch)
	}
	if acc, ok := acceleratorLabel(off.Shape); ok {
		add("bigfleet.io/accelerator", acc)
	}
	return labels
}

// archLabel reports the CPU architecture for Ampere (Arm64) OCI shape families
// (A1, A2), so the shard can satisfy kubernetes.io/arch node-selectors.
func archLabel(shape string) (string, bool) {
	if strings.Contains(shape, ".A1.") || strings.Contains(shape, ".A2.") ||
		strings.HasPrefix(shape, "BM.Standard.A1") {
		return "arm64", true
	}
	return "", false
}

// acceleratorLabel reports the accelerator type for OCI GPU shape families.
func acceleratorLabel(shape string) (string, bool) {
	switch {
	case strings.Contains(shape, ".GPU.A10"):
		return "nvidia-a10", true
	case strings.Contains(shape, ".GPU.A100"):
		return "nvidia-a100", true
	case strings.Contains(shape, ".GPU4"), strings.Contains(shape, ".GPU.H100"):
		return "nvidia-h100", true
	case strings.Contains(shape, ".GPU"):
		return "nvidia-gpu", true
	default:
		return "", false
	}
}
