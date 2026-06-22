package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakePriceSource is a deterministic, network-free priceSource for tests — the
// live-refresh path under test must never make a real HTTP call.
type fakePriceSource struct {
	eur map[string]float64
	err error
}

func (f *fakePriceSource) HourlyEUR(_ context.Context, flavors []string) (map[string]float64, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]float64)
	for _, fl := range flavors {
		if v, ok := f.eur[fl]; ok {
			out[fl] = v
		}
	}
	return out, nil
}

// A successful live refresh overlays catalog prices (converted EUR->USD) onto the
// seed, and price() then returns the live value off the hot path.
func TestPricing_RefreshOverlaysLive(t *testing.T) {
	src := &fakePriceSource{eur: map[string]float64{"b2-7": 0.10}}
	p := newPricing(2.0, src, quietLogger()) // exaggerated rate for an exact assertion

	// Before refresh: cold cache falls back to the dated seed * rate.
	if got, want := p.price("b2-7"), onDemandEURHourly["b2-7"]*2.0; got != want {
		t.Fatalf("pre-refresh price = %v, want seed fallback %v", got, want)
	}
	if !p.lastRefresh().IsZero() {
		t.Fatal("lastRefresh should be zero before any successful refresh (source=manual)")
	}

	missing, err := p.refresh(context.Background(), []string{"b2-7"})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if missing != 0 {
		t.Fatalf("refresh reported %d missing, want 0", missing)
	}
	if got, want := p.price("b2-7"), 0.10*2.0; got != want {
		t.Errorf("post-refresh price = %v, want live %v", got, want)
	}
	if p.lastRefresh().IsZero() {
		t.Error("lastRefresh must be set after a successful refresh")
	}
}

// An operator override wins over both the live catalog price and the seed.
func TestPricing_OverrideWinsOverLive(t *testing.T) {
	src := &fakePriceSource{eur: map[string]float64{"b2-7": 0.10}}
	p := newPricing(1.0, src, quietLogger())
	p.setOverride("b2-7", 0.99)
	if _, err := p.refresh(context.Background(), []string{"b2-7"}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := p.price("b2-7"); got != 0.99 {
		t.Errorf("price = %v, want override 0.99 (override must win over live)", got)
	}
}

// A catalog fetch error must NOT clobber existing prices: the seed/fallback stays
// and the error is surfaced so the caller can mark the refresh failed
// (source=manual), never silently zeroing a price.
func TestPricing_RefreshErrorKeepsFallback(t *testing.T) {
	src := &fakePriceSource{err: errors.New("catalog down")}
	p := newPricing(1.0, src, quietLogger())
	missing, err := p.refresh(context.Background(), []string{"b2-7"})
	if err == nil {
		t.Fatal("expected refresh to surface the fetch error")
	}
	if missing != 0 {
		t.Errorf("missing = %d on fetch error, want 0", missing)
	}
	// Seed price still serves; never 0.
	if got, want := p.price("b2-7"), onDemandEURHourly["b2-7"]*1.0; got != want {
		t.Errorf("price after failed refresh = %v, want seed %v", got, want)
	}
	if !p.lastRefresh().IsZero() {
		t.Error("lastRefresh must stay zero when the only refresh failed")
	}
}

// With no live source (fake backend / dev), refresh is a no-op and prices stay on
// the deterministic seed — the offline, reproducible conformance path.
func TestPricing_NoSourceIsDeterministicSeed(t *testing.T) {
	p := newPricing(1.08, nil, quietLogger())
	missing, err := p.refresh(context.Background(), []string{"b2-7"})
	if err != nil || missing != 0 {
		t.Fatalf("no-source refresh = (%d, %v), want (0, nil)", missing, err)
	}
	if got, want := p.price("b2-7"), onDemandEURHourly["b2-7"]*1.08; got != want {
		t.Errorf("seed price = %v, want %v", got, want)
	}
}

// Fail closed on an unpriced flavor: known() is false for a flavor with neither a
// seed entry nor an override (regardless of the live catalog, which may be down
// at startup), so newOVHBackend rejects an offering for it rather than publishing
// price_per_hour=0 (which always wins the shard's cost ranking).
func TestPricing_KnownFailsClosed(t *testing.T) {
	p := newPricing(1.08, &fakePriceSource{eur: map[string]float64{"made-up-9": 0.5}}, quietLogger())
	if p.known("made-up-9") {
		t.Error("known() must not count live prices (catalog can be down at startup)")
	}
	if !p.known("b2-7") {
		t.Error("known() must be true for a seeded flavor")
	}
	p.setOverride("made-up-9", 0.5)
	if !p.known("made-up-9") {
		t.Error("known() must be true once an override is set")
	}

	// The construction guard rejects an offering whose flavor has no guaranteed
	// price (no seed entry, no override).
	offs := []offering{{Flavor: "totally-unpriced", Region: "GRA", Capacity: "on_demand", Count: 1, Resources: map[string]string{"cpu": "1", "memory": "2Gi"}}}
	if _, err := newOVHBackend("ovh-public-GRA", "GRA", "img", newOVHFake(), offs, newPricing(1.08, nil, quietLogger()), nil, quietLogger()); err == nil {
		t.Error("expected newOVHBackend to reject an offering with an unpriced flavor (fail closed)")
	}
}

// The catalog source parses the public order-catalog shape, picking the hourly
// "<flavor>.consumption" rate and converting catalog units to EUR — served from a
// local httptest server, so the test is offline and deterministic.
func TestCatalogPriceSource_ParsesHourlyConsumption(t *testing.T) {
	const body = `{
	  "locale": {"currencyCode": "EUR", "subsidiary": "FR"},
	  "addons": [
	    {"planCode": "b2-7.consumption", "pricings": [
	      {"intervalUnit": "month", "capacities": ["renew"], "price": 4666000000},
	      {"intervalUnit": "hour", "capacities": ["consumption"], "price": 7090000}
	    ]},
	    {"planCode": "b2-7.monthly.postpaid", "pricings": [
	      {"intervalUnit": "month", "capacities": ["consumption"], "price": 4666000000}
	    ]},
	    {"planCode": "win-b2-7.consumption", "pricings": [
	      {"intervalUnit": "hour", "capacities": ["consumption"], "price": 9999000}
	    ]},
	    {"planCode": "c2-7.consumption", "pricings": [
	      {"intervalUnit": "hour", "capacities": ["consumption"], "price": 10180000}
	    ]}
	  ]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("ovhSubsidiary"); got != "FR" {
			t.Errorf("ovhSubsidiary = %q, want FR", got)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	src := newCatalogPriceSource(srv.URL, "FR", quietLogger(), nil)
	got, err := src.HourlyEUR(context.Background(), []string{"b2-7", "c2-7", "absent-flavor"})
	if err != nil {
		t.Fatalf("HourlyEUR: %v", err)
	}
	// b2-7 must come from "b2-7.consumption" (0.0709), NOT "win-b2-7.consumption"
	// or the monthly entry.
	if got["b2-7"] != 0.0709 {
		t.Errorf("b2-7 = %v, want 0.0709 (the base hourly consumption rate)", got["b2-7"])
	}
	if got["c2-7"] != 0.1018 {
		t.Errorf("c2-7 = %v, want 0.1018", got["c2-7"])
	}
	if _, ok := got["absent-flavor"]; ok {
		t.Error("a flavor absent from the catalog must be omitted (seed fallback covers it)")
	}
}

// A non-positive catalog price must be ignored (not overlaid as a live 0, which
// would always win the cost ranking): the flavor stays absent so the seed covers it.
func TestCatalogPriceSource_SkipsNonPositivePrice(t *testing.T) {
	const body = `{
	  "locale": {"currencyCode": "EUR"},
	  "addons": [
	    {"planCode": "b2-7.consumption", "pricings": [
	      {"intervalUnit": "hour", "capacities": ["consumption"], "price": 0}
	    ]}
	  ]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	src := newCatalogPriceSource(srv.URL, "FR", quietLogger(), nil)
	got, err := src.HourlyEUR(context.Background(), []string{"b2-7"})
	if err != nil {
		t.Fatalf("HourlyEUR: %v", err)
	}
	if _, ok := got["b2-7"]; ok {
		t.Error("a zero-priced catalog entry must be omitted, not overlaid as a live 0")
	}
}

// A non-EUR subsidiary must fail the fetch rather than convert a wrong-currency
// number with --eur-usd.
func TestCatalogPriceSource_RejectsNonEUR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"locale": {"currencyCode": "GBP"}, "addons": []}`))
	}))
	defer srv.Close()
	src := newCatalogPriceSource(srv.URL, "GB", quietLogger(), nil)
	if _, err := src.HourlyEUR(context.Background(), []string{"b2-7"}); err == nil {
		t.Error("expected a non-EUR catalog to be rejected (--eur-usd assumes EUR)")
	}
}

func TestConsumptionFlavor(t *testing.T) {
	cases := map[string]struct {
		flavor string
		ok     bool
	}{
		"b2-7.consumption":               {"b2-7", true},
		"c2-15.consumption":              {"c2-15", true},
		"win-b2-7.consumption":           {"win-b2-7", true}, // a real flavor token; the caller only asks about bare flavors
		"b2-7.monthly.postpaid":          {"", false},
		"b2-7.option.dc-adp.consumption": {"", false}, // contains a dot before .consumption
		"consumption":                    {"", false},
		"":                               {"", false},
	}
	for code, want := range cases {
		gotF, gotOK := consumptionFlavor(code)
		if gotF != want.flavor || gotOK != want.ok {
			t.Errorf("consumptionFlavor(%q) = (%q, %v), want (%q, %v)", code, gotF, gotOK, want.flavor, want.ok)
		}
	}
}
