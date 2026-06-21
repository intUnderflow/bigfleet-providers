package main

import (
	"testing"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// On-demand machines carry zero interruption probability; SPOT machines never do.
func TestInterruption_SpotNeverZero(t *testing.T) {
	in := newInterruption()

	if p := in.probability("m1", "VM.Standard.E5.Flex", providerkit.CapacityOnDemand); p != 0 {
		t.Errorf("on-demand probability = %v, want 0", p)
	}

	// A shape with a configured prior uses it; an unknown shape uses the default.
	if p := in.probability("m2", "VM.Standard.E5.Flex", providerkit.CapacitySpot); !(p > 0) {
		t.Errorf("spot probability (known shape) = %v, want > 0", p)
	}
	if p := in.probability("m3", "VM.Mystery.Flex", providerkit.CapacitySpot); p != defaultSpotPrior {
		t.Errorf("spot probability (unknown shape) = %v, want default %v", p, defaultSpotPrior)
	}
}

// An observed preemption raises the published probability above the forecast,
// and clear drops it back to the forecast.
func TestInterruption_ObservedEscalation(t *testing.T) {
	in := newInterruption()
	id, shape := "m1", "VM.Standard.E5.Flex"

	base := in.probability(id, shape, providerkit.CapacitySpot)
	in.markPreemption(id, 0.9)
	if p := in.probability(id, shape, providerkit.CapacitySpot); p <= base {
		t.Errorf("observed probability %v should exceed forecast %v", p, base)
	}

	// markPreemption clamps into [0,1].
	in.markPreemption(id, 5)
	if p := in.probability(id, shape, providerkit.CapacitySpot); p != 1.0 {
		t.Errorf("clamped probability = %v, want 1.0", p)
	}

	in.clear(id)
	if p := in.probability(id, shape, providerkit.CapacitySpot); p != base {
		t.Errorf("after clear probability = %v, want forecast %v", p, base)
	}
}
