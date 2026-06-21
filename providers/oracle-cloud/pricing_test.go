package main

import (
	"testing"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// The embedded price table must load and price the default offerings sensibly:
// flexible shapes priced per-OCPU+per-GB, spot discounted below on-demand, bare
// metal at 0.
func TestPricing_EmbeddedTable(t *testing.T) {
	pr, err := newPricing("")
	if err != nil {
		t.Fatalf("newPricing: %v", err)
	}

	onDemand := pr.price("VM.Standard.E5.Flex", 2, 16, providerkit.CapacityOnDemand)
	if onDemand <= 0 {
		t.Fatalf("on-demand flex price = %v, want > 0", onDemand)
	}
	spot := pr.price("VM.Standard.E5.Flex", 2, 16, providerkit.CapacitySpot)
	if !(spot > 0 && spot < onDemand) {
		t.Errorf("spot price %v should be > 0 and < on-demand %v", spot, onDemand)
	}
	if bm := pr.price("BM.Standard.E5.192", 0, 0, providerkit.CapacityBareMetal); bm != 0 {
		t.Errorf("bare-metal (held) price = %v, want 0", bm)
	}
	// A BM.* shape offered as on-demand must be priced (non-zero), not ranked free.
	if bmOD := pr.price("BM.Standard.E5.192", 0, 0, providerkit.CapacityOnDemand); bmOD <= 0 {
		t.Errorf("on-demand bare-metal price = %v, want > 0 (fixed_hourly entry)", bmOD)
	}
	if gpu := pr.price("VM.GPU.A10.1", 0, 0, providerkit.CapacityOnDemand); gpu <= 0 {
		t.Errorf("fixed GPU shape price = %v, want > 0", gpu)
	}
}

// An unknown flexible shape falls back to the default flex rate (non-zero), so a
// newly offered shape is still ranked rather than appearing free.
func TestPricing_UnknownFlexFallsBack(t *testing.T) {
	pr, err := newPricing("")
	if err != nil {
		t.Fatalf("newPricing: %v", err)
	}
	if p := pr.price("VM.Future.Flex", 4, 32, providerkit.CapacityOnDemand); p <= 0 {
		t.Errorf("unknown flex shape price = %v, want > 0 (default flex rate)", p)
	}
}
