package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// pricing supplies Machine.price_per_hour (USD/hour). Both on-demand
// (pay-as-you-go) and Spot prices are sourced live from the Azure Retail Prices
// API (https://prices.azure.com/api/retail/prices — no credentials), cached and
// refreshed on a timer rather than per-List. The pinned, region-keyed table is a
// startup seed and a fallback only — once the background refresh runs, the live
// price is the source of truth.
//
// The seed table is not load-bearing for correctness (it feeds the engine's
// relative cost ranking, and only until the first refresh populates the live
// cache), but keep it roughly accurate so the cold window before the first
// refresh ranks sensibly. Regenerate a region's seed offline from the public
// Retail Prices API and drop it into onDemandByRegion.
type pricing struct {
	region       string
	seedOnDemand map[string]float64 // vmSize -> seed/fallback USD/hr, resolved for `region`
	client       azureClient
	logger       *slog.Logger

	mu           sync.Mutex
	liveOnDemand map[string]float64 // vmSize -> last fetched USD/hr (on-demand, region-scoped client)
	spot         map[string]float64 // vmSize -> last fetched USD/hr (Spot, region-scoped client)
	lastSuccess  time.Time          // wall-clock of the last fully-successful refresh (staleness signal)
}

// onDemandBaselineRegion supplies seed on-demand prices when a region has no
// pinned table of its own (the seed is then approximate until the live refresh
// populates — regenerate per region).
const onDemandBaselineRegion = "eastus"

// onDemandByRegion holds pinned on-demand seed prices (USD/hr) per region for the
// pinned VM sizes. eastus is the authoritative baseline; westeurope is priced
// from its own snapshot. These are the seed/fallback until the live refresh runs;
// add a region by generating its table from the Retail Prices API and dropping it
// in here.
var onDemandByRegion = map[string]map[string]float64{
	"eastus":     onDemandEastUS,
	"westeurope": onDemandWestEurope,
}

// onDemandEastUS is a pinned snapshot of East US pay-as-you-go Linux prices for
// the pinned VM sizes (USD/hr). Seed/fallback only — the live refresh is the
// source of truth once it runs.
var onDemandEastUS = map[string]float64{
	"Standard_D2s_v5": 0.096, "Standard_D4s_v5": 0.192, "Standard_D8s_v5": 0.384,
	"Standard_D16s_v5": 0.768, "Standard_D32s_v5": 1.536, "Standard_D48s_v5": 2.304, "Standard_D64s_v5": 3.072,
	"Standard_D2as_v5": 0.086, "Standard_D4as_v5": 0.172, "Standard_D8as_v5": 0.344,
	"Standard_D16as_v5": 0.688, "Standard_D32as_v5": 1.376,
	"Standard_F2s_v2": 0.0846, "Standard_F4s_v2": 0.1692, "Standard_F8s_v2": 0.3384,
	"Standard_F16s_v2": 0.6768, "Standard_F32s_v2": 1.3536, "Standard_F48s_v2": 2.0304, "Standard_F64s_v2": 2.7072,
	"Standard_E2s_v5": 0.126, "Standard_E4s_v5": 0.252, "Standard_E8s_v5": 0.504,
	"Standard_E16s_v5": 1.008, "Standard_E32s_v5": 2.016,
	"Standard_NC24ads_A100_v4": 3.6741, "Standard_NC48ads_A100_v4": 7.3482, "Standard_NC96ads_A100_v4": 14.6964,
}

// onDemandWestEurope is a pinned snapshot of West Europe pay-as-you-go Linux
// prices for the pinned VM sizes (USD/hr). Seed/fallback only.
var onDemandWestEurope = map[string]float64{
	"Standard_D2s_v5": 0.107, "Standard_D4s_v5": 0.214, "Standard_D8s_v5": 0.428,
	"Standard_D16s_v5": 0.856, "Standard_D32s_v5": 1.712, "Standard_D48s_v5": 2.568, "Standard_D64s_v5": 3.424,
	"Standard_D2as_v5": 0.0962, "Standard_D4as_v5": 0.1924, "Standard_D8as_v5": 0.3848,
	"Standard_D16as_v5": 0.7696, "Standard_D32as_v5": 1.5392,
	"Standard_F2s_v2": 0.0949, "Standard_F4s_v2": 0.1898, "Standard_F8s_v2": 0.3796,
	"Standard_F16s_v2": 0.7592, "Standard_F32s_v2": 1.5184, "Standard_F48s_v2": 2.2776, "Standard_F64s_v2": 3.0368,
	"Standard_E2s_v5": 0.1408, "Standard_E4s_v5": 0.2816, "Standard_E8s_v5": 0.5632,
	"Standard_E16s_v5": 1.1264, "Standard_E32s_v5": 2.2528,
	"Standard_NC24ads_A100_v4": 4.0416, "Standard_NC48ads_A100_v4": 8.0832, "Standard_NC96ads_A100_v4": 16.1664,
}

func newPricing(region string, client azureClient, logger *slog.Logger) *pricing {
	if logger == nil {
		// Normalise once so refresh and the region warning below never nil-panic.
		logger = slog.New(slog.DiscardHandler)
	}
	seed, ok := onDemandByRegion[region]
	if !ok {
		// No pinned seed table for this region; fall back to the baseline. Stay quiet
		// for the empty region (the fake/dev backend doesn't price-rank). The live
		// refresh will replace these approximations with the region's real prices.
		seed = onDemandByRegion[onDemandBaselineRegion]
		if region != "" {
			logger.Warn("pricing: no pinned on-demand seed table for region; using baseline approximations until the live refresh populates — regenerate from the Retail Prices API",
				"region", region, "baseline", onDemandBaselineRegion)
		}
	}
	return &pricing{
		region:       region,
		seedOnDemand: seed,
		client:       client,
		logger:       logger,
		liveOnDemand: make(map[string]float64),
		spot:         make(map[string]float64),
	}
}

// price returns USD/hour for a machine of the given shape. Reads only cached
// state (never blocks on the network), so it is safe on the List/seed path.
//
// Contract: every offered VM size must appear in the on-demand seed table
// (onDemandByRegion), enforced at startup (hasPrice), so a cold cache still
// prices it before the first refresh. The live on-demand price (refreshed from
// the Retail Prices API on the timer) is preferred once present. A spot size
// whose live price has not yet been fetched falls back to a fraction of its
// on-demand price; startup pre-warms prices before serving, so that cold window
// does not exist for pinned sizes. A size with no seed entry at all (only
// reachable via a recovered/orphan VM of an unpinned size) prices at a high
// sentinel rather than 0, so cost ranking never treats it as "free".
func (p *pricing) price(vmSize string, capacity providerkit.CapacityType) float64 {
	switch capacity {
	case providerkit.CapacityBareMetal:
		return 0 // already paid for (not offered by this provider, but priced correctly)
	case providerkit.CapacitySpot:
		p.mu.Lock()
		v, ok := p.spot[vmSize]
		p.mu.Unlock()
		if ok {
			return v
		}
		// Cold cache: a conservative fraction of pay-as-you-go until refresh runs.
		return 0.4 * p.onDemand(vmSize)
	default: // ON_DEMAND / RESERVED — reserved is a billing construct; price it at
		// pay-as-you-go unless you model a real reservation discount.
		return p.onDemand(vmSize)
	}
}

// unknownSizePriceUSD is a deliberately high per-hour price returned for a VM
// size with no seed on-demand entry. Offered sizes are rejected at startup
// (hasPrice), so this only applies to a recovered/orphan VM of an unpinned size:
// pricing it as expensive keeps cost ranking from treating it as "free".
const unknownSizePriceUSD = 1000.0

// onDemand returns the pay-as-you-go price for vmSize: the live refreshed price
// when present, else the pinned seed, else a conservative high sentinel when the
// size is unseeded (never 0, which would read as free).
func (p *pricing) onDemand(vmSize string) float64 {
	p.mu.Lock()
	v, ok := p.liveOnDemand[vmSize]
	p.mu.Unlock()
	if ok {
		return v
	}
	if seed, ok := p.seedOnDemand[vmSize]; ok {
		return seed
	}
	return unknownSizePriceUSD
}

// hasPrice reports whether vmSize has a seed on-demand price for this region (the
// baseline table when the region itself is unseeded). Used to fail startup loudly
// on a coverage gap rather than publish a misleading 0 — the live refresh keys
// off the same offered sizes, so a seeded size is also a refreshed one.
func (p *pricing) hasPrice(vmSize string) bool {
	_, ok := p.seedOnDemand[vmSize]
	return ok
}

// lastRefreshSuccess reports the wall-clock time of the last fully-successful
// refresh (zero until the first one completes). The caller surfaces its age as a
// staleness signal (metric/log).
func (p *pricing) lastRefreshSuccess() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSuccess
}

// refresh fetches the current on-demand price for every VM size and the Spot
// price for the Spot sizes, caching each. Best-effort: a fetch error leaves the
// prior (seed or last-good) value rather than zeroing it. Call it once at startup
// and on a timer; never on the List hot path. Returns the number of fetches that
// failed; on a fully-successful cycle (zero failures) it stamps the
// last-success time for staleness reporting.
func (p *pricing) refresh(ctx context.Context, onDemandSizes, spotSizes []string) int {
	failures := 0
	for _, size := range dedupeNonEmpty(onDemandSizes) {
		v, err := p.client.OnDemandPriceUSD(ctx, size)
		if err != nil {
			failures++
			p.logger.Warn("pricing: on-demand price fetch failed; keeping fallback",
				"vm_size", size, "err", err)
			continue
		}
		p.mu.Lock()
		p.liveOnDemand[size] = v
		p.mu.Unlock()
	}
	for _, size := range dedupeNonEmpty(spotSizes) {
		v, err := p.client.SpotPriceUSD(ctx, size)
		if err != nil {
			failures++
			p.logger.Warn("pricing: spot price fetch failed; keeping fallback",
				"vm_size", size, "err", err)
			continue
		}
		p.mu.Lock()
		p.spot[size] = v
		p.mu.Unlock()
	}
	if failures == 0 {
		p.mu.Lock()
		p.lastSuccess = time.Now()
		p.mu.Unlock()
	}
	return failures
}
