package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// pricing supplies Machine.price_per_hour (USD/hour). On-demand GCE prices are
// refreshed live, off the List hot path: a background loop periodically pulls
// the current on-demand rates from a pricingSource (the Cloud Billing Catalog
// API in production) into a mutex-guarded map, and List/Describe read that map
// without ever calling pricing. The pinned, region-keyed table is the source's
// startup SEED and its FALLBACK only — it seeds price_per_hour before the first
// refresh and backstops a refresh failure, so a billing-API outage never zeroes
// a price; the live refresh is the source of truth once it lands.
//
// Spot (preemptible) prices are dynamic and deeply discounted; we model them as
// a fixed fraction of the (live or fallback) on-demand rate (spotFraction),
// which keeps the spot price non-zero and roughly accurate without a separate
// lookup. The cost field is a relative ranking signal — effective_cost = price +
// interruption_probability × penalty — so an approximate spot price is
// acceptable, but it must never be 0.
type pricing struct {
	region string
	// seed is the pinned, region-resolved on-demand table (USD/hr). It seeds
	// price_per_hour before the first live refresh and is the fallback when the
	// live source has no value for a type. Read-only after construction.
	seed   map[string]float64
	src    pricingSource
	logger *slog.Logger

	mu          sync.Mutex
	live        map[string]float64 // machineType -> last live on-demand USD/hr
	lastRefresh time.Time          // last fully-successful refresh (zero until first success)
}

// spotFraction is the share of the on-demand price a Spot VM is billed at. GCE
// Spot discounts run ~60–91%; 0.4 is a conservative (high) estimate so the cost
// engine never under-prices spot.
const spotFraction = 0.4

// onDemandBaselineRegion supplies on-demand prices when a region has no pinned
// table of its own (the prices are then approximate — pin a per-region table).
const onDemandBaselineRegion = "us-central1"

// onDemandByRegion holds pinned on-demand prices (USD/hr) per region — the live
// refresh's startup seed and fallback. us-central1 is the authoritative
// baseline; add a region by pinning its table and dropping it in here.
var onDemandByRegion = map[string]map[string]float64{
	"us-central1": onDemandUSCentral1,
	// us-east1 matches us-central1 for these standard families.
	"us-east1": onDemandUSCentral1,
}

// onDemandUSCentral1 is a pinned snapshot of us-central1 on-demand prices (USD/hr)
// for the pinned machine types — the seed/fallback for the live refresh, not the
// runtime source of truth. Refreshed live from the Cloud Billing Catalog; this
// snapshot only has to be roughly right for the cold-start window.
var onDemandUSCentral1 = map[string]float64{
	"e2-standard-2": 0.067, "e2-standard-4": 0.134, "e2-standard-8": 0.268, "e2-standard-16": 0.536,
	"n2-standard-2": 0.097, "n2-standard-4": 0.194, "n2-standard-8": 0.388, "n2-standard-16": 0.777, "n2-standard-32": 1.554,
	"n2-highmem-2": 0.131, "n2-highmem-4": 0.262, "n2-highmem-8": 0.524,
	"n2d-standard-4": 0.169, "n2d-standard-8": 0.338, "n2d-standard-16": 0.676,
	"c2-standard-4": 0.209, "c2-standard-8": 0.418, "c2-standard-16": 0.836, "c2-standard-30": 1.567,
	"c3-standard-4": 0.207, "c3-standard-8": 0.414, "c3-standard-22": 1.139, "c3-highmem-22": 1.535,
	"m1-megamem-96": 10.674,
	"a2-highgpu-1g": 3.673, "g2-standard-4": 0.708,
}

// pricingSource is the live on-demand price feed the refresher pulls from. The
// production implementation (gceBillingPricer) reads the Cloud Billing Catalog
// API; the credential-free staticPricer backs the fake backend and tests. It is
// only ever called off the List hot path (by pricing.refresh).
type pricingSource interface {
	// OnDemandPriceUSD returns the current on-demand USD/hour for machineType in
	// region. A non-nil error (or a non-positive price) leaves the cached/seed
	// value in place.
	OnDemandPriceUSD(ctx context.Context, machineType, region string) (float64, error)
}

func newPricing(region string, src pricingSource, logger *slog.Logger) *pricing {
	table, ok := onDemandByRegion[region]
	if !ok {
		// No pinned table for this region; fall back to the baseline.
		table = onDemandByRegion[onDemandBaselineRegion]
	}
	return &pricing{
		region: region,
		seed:   table,
		src:    src,
		logger: logger,
		live:   make(map[string]float64),
	}
}

// hasPrice reports whether the SEED table has a positive on-demand price for the
// machine type. newGCPBackend uses it to fail closed at startup: an offering
// whose type has no seed price has no safe value to publish before the first
// live refresh, so it is rejected rather than allowed to emit price_per_hour = 0.
func (p *pricing) hasPrice(machineType string) bool {
	return p.seed[machineType] > 0
}

// price returns USD/hour for a machine of the given shape. Pure cached lookup —
// never blocks on the network, so it is safe on the List/seed hot path.
func (p *pricing) price(machineType string, capacity providerkit.CapacityType) float64 {
	switch capacity {
	case providerkit.CapacityBareMetal:
		return 0 // GCE has no bare metal in this provider; defensive only
	case providerkit.CapacitySpot:
		return spotFraction * p.onDemand(machineType)
	default: // ON_DEMAND / RESERVED — reserved is a billing construct; price it
		// at on-demand unless a real committed-use discount is modelled.
		return p.onDemand(machineType)
	}
}

// onDemand returns the live on-demand rate when the refresher has one, else the
// pinned seed/fallback. The startup hasPrice gate guarantees the seed is > 0 for
// every offered type, so this never returns 0 for a real VM.
func (p *pricing) onDemand(machineType string) float64 {
	p.mu.Lock()
	v, ok := p.live[machineType]
	p.mu.Unlock()
	if ok && v > 0 {
		return v
	}
	return p.seed[machineType]
}

// refresh pulls the current on-demand price for each machine type from the live
// source into the cache. Best-effort: a type whose fetch fails keeps its prior
// (or seed) value. Call it at startup and on a timer; never on the List hot path.
// Returns the number of types that failed to refresh.
func (p *pricing) refresh(ctx context.Context, types []string) int {
	if p.src == nil {
		return 0
	}
	failed := 0
	updates := make(map[string]float64, len(types))
	for _, t := range types {
		v, err := p.src.OnDemandPriceUSD(ctx, t, p.region)
		if err != nil || v <= 0 {
			failed++
			if p.logger != nil {
				p.logger.Warn("pricing: live price fetch failed; keeping fallback",
					"machine_type", t, "region", p.region, "price", v, "err", err)
			}
			continue
		}
		updates[t] = v
	}
	p.mu.Lock()
	for t, v := range updates {
		p.live[t] = v
	}
	if failed == 0 && len(types) > 0 {
		p.lastRefresh = time.Now()
	}
	p.mu.Unlock()
	return failed
}

// staleness reports the age of the last fully-successful refresh and whether one
// has ever happened. Used to surface price-cache freshness (log/metric).
func (p *pricing) staleness() (time.Duration, bool) {
	p.mu.Lock()
	last := p.lastRefresh
	p.mu.Unlock()
	if last.IsZero() {
		return 0, false
	}
	return time.Since(last), true
}

// staticPricer is a deterministic, credential-free pricingSource. It backs the
// fake GCE backend and certification (no live billing calls) and the unit tests:
// it returns the pinned table price for the region, so prices stay deterministic
// and the live-refresh path is still exercised end-to-end.
type staticPricer struct {
	table map[string]float64
}

func newStaticPricer(region string) *staticPricer {
	t, ok := onDemandByRegion[region]
	if !ok {
		t = onDemandByRegion[onDemandBaselineRegion]
	}
	return &staticPricer{table: t}
}

func (s *staticPricer) OnDemandPriceUSD(_ context.Context, machineType, _ string) (float64, error) {
	v, ok := s.table[machineType]
	if !ok || v <= 0 {
		return 0, fmt.Errorf("static pricer: no price for machine type %q", machineType)
	}
	return v, nil
}

var _ pricingSource = (*staticPricer)(nil)
