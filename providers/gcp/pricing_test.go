package main

import (
	"testing"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

func TestPricing_OnDemandAndSpot(t *testing.T) {
	p := newPricing("us-central1")
	od := p.price("n2-standard-4", providerkit.CapacityOnDemand)
	if od <= 0 {
		t.Fatalf("on-demand price = %v, want > 0", od)
	}
	spot := p.price("n2-standard-4", providerkit.CapacitySpot)
	if spot <= 0 {
		t.Errorf("spot price = %v, want > 0 (never falsely-cheap zero)", spot)
	}
	if spot >= od {
		t.Errorf("spot price %v should be cheaper than on-demand %v", spot, od)
	}
}

func TestPricing_UnknownRegionFallsBackToBaseline(t *testing.T) {
	p := newPricing("europe-west99")
	if p.price("n2-standard-4", providerkit.CapacityOnDemand) <= 0 {
		t.Error("unknown region should fall back to the baseline table, not zero")
	}
}

func TestInterruption_SpotNeverZero(t *testing.T) {
	in := newInterruption()
	// Known family.
	if got := in.probability("m1", "n2-standard-4", providerkit.CapacitySpot); got <= 0 {
		t.Errorf("spot probability = %v, want > 0", got)
	}
	// Unknown family still non-zero.
	if got := in.probability("m2", "zz-weird-8", providerkit.CapacitySpot); got != defaultSpotProbability {
		t.Errorf("unknown spot family probability = %v, want %v", got, defaultSpotProbability)
	}
	// On-demand is exactly 0.
	if got := in.probability("m3", "n2-standard-4", providerkit.CapacityOnDemand); got != 0 {
		t.Errorf("on-demand probability = %v, want 0", got)
	}
	// Observed preemption raises it toward 1.0.
	in.markPreempted("m1", 0.9)
	if got := in.probability("m1", "n2-standard-4", providerkit.CapacitySpot); got != 0.9 {
		t.Errorf("observed probability = %v, want 0.9", got)
	}
}

func TestMachineTypeResolver_AllocatableDensity(t *testing.T) {
	r := newMachineTypeResolver(newGCEFake(), quietLogger())
	alloc := r.allocatable("n2-standard-4")
	if alloc["cpu"] != "4" || alloc["memory"] != "16Gi" {
		t.Errorf("n2-standard-4 allocatable = %v, want cpu=4 memory=16Gi", alloc)
	}
	// resources (per-replica) must be distinct from allocatable so density > 1.
	if alloc["cpu"] == "1" {
		t.Error("allocatable cpu must reflect hardware, not the per-replica request")
	}
	if r.allocatable("totally-unknown") != nil {
		t.Error("unknown machine type should resolve to nil allocatable")
	}
}

func TestInstanceName_StableAndDNSSafe(t *testing.T) {
	// A retried Insert (same operation id) must derive the same instance name, so
	// a transport retry maps to the existing instance instead of a duplicate.
	spec := instanceSpec{MachineID: "gcp-test/Spot/n2-standard-8/us-central1-a/000", IdempotencyToken: "op-42"}
	a, b := instanceName(spec), instanceName(spec)
	if a != b {
		t.Errorf("instanceName not stable: %q vs %q", a, b)
	}
	if len(a) > 63 || a[:3] != "bf-" {
		t.Errorf("instanceName %q is not a valid GCE name (<=63, bf- prefix)", a)
	}
	// A fresh operation id yields a distinct name (re-Create after Delete).
	if c := instanceName(instanceSpec{MachineID: spec.MachineID, IdempotencyToken: "op-43"}); c == a {
		t.Errorf("distinct operation ids must yield distinct names; both %q", a)
	}
}
