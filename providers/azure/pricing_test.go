package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// erroringPriceClient is an azureClient whose price lookups always fail, to
// drive pricing.refresh down its failure-logging path.
type erroringPriceClient struct {
	*azureFake
}

func (erroringPriceClient) SpotPriceUSD(context.Context, string) (float64, error) {
	return 0, fmt.Errorf("boom")
}

func (erroringPriceClient) OnDemandPriceUSD(context.Context, string) (float64, error) {
	return 0, fmt.Errorf("boom")
}

// refresh must not panic when the optional logger is nil (newPricing normalises
// it): the failure path logs a warning, which would nil-deref a nil logger.
func TestPricing_RefreshNilLoggerNoPanic(t *testing.T) {
	p := newPricing("eastus", erroringPriceClient{newAzureFake()}, nil)
	// Two on-demand + two spot fetches, all failing.
	got := p.refresh(context.Background(),
		[]string{"Standard_D4s_v5", "Standard_F8s_v2"},
		[]string{"Standard_D4s_v5", "Standard_F8s_v2"})
	if got != 4 {
		t.Fatalf("refresh failures = %d, want 4", got)
	}
	if !p.lastRefreshSuccess().IsZero() {
		t.Errorf("lastRefreshSuccess set despite all fetches failing")
	}
}

// stubOnDemandClient reports a fixed on-demand price distinct from the pinned
// seed, to prove the timer refresh — not the frozen seed — drives the served
// on-demand price.
type stubOnDemandClient struct {
	*azureFake
	usd float64
}

func (s stubOnDemandClient) OnDemandPriceUSD(context.Context, string) (float64, error) {
	return s.usd, nil
}

// On-demand price must be live: the served price comes from the seed only until
// a refresh runs, after which it tracks the live Retail Prices value.
func TestPricing_OnDemandRefreshUpdatesPrice(t *testing.T) {
	const live = 0.27
	const size = "Standard_D4s_v5"
	seed := onDemandEastUS[size]
	if seed == 0 || seed == live {
		t.Fatalf("test precondition: seed %v must be nonzero and differ from live %v", seed, live)
	}
	p := newPricing("eastus", stubOnDemandClient{azureFake: newAzureFake(), usd: live}, quietLogger())

	// Cold cache: served from the pinned seed.
	if got := p.price(size, providerkit.CapacityOnDemand); got != seed {
		t.Fatalf("cold on-demand price = %v, want seed %v", got, seed)
	}
	// After a refresh: served from the live value, not the seed.
	if failed := p.refresh(context.Background(), []string{size}, nil); failed != 0 {
		t.Fatalf("refresh failures = %d, want 0", failed)
	}
	if got := p.price(size, providerkit.CapacityOnDemand); got != live {
		t.Errorf("refreshed on-demand price = %v, want live %v", got, live)
	}
	if p.lastRefreshSuccess().IsZero() {
		t.Error("lastRefreshSuccess not stamped after a clean refresh")
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
