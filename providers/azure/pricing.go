package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// pricing supplies Machine.price_per_hour (USD/hour). On-demand (pay-as-you-go)
// comes from a pinned, region-keyed table (deterministic, no runtime
// pricing-API dependency on the List hot path); Spot comes from the Azure Retail
// Prices API, cached and refreshed on a timer rather than per-List.
//
// On-demand prices are stable, so a pinned table is the right model: regenerate
// a region's table offline from the public Retail Prices API
// (https://prices.azure.com/api/retail/prices — no credentials) rather than
// calling it on the hot path. The table is not load-bearing for correctness (it
// feeds the engine's relative cost ranking), but keep it roughly accurate.
type pricing struct {
	region      string
	onDemandUSD map[string]float64 // vmSize -> USD/hr, resolved for `region`
	client      azureClient
	logger      *slog.Logger

	mu   sync.Mutex
	spot map[string]float64 // vmSize -> last fetched USD/hr (Spot, region-scoped client)
}

// onDemandBaselineRegion supplies on-demand prices when a region has no pinned
// table of its own (the prices are then approximate — regenerate per region).
const onDemandBaselineRegion = "eastus"

// onDemandByRegion holds pinned on-demand prices (USD/hr) per region for the
// pinned VM sizes. eastus is the authoritative baseline; westeurope is priced
// from its own snapshot. Add a region by generating its table from the Retail
// Prices API and dropping it in here.
var onDemandByRegion = map[string]map[string]float64{
	"eastus":     onDemandEastUS,
	"westeurope": onDemandWestEurope,
}

// onDemandEastUS is a pinned snapshot of East US pay-as-you-go Linux prices for
// the pinned VM sizes (USD/hr).
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
// prices for the pinned VM sizes (USD/hr).
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
	table, ok := onDemandByRegion[region]
	if !ok {
		// No pinned table for this region; fall back to the baseline. Stay quiet
		// for the empty region (the fake/dev backend doesn't price-rank).
		table = onDemandByRegion[onDemandBaselineRegion]
		if region != "" {
			logger.Warn("pricing: no pinned on-demand table for region; using baseline approximations — regenerate from the Retail Prices API",
				"region", region, "baseline", onDemandBaselineRegion)
		}
	}
	return &pricing{
		region:      region,
		onDemandUSD: table,
		client:      client,
		logger:      logger,
		spot:        make(map[string]float64),
	}
}

// price returns USD/hour for a machine of the given shape. Reads only cached
// state (never blocks on the network), so it is safe on the List/seed path.
//
// Contract: every offered VM size must appear in the pinned on-demand table
// (onDemandByRegion), enforced at startup (hasPrice). A spot size whose live
// price has not yet been fetched falls back to a fraction of its on-demand price;
// startup pre-warms spot prices before serving, so that cold window does not
// exist for pinned sizes. A size with no pinned entry at all (only reachable via
// a recovered/orphan VM of an unpinned size) prices at a high sentinel rather
// than 0, so cost ranking never treats it as "free".
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
// size with no pinned on-demand entry. Offered sizes are rejected at startup
// (hasPrice), so this only applies to a recovered/orphan VM of an unpinned size:
// pricing it as expensive keeps cost ranking from treating it as "free".
const unknownSizePriceUSD = 1000.0

// onDemand returns the pinned pay-as-you-go price for vmSize, or a conservative
// high sentinel when the size is unpinned (never 0, which would read as free).
func (p *pricing) onDemand(vmSize string) float64 {
	if v, ok := p.onDemandUSD[vmSize]; ok {
		return v
	}
	return unknownSizePriceUSD
}

// hasPrice reports whether vmSize has a pinned on-demand price for this region
// (the baseline table when the region itself is unpinned). Used to fail startup
// loudly on a coverage gap rather than publish a misleading 0.
func (p *pricing) hasPrice(vmSize string) bool {
	_, ok := p.onDemandUSD[vmSize]
	return ok
}

// refresh fetches the current Spot price for each VM size and caches it.
// Best-effort: a fetch error leaves the prior (or fallback) value. Call it once
// at startup and on a timer; never on the List hot path. Returns the number of
// sizes that failed to refresh.
func (p *pricing) refresh(ctx context.Context, vmSizes []string) int {
	failures := 0
	for _, size := range dedupeNonEmpty(vmSizes) {
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
	return failures
}
