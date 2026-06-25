package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// pricing supplies Machine.price_per_hour (USD/hour). Scaleway has no spot
// market, so every Instances price is the published hourly on-demand rate for
// the (commercial type, zone). Scaleway publishes prices in EUR; we convert to
// USD with a configurable FX rate (eurToUSD). Elastic Metal capacity is owned
// hardware, so its price is exactly 0.
//
// Prices come from a pinned, zone-agnostic EUR table (deterministic, no runtime
// pricing dependency on the List hot path), optionally refreshed out-of-band
// from the Scaleway product catalogue on a timer rather than per-List. The table
// feeds the engine's relative cost ranking; the live refresh is authoritative, so
// it is never hand-maintained.
type pricing struct {
	eurToUSD float64
	client   scwClient
	logger   *slog.Logger

	mu         sync.Mutex
	hourly     map[string]float64 // "commercialType|zone" -> last fetched USD/hr
	warnedMiss map[string]bool    // commercial types we've already warned about (dedupe)
}

// defaultEURtoUSD is a reasonable fallback FX rate; operators should pin a
// current rate via --eur-usd. The cost field is a relative ranking signal, so an
// approximate rate is acceptable, but a stale one skews effective-cost.
const defaultEURtoUSD = 1.08

// onDemandEURHourly is a pinned snapshot of Scaleway Instances hourly on-demand
// prices in EUR (server-only, excluding the flexible-IP surcharge), keyed by
// commercial type. Scaleway prices a given type identically across its EU zones;
// the live refresh overlays any zone-specific deviation. Regenerate from the
// Scaleway product catalogue (no credentials needed for the public catalogue).
var onDemandEURHourly = map[string]float64{
	// DEV1 (development).
	"DEV1-S": 0.0090, "DEV1-M": 0.0180, "DEV1-L": 0.0360, "DEV1-XL": 0.0540,
	// GP1 (general purpose, AMD EPYC).
	"GP1-XS": 0.0510, "GP1-S": 0.1020, "GP1-M": 0.2040, "GP1-L": 0.4080, "GP1-XL": 0.8160,
	// PLAY2 / PRO2 (current shared/dedicated lines).
	"PLAY2-PICO": 0.0061, "PLAY2-NANO": 0.0122, "PLAY2-MICRO": 0.0244,
	"PRO2-XXS": 0.0258, "PRO2-XS": 0.0516, "PRO2-S": 0.1032, "PRO2-M": 0.2064, "PRO2-L": 0.4128,
	// COPARM1 (Ampere Arm64).
	"COPARM1-2C-8G": 0.0280, "COPARM1-4C-16G": 0.0560, "COPARM1-8C-32G": 0.1120, "COPARM1-16C-64G": 0.2240,
	// RENDER / GPU.
	"RENDER-S": 1.2400, "H100-1-80G": 2.7300, "L4-1-24G": 0.7500,
}

func newPricing(eurToUSD float64, client scwClient, logger *slog.Logger) *pricing {
	if eurToUSD <= 0 {
		eurToUSD = defaultEURtoUSD
	}
	return &pricing{
		eurToUSD: eurToUSD,
		client:   client,
		logger:   logger,
		hourly:   make(map[string]float64),
	}
}

func priceKey(commercialType, zone string) string { return commercialType + "|" + zone }

// price returns USD/hour for a machine of the given shape. Reads only cached
// state (never blocks on the network), so it is safe on the List/seed path.
func (p *pricing) price(commercialType, zone string, capacity providerkit.CapacityType) float64 {
	if capacity == providerkit.CapacityBareMetal {
		return 0 // owned hardware, already paid for
	}
	p.mu.Lock()
	v, ok := p.hourly[priceKey(commercialType, zone)]
	p.mu.Unlock()
	if ok {
		return v
	}
	// Cold cache: fall back to the pinned EUR table converted to USD.
	eur, pinned := onDemandEURHourly[commercialType]
	if !pinned {
		// No live and no pinned price — returning 0 would make a paid VM look free
		// (e.g. an orphan of an unpinned type discovered via Describe; offered types
		// are rejected at construction). Warn once so it's visible, not silent.
		p.warnMissing(commercialType)
	}
	return eur * p.eurToUSD
}

// warnMissing logs a missing-pinned-price warning once per commercial type.
func (p *pricing) warnMissing(commercialType string) {
	if p.logger == nil {
		return
	}
	p.mu.Lock()
	if p.warnedMiss == nil {
		p.warnedMiss = make(map[string]bool)
	}
	first := !p.warnedMiss[commercialType]
	p.warnedMiss[commercialType] = true
	p.mu.Unlock()
	if first {
		p.logger.Warn("pricing: no pinned or live price for on-demand commercial type; reporting price_per_hour=0 (add it to onDemandEURHourly)",
			"commercial_type", commercialType)
	}
}

// refresh fetches the current hourly price for each (commercialType, zone) pair
// and caches it (already USD — the client converts EUR→USD). Best-effort: a
// fetch error leaves the prior (or fallback) value. Call it once at startup and
// on a timer; never on the List hot path. Returns the number of pairs that
// failed to refresh.
func (p *pricing) refresh(ctx context.Context, pairs []pricePair) int {
	failures := 0
	for _, pr := range pairs {
		v, err := p.client.PriceUSD(ctx, pr.commercialType, pr.zone)
		if err != nil {
			failures++
			if p.logger != nil {
				p.logger.Warn("pricing: price fetch failed; keeping fallback",
					"commercial_type", pr.commercialType, "zone", pr.zone, "err", err)
			}
			continue
		}
		p.mu.Lock()
		p.hourly[priceKey(pr.commercialType, pr.zone)] = v
		p.mu.Unlock()
	}
	return failures
}

// pricePair identifies one (commercialType, zone) to refresh pricing for.
type pricePair struct {
	commercialType string
	zone           string
}
