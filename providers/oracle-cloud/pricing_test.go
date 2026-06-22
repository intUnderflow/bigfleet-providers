package main

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// approxEqual reports whether two USD prices match within a cent-fraction, so
// float rounding in the per-OCPU/per-GB arithmetic doesn't make assertions flaky.
func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// newSeedPricing builds a pricing seeded from the embedded prices.yaml with a
// deterministic, network-free live source (not yet refreshed).
func newSeedPricing(t *testing.T) *pricing {
	t.Helper()
	pr, err := newPricing("", newFakePriceSource(), quietLogger())
	if err != nil {
		t.Fatalf("newPricing: %v", err)
	}
	return pr
}

// The embedded price table must seed and price the default offerings sensibly:
// flexible shapes priced per-OCPU+per-GB, spot discounted below on-demand, bare
// metal at 0 — all before any live refresh.
func TestPricing_EmbeddedSeed(t *testing.T) {
	pr := newSeedPricing(t)

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
	pr := newSeedPricing(t)
	if p := pr.price("VM.Future.Flex", 4, 32, providerkit.CapacityOnDemand); p <= 0 {
		t.Errorf("unknown flex shape price = %v, want > 0 (default flex rate)", p)
	}
}

// refresh pulls a live rate into the table and price() reflects it: the real
// refresher path (priceSource -> mutex-guarded table -> price()) updates a price.
func TestPricing_RefreshUpdatesPrice(t *testing.T) {
	src := newFakePriceSource()
	// A live rate distinct from the seed, so the assertion is unambiguous.
	src.prices.flexRates["VM.Standard.E5.Flex"] = rate{OCPU: 0.099, MemoryGB: 0.009}
	pr, err := newPricing("", src, quietLogger())
	if err != nil {
		t.Fatalf("newPricing: %v", err)
	}
	before := pr.price("VM.Standard.E5.Flex", 2, 16, providerkit.CapacityOnDemand)
	if !pr.lastRefresh().IsZero() {
		t.Fatal("lastRefresh should be zero before any refresh")
	}
	if err := pr.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	want := 2*0.099 + 16*0.009
	if got := pr.price("VM.Standard.E5.Flex", 2, 16, providerkit.CapacityOnDemand); !approxEqual(got, want) {
		t.Errorf("post-refresh price = %v, want %v (before refresh: %v)", got, want, before)
	}
	if pr.lastRefresh().IsZero() {
		t.Error("lastRefresh should be set after a successful refresh")
	}
	if pr.stalenessSeconds() < 0 {
		t.Error("stalenessSeconds should be >= 0 after a successful refresh")
	}
}

// A live rate of 0 (e.g. the always-free Ampere A1 SKUs) must NOT overwrite the
// seed: the shape keeps its non-zero seed price rather than being ranked free.
func TestPricing_RefreshKeepsSeedOnZeroLiveRate(t *testing.T) {
	src := newFakePriceSource()
	src.prices.flexRates["VM.Standard.A1.Flex"] = rate{OCPU: 0, MemoryGB: 0}
	pr, err := newPricing("", src, quietLogger())
	if err != nil {
		t.Fatalf("newPricing: %v", err)
	}
	seed := pr.price("VM.Standard.A1.Flex", 2, 12, providerkit.CapacityOnDemand)
	if seed <= 0 {
		t.Fatalf("A1 seed price = %v, want > 0", seed)
	}
	if err := pr.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := pr.price("VM.Standard.A1.Flex", 2, 12, providerkit.CapacityOnDemand); got != seed {
		t.Errorf("zero live rate overwrote seed: got %v, want seed %v", got, seed)
	}
}

// The production source parses the OCI price-list JSON and maps SKUs onto shapes;
// driven against an httptest server it must update a flex price end-to-end.
func TestOCIPriceList_RefreshFromHTTP(t *testing.T) {
	// Minimal price-list document: the E5 OCPU + memory SKUs at known values.
	const body = `{"items":[
		{"partNumber":"B97384","metricName":"OCPU Per Hour","currencyCodeLocalizations":[{"currencyCode":"USD","prices":[{"model":"PAY_AS_YOU_GO","value":0.05}]}]},
		{"partNumber":"B97385","metricName":"Gigabytes Per Hour","currencyCodeLocalizations":[{"currencyCode":"USD","prices":[{"model":"PAY_AS_YOU_GO","value":0.004}]}]}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	source := newOCIPriceList(srv.URL, quietLogger())
	pr, err := newPricing("", source, quietLogger())
	if err != nil {
		t.Fatalf("newPricing: %v", err)
	}
	if err := pr.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	want := 2*0.05 + 16*0.004
	if got := pr.price("VM.Standard.E5.Flex", 2, 16, providerkit.CapacityOnDemand); !approxEqual(got, want) {
		t.Errorf("live HTTP-refreshed E5 price = %v, want %v", got, want)
	}
}

// A fetch error leaves the seed/last-live prices in place and surfaces the error.
func TestPricing_RefreshErrorKeepsSeed(t *testing.T) {
	src := newFakePriceSource()
	src.err = context.DeadlineExceeded
	pr, err := newPricing("", src, quietLogger())
	if err != nil {
		t.Fatalf("newPricing: %v", err)
	}
	seed := pr.price("VM.Standard.E5.Flex", 2, 16, providerkit.CapacityOnDemand)
	if err := pr.refresh(context.Background()); err == nil {
		t.Fatal("expected refresh to return the source error")
	}
	if got := pr.price("VM.Standard.E5.Flex", 2, 16, providerkit.CapacityOnDemand); got != seed {
		t.Errorf("failed refresh changed price: got %v, want seed %v", got, seed)
	}
	if !pr.lastRefresh().IsZero() {
		t.Error("lastRefresh should stay zero after a failed refresh")
	}
}

// validatePricing fails closed: an offered, hourly-billed shape that prices at 0
// is rejected, while a genuine bare_metal lane (honest 0) is allowed.
func TestValidatePricing_FailsClosedOnUnpriced(t *testing.T) {
	pr := newSeedPricing(t)
	logger := quietLogger()

	// An unknown FIXED shape has no seed and no live SKU -> priced 0 -> rejected.
	unpriced := []offering{
		{Shape: "VM.Mystery.Fixed", AvailabilityDomain: "AD-1", Capacity: "on_demand", Count: 1},
	}
	b, err := newOCIBackend("oci-test", newOCIFake(), unpriced, pr, newInterruption(), nil, logger)
	if err != nil {
		t.Fatalf("newOCIBackend: %v", err)
	}
	if err := b.validatePricing(); err == nil {
		t.Fatal("expected validatePricing to reject an unpriced on-demand offering")
	}

	// The same unpriced shape as a bare_metal lane is allowed (0 is honest).
	bmLane := []offering{
		{Shape: "VM.Mystery.Fixed", AvailabilityDomain: "AD-1", Capacity: "bare_metal", Count: 1},
	}
	bm, err := newOCIBackend("oci-test", newOCIFake(), bmLane, pr, newInterruption(), nil, logger)
	if err != nil {
		t.Fatalf("newOCIBackend: %v", err)
	}
	if err := bm.validatePricing(); err != nil {
		t.Errorf("bare_metal lane should pass validatePricing (0 is honest), got %v", err)
	}

	// The default offerings (all priced from the seed) pass.
	def, _ := newTestBackend(t, 8)
	if err := def.validatePricing(); err != nil {
		t.Errorf("default offerings should pass validatePricing, got %v", err)
	}
}
