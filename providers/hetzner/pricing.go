package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// pricing supplies Machine.price_per_hour (USD/hour). Hetzner Cloud is
// on-demand only — there is no spot market — so every price is the published
// hourly on-demand rate for the (server type, location). Hetzner publishes
// prices in EUR; we convert to USD with a configurable rate (eurToUSD).
//
// Prices come from a pinned, location-agnostic EUR table (deterministic, no
// runtime pricing dependency on the List hot path), refreshed out-of-band from
// the live ServerType.Pricings on a timer rather than per-List. The table is
// not load-bearing for correctness (it feeds the engine's relative cost
// ranking); the live refresh is authoritative, so it is never hand-maintained.
type pricing struct {
	eurToUSD float64
	client   hcloudClient
	logger   *slog.Logger

	mu     sync.Mutex
	hourly map[string]float64 // "serverType|location" -> last fetched USD/hr
}

// defaultEURtoUSD is a reasonable fallback FX rate; operators should pin a
// current rate via --eur-usd. The cost field is a relative ranking signal, so
// an approximate rate is acceptable, but a stale one skews effective-cost.
const defaultEURtoUSD = 1.08

// onDemandEURHourly is a pinned snapshot of Hetzner Cloud hourly on-demand
// prices in EUR (gross, incl. the IPv4 surcharge is excluded — server-only),
// keyed by server type. Hetzner prices a given type identically across its EU
// locations; US locations (ash, hil) carry a small premium folded into the live
// refresh, so the pinned table is the EU baseline. Regenerate by reading
// ServerType.Pricings (no credentials needed for the public catalogue).
var onDemandEURHourly = map[string]float64{
	// CX (shared Intel/AMD).
	"cx22": 0.0060, "cx32": 0.0113, "cx42": 0.0273, "cx52": 0.0540,
	// CPX (shared AMD).
	"cpx11": 0.0070, "cpx21": 0.0120, "cpx31": 0.0230, "cpx41": 0.0440, "cpx51": 0.0860,
	// CAX (shared Ampere Arm64).
	"cax11": 0.0059, "cax21": 0.0110, "cax31": 0.0220, "cax41": 0.0430,
	// CCX (dedicated AMD).
	"ccx13": 0.0200, "ccx23": 0.0390, "ccx33": 0.0760, "ccx43": 0.1510,
	"ccx53": 0.3010, "ccx63": 0.4510,
}

func newPricing(eurToUSD float64, client hcloudClient, logger *slog.Logger) *pricing {
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

func priceKey(serverType, location string) string { return serverType + "|" + location }

// price returns USD/hour for a machine of the given shape. Reads only cached
// state (never blocks on the network), so it is safe on the List/seed path.
func (p *pricing) price(serverType, location string, capacity providerkit.CapacityType) float64 {
	if capacity == providerkit.CapacityBareMetal {
		return 0 // owned hardware, already paid for
	}
	p.mu.Lock()
	v, ok := p.hourly[priceKey(serverType, location)]
	p.mu.Unlock()
	if ok {
		return v
	}
	// Cold cache: fall back to the pinned EUR table converted to USD.
	return onDemandEURHourly[serverType] * p.eurToUSD
}

// refresh fetches the current hourly price for each (serverType, location) pair
// and caches it (already USD — the client converts EUR→USD). Best-effort: a
// fetch error leaves the prior (or fallback) value. Call it once at startup and
// on a timer; never on the List hot path. Returns the number of pairs that
// failed to refresh.
func (p *pricing) refresh(ctx context.Context, pairs []pricePair) int {
	failures := 0
	for _, pr := range pairs {
		v, err := p.client.PriceUSD(ctx, pr.serverType, pr.location)
		if err != nil {
			failures++
			if p.logger != nil {
				p.logger.Warn("pricing: price fetch failed; keeping fallback",
					"server_type", pr.serverType, "location", pr.location, "err", err)
			}
			continue
		}
		p.mu.Lock()
		p.hourly[priceKey(pr.serverType, pr.location)] = v
		p.mu.Unlock()
	}
	return failures
}

// pricePair identifies one (serverType, location) to refresh pricing for.
type pricePair struct {
	serverType string
	location   string
}
