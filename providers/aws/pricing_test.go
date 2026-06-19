package main

import (
	"testing"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

func TestNewPricing_RegionTableSelection(t *testing.T) {
	const onDemandM6iLarge = 0.096

	cases := []struct {
		name   string
		region string
	}{
		{"baseline region", "us-east-1"},
		{"tabulated peer region", "us-west-2"},
		{"untabulated region falls back to baseline", "eu-west-1"},
		{"empty region falls back quietly", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPricing(tc.region, newEC2Fake(), quietLogger())
			if got := p.price("m6i.large", "zone-a", providerkit.CapacityOnDemand); got != onDemandM6iLarge {
				t.Fatalf("on-demand m6i.large in %q = %v, want %v (baseline)", tc.region, got, onDemandM6iLarge)
			}
		})
	}
}

func TestNewPricing_SpotColdCacheFraction(t *testing.T) {
	p := newPricing("us-east-1", newEC2Fake(), quietLogger())
	// Cold spot cache returns a conservative fraction of on-demand so spot still
	// ranks below on-demand without ever reading 0.
	want := 0.3 * 0.096
	if got := p.price("m6i.large", "us-east-1a", providerkit.CapacitySpot); got != want {
		t.Fatalf("cold spot m6i.large = %v, want %v", got, want)
	}
}

func TestNewPricing_BareMetalIsFree(t *testing.T) {
	p := newPricing("us-east-1", newEC2Fake(), quietLogger())
	if got := p.price("m6i.large", "us-east-1a", providerkit.CapacityBareMetal); got != 0 {
		t.Fatalf("bare-metal price = %v, want 0 (already paid for)", got)
	}
}
