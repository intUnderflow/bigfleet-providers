package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// pricing supplies Machine.price_per_hour (USD/hour). Latitude.sh is on-demand
// only — there is no spot market — so every price is the published hourly
// on-demand rate for the (plan, site). Latitude publishes prices in USD directly,
// so there is no FX conversion (unlike Hetzner's EUR).
//
// Prices come from a pinned plan table (deterministic, no runtime pricing
// dependency on the List hot path), refreshed out-of-band from the live Plans
// API on a timer rather than per-List. The table is not load-bearing for
// correctness (it feeds the engine's relative cost ranking), but keep it roughly
// accurate.
type pricing struct {
	client latitudeClient
	logger *slog.Logger

	mu     sync.Mutex
	hourly map[string]float64 // "plan|site" -> last fetched USD/hr
}

// onDemandUSDHourly is a pinned snapshot of Latitude.sh hourly on-demand prices
// in USD, keyed by plan slug. Latitude prices a plan close-to-identically across
// sites (a small per-site premium is folded in by the live refresh), so the
// table is the baseline. Regenerate by reading the public Plans catalogue.
var onDemandUSDHourly = map[string]float64{
	// Compute (c-series, x86 bare metal).
	"c2-small-x86":  0.30,
	"c2-medium-x86": 0.50,
	"c2-large-x86":  0.75,
	"c3-small-x86":  0.40,
	"c3-medium-x86": 0.65,
	"c3-large-x86":  1.10,
	"c3-xlarge-x86": 2.20,
	// Storage (s-series).
	"s2-small-x86": 0.45,
	"s3-large-x86": 1.30,
	// Memory (m-series).
	"m3-large-x86":   1.60,
	"m4-metal-large": 3.20,
	// GPU (g-series).
	"g3-large-x86":  3.50,
	"g3-xlarge-x86": 6.00,
	"g4-xlarge-x86": 9.50,
}

func newPricing(client latitudeClient, logger *slog.Logger) *pricing {
	return &pricing{
		client: client,
		logger: logger,
		hourly: make(map[string]float64),
	}
}

func priceKey(plan, site string) string { return plan + "|" + site }

// price returns USD/hour for a machine of the given shape. Reads only cached
// state (never blocks on the network), so it is safe on the List/seed path.
func (p *pricing) price(plan, site string, capacity providerkit.CapacityType) float64 {
	if capacity == providerkit.CapacityBareMetal {
		return 0 // owned hardware, already paid for (not how Latitude declares — ON_DEMAND)
	}
	p.mu.Lock()
	v, ok := p.hourly[priceKey(plan, site)]
	p.mu.Unlock()
	if ok {
		return v
	}
	// Cold cache: fall back to the pinned USD table.
	return onDemandUSDHourly[plan]
}

// refresh fetches the current hourly price for each (plan, site) pair and caches
// it (already USD). Best-effort: a fetch error leaves the prior (or fallback)
// value. Call it once at startup and on a timer; never on the List hot path.
// Returns the number of pairs that failed to refresh.
func (p *pricing) refresh(ctx context.Context, pairs []pricePair) int {
	failures := 0
	for _, pr := range pairs {
		v, err := p.client.PriceUSD(ctx, pr.plan, pr.site)
		if err != nil {
			failures++
			if p.logger != nil {
				p.logger.Warn("pricing: price fetch failed; keeping fallback",
					"plan", pr.plan, "site", pr.site, "err", err)
			}
			continue
		}
		p.mu.Lock()
		p.hourly[priceKey(pr.plan, pr.site)] = v
		p.mu.Unlock()
	}
	return failures
}

// pricePair identifies one (plan, site) to refresh pricing for.
type pricePair struct {
	plan string
	site string
}
