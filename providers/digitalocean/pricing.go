package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// pricing supplies Machine.price_per_hour (USD/hour). DigitalOcean is on-demand
// only — there is no spot market — so every price is the published hourly
// on-demand rate for a size. DigitalOcean prices a size identically across all
// regions, so price is keyed by size slug alone (unlike a region-priced cloud).
//
// Prices come from a pinned USD table (deterministic, no runtime pricing
// dependency on the List hot path), refreshed out-of-band from the live
// Sizes.List catalogue on a timer rather than per-List. The table is not
// load-bearing for correctness (it feeds the engine's relative cost ranking);
// the live refresh is authoritative, so it is never hand-maintained.
type pricing struct {
	client doClient
	logger *slog.Logger

	mu     sync.Mutex
	hourly map[string]float64  // size slug -> last fetched USD/hr
	warned map[string]struct{} // sizes we've already warned have no price (dedupe log spam)
}

// onDemandUSDHourly is a pinned snapshot of DigitalOcean hourly on-demand prices
// in USD, keyed by size slug. DigitalOcean publishes a monthly and an hourly
// price per size; this is the hourly figure. Regenerate by reading Sizes.List
// (the public catalogue needs only a valid token, no special scope). The live
// refresh overlays authoritative values; this table keeps the fake backend and
// any catalogue outage deterministic and offline-correct.
var onDemandUSDHourly = map[string]float64{
	// Basic shared-CPU (s-*).
	"s-1vcpu-1gb":  0.00744,
	"s-1vcpu-2gb":  0.01488,
	"s-2vcpu-2gb":  0.01935,
	"s-2vcpu-4gb":  0.03571,
	"s-4vcpu-8gb":  0.07143,
	"s-8vcpu-16gb": 0.14286,
	// General Purpose (g-*).
	"g-2vcpu-8gb":   0.09226,
	"g-4vcpu-16gb":  0.18452,
	"g-8vcpu-32gb":  0.36905,
	"g-16vcpu-64gb": 0.73810,
	// CPU-Optimized (c-*).
	"c-2":  0.06250,
	"c-4":  0.12500,
	"c-8":  0.25000,
	"c-16": 0.50000,
	// Memory-Optimized (m-*).
	"m-2vcpu-16gb":   0.13393,
	"m-4vcpu-32gb":   0.26786,
	"m-8vcpu-64gb":   0.53571,
	"m-16vcpu-128gb": 1.07143,
}

func newPricing(client doClient, logger *slog.Logger) *pricing {
	return &pricing{
		client: client,
		logger: logger,
		hourly: make(map[string]float64),
		warned: make(map[string]struct{}),
	}
}

// price returns USD/hour for a machine of the given size. Reads only cached
// state (never blocks on the network), so it is safe on the List/seed path.
func (p *pricing) price(sizeSlug string, capacity providerkit.CapacityType) float64 {
	if capacity == providerkit.CapacityBareMetal {
		return 0 // owned hardware, already paid for (not a DigitalOcean shape)
	}
	p.mu.Lock()
	v, ok := p.hourly[sizeSlug]
	p.mu.Unlock()
	if ok {
		return v
	}
	if pinned, ok := onDemandUSDHourly[sizeSlug]; ok {
		return pinned
	}
	// No live and no pinned price. Returning 0 would make the size look free and
	// skew the engine's relative cost ranking, so warn once per size — an operator
	// can add it to the pinned table or fix a failing refresh. (0 stays a valid,
	// fleet-pessimistic price until a refresh fills it in.)
	p.warnUnknown(sizeSlug)
	return 0
}

// warnUnknown logs once per size that has no live or pinned price.
func (p *pricing) warnUnknown(sizeSlug string) {
	p.mu.Lock()
	_, already := p.warned[sizeSlug]
	if !already {
		p.warned[sizeSlug] = struct{}{}
	}
	p.mu.Unlock()
	if !already && p.logger != nil {
		p.logger.Warn("pricing: no live or pinned price for size; reporting 0 until a refresh succeeds (skews cost ranking — add it to the pinned table or check --price-refresh)", "size", sizeSlug)
	}
}

// refresh fetches the current hourly price for each size and caches it.
// Best-effort: a fetch error leaves the prior (or fallback) value. Call it once
// at startup and on a timer; never on the List hot path. Returns the number of
// sizes that failed to refresh.
func (p *pricing) refresh(ctx context.Context, sizes []string) int {
	failures := 0
	for _, size := range dedupeNonEmpty(sizes) {
		v, err := p.client.PriceUSD(ctx, size)
		if err != nil {
			failures++
			if p.logger != nil {
				p.logger.Warn("pricing: price fetch failed; keeping fallback", "size", size, "err", err)
			}
			continue
		}
		p.mu.Lock()
		p.hourly[size] = v
		delete(p.warned, size) // a real price arrived; re-warn if it ever regresses
		p.mu.Unlock()
	}
	return failures
}
