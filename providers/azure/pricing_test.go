package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// erroringPriceClient is an azureClient whose spot-price lookup always fails, to
// drive pricing.refresh down its failure-logging path.
type erroringPriceClient struct {
	*azureFake
}

func (erroringPriceClient) SpotPriceUSD(context.Context, string) (float64, error) {
	return 0, fmt.Errorf("boom")
}

// refresh must not panic when the optional logger is nil (newPricing normalises
// it): the failure path logs a warning, which would nil-deref a nil logger.
func TestPricing_RefreshNilLoggerNoPanic(t *testing.T) {
	p := newPricing("eastus", erroringPriceClient{newAzureFake()}, nil)
	got := p.refresh(context.Background(), []string{"Standard_D4s_v5", "Standard_F8s_v2"})
	if got != 2 {
		t.Fatalf("refresh failures = %d, want 2", got)
	}
}

// An unpinned VM size must never price at 0 (which reads as "free" to cost
// ranking) — it falls back to the high sentinel for both on-demand and the spot
// cold-cache path.
func TestPricing_UnknownSizeNotFree(t *testing.T) {
	p := newPricing("eastus", newAzureFake(), quietLogger())
	if got := p.price("Standard_UNPINNED_v9", providerkit.CapacityOnDemand); got != unknownSizePriceUSD {
		t.Errorf("on-demand unpinned price = %v, want %v", got, unknownSizePriceUSD)
	}
	if got := p.price("Standard_UNPINNED_v9", providerkit.CapacitySpot); got <= 0 {
		t.Errorf("spot unpinned cold price = %v, want > 0", got)
	}
}

// newPricing with a nil logger and an unknown region must not panic on the
// region-fallback warning either.
func TestPricing_NewNilLoggerUnknownRegion(t *testing.T) {
	p := newPricing("nonexistent-region", newAzureFake(), nil)
	if p == nil {
		t.Fatal("newPricing returned nil")
	}
}
