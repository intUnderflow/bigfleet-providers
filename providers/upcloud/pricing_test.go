package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

func TestPricing_PinnedTableConvertsToUSD(t *testing.T) {
	p := newPricing(1.10, newUpcloudFake(), quietLogger())
	off := offering{Plan: "2xCPU-4GB", Zone: "fi-hel1"}
	want := onDemandEURHourly["2xCPU-4GB"] * 1.10
	if got := p.price(off, providerkit.CapacityOnDemand); got != want {
		t.Errorf("price(2xCPU-4GB) = %v, want %v", got, want)
	}
}

func TestPricing_OperatorOverrideWins(t *testing.T) {
	p := newPricing(defaultEURtoUSD, newUpcloudFake(), quietLogger())
	off := offering{Plan: "2xCPU-4GB", Zone: "fi-hel1", PriceUSDPerHour: 0.123}
	if got := p.price(off, providerkit.CapacityOnDemand); got != 0.123 {
		t.Errorf("override price = %v, want 0.123", got)
	}
}

func TestPricing_UnknownPlanIsZero(t *testing.T) {
	p := newPricing(defaultEURtoUSD, newUpcloudFake(), quietLogger())
	off := offering{Plan: "no-such-plan", Zone: "fi-hel1"}
	if got := p.price(off, providerkit.CapacityOnDemand); got != 0 {
		t.Errorf("unknown plan price = %v, want 0", got)
	}
}

// The real path: the refresher pulls a live price (from the fake /price source)
// and overlays it on the pinned table, so price() reports the LIVE value — the
// source of truth — rather than the frozen snapshot.
func TestPricing_RefreshOverlaysLivePrice(t *testing.T) {
	fake := newUpcloudFake()
	fake.priceFactor = 2.0 // live EUR = pinned EUR * 2, clearly distinct from the table
	p := newPricing(1.0, fake, quietLogger())
	off := offering{Plan: "2xCPU-4GB", Zone: "fi-hel1"}

	pinned := onDemandEURHourly["2xCPU-4GB"] * 1.0 // pre-refresh: the seeded fallback
	if got := p.price(off, providerkit.CapacityOnDemand); got != pinned {
		t.Fatalf("pre-refresh price = %v, want pinned fallback %v", got, pinned)
	}

	if unpriced := p.refresh(context.Background(), []string{"2xCPU-4GB"}); unpriced != 0 {
		t.Fatalf("refresh reported %d unpriced plans, want 0", unpriced)
	}

	wantLive := onDemandEURHourly["2xCPU-4GB"] * 2.0 * 1.0
	got := p.price(off, providerkit.CapacityOnDemand)
	if got != wantLive {
		t.Errorf("post-refresh price = %v, want live %v", got, wantLive)
	}
	if got == pinned {
		t.Errorf("price did not move off the pinned fallback after a live refresh (%v)", got)
	}
}

// A live API failure leaves the pinned fallback in place (best-effort refresh) and
// reports every requested plan as a failure for the staleness metric.
func TestPricing_RefreshFailureKeepsFallback(t *testing.T) {
	p := newPricing(1.0, errClient{}, quietLogger())
	off := offering{Plan: "2xCPU-4GB", Zone: "fi-hel1"}
	if failed := p.refresh(context.Background(), []string{"2xCPU-4GB"}); failed != 1 {
		t.Fatalf("refresh failures = %d, want 1 (API error)", failed)
	}
	want := onDemandEURHourly["2xCPU-4GB"] * 1.0
	if got := p.price(off, providerkit.CapacityOnDemand); got != want {
		t.Errorf("price after failed refresh = %v, want pinned fallback %v", got, want)
	}
}

// Fail closed: an offered plan that has no live price AND no pinned fallback would
// emit price_per_hour=0, so the backend must refuse to start and name it.
func TestRequirePrices_FailsClosedOnUnpricedPlan(t *testing.T) {
	fake := newUpcloudFake()
	logger := quietLogger()
	offs := []offering{{Plan: "exotic-unpriced-plan", Zone: "fi-hel1", Capacity: "on_demand", Count: 1}}
	b, err := newUpcloudBackend("upcloud-test", "tpl", fake, offs, newPricing(defaultEURtoUSD, fake, logger), nil, logger)
	if err != nil {
		t.Fatalf("newUpcloudBackend: %v", err)
	}
	// A refresh cannot price the exotic plan (the fake omits it), so it stays unpriced.
	b.refreshPrices(context.Background())
	err = b.requirePrices()
	if err == nil {
		t.Fatal("requirePrices accepted an offering with no resolvable price; want fail-closed error")
	}
	if !strings.Contains(err.Error(), "exotic-unpriced-plan") {
		t.Errorf("error should name the unpriced plan, got: %v", err)
	}
}

// A priced offering (live or pinned fallback or override) passes the fail-closed gate.
func TestRequirePrices_PassesWhenPriced(t *testing.T) {
	fake := newUpcloudFake()
	logger := quietLogger()
	offs := defaultOfferings(8, "fi-hel1", "de-fra1") // all plans are in the pinned table
	b, err := newUpcloudBackend("upcloud-test", "tpl", fake, offs, newPricing(defaultEURtoUSD, fake, logger), nil, logger)
	if err != nil {
		t.Fatalf("newUpcloudBackend: %v", err)
	}
	b.refreshPrices(context.Background())
	if err := b.requirePrices(); err != nil {
		t.Errorf("requirePrices rejected fully-priced offerings: %v", err)
	}
}

// The warn dedup map and the live map are touched concurrently from the gRPC
// serving goroutines, the background reconciler, and the price refresher; run them
// under -race to prove the guard holds.
func TestPricing_ConcurrentPriceAndRefresh(t *testing.T) {
	fake := newUpcloudFake()
	p := newPricing(defaultEURtoUSD, fake, quietLogger())
	unknown := offering{Plan: "no-such-plan", Zone: "fi-hel1"}
	known := offering{Plan: "2xCPU-4GB", Zone: "fi-hel1"}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = p.price(unknown, providerkit.CapacityOnDemand)
				_ = p.price(known, providerkit.CapacityOnDemand)
				_ = p.refresh(context.Background(), []string{"2xCPU-4GB"})
			}
		}()
	}
	wg.Wait()
}

func TestPlanResolver_AllocatableDistinctFromMemoryUnits(t *testing.T) {
	r := newPlanResolver(newUpcloudFake(), quietLogger())
	alloc := r.allocatable("2xCPU-4GB")
	if alloc["cpu"] != "2" {
		t.Errorf("cpu = %q, want 2", alloc["cpu"])
	}
	if alloc["memory"] != "4Gi" {
		t.Errorf("memory = %q, want 4Gi", alloc["memory"])
	}
	if r.allocatable("totally-unknown-plan") != nil {
		t.Error("unknown plan should yield nil allocatable")
	}
}

// errClient is an upcloudClient whose pricing call always fails, to exercise the
// best-effort refresh fallback path. Only DescribePlanPrices is exercised; the
// embedded nil interface satisfies the rest of the surface.
type errClient struct{ upcloudClient }

func (errClient) DescribePlanPrices(context.Context, []string) (map[string]float64, error) {
	return nil, context.DeadlineExceeded
}
