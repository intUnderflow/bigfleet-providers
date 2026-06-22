package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// pricing supplies Machine.price_per_hour (USD/hour). Both on-demand and spot
// are live-refreshed off the List hot path into mutex-guarded maps; reads
// (price) only ever touch cached state, never the network.
//
//   - On-demand is refreshed from the public AWS Price List Bulk API (the
//     region offer JSON — no credentials) on a timer. The pinned per-region
//     table (onDemandByRegion) is the startup seed and the fallback: it seeds
//     a price before the first refresh and is kept whenever a refresh fails or
//     omits a type, so a successful refresh never zeroes an on-demand price.
//   - Spot is refreshed from DescribeSpotPriceHistory, one fetch per offered
//     (instanceType, zone) SPOT pair.
//
// Pricing is not load-bearing for correctness (it feeds the engine's relative
// cost ranking), but it must never read 0 for a priced offering — 0 wins the
// cost signal. The live refresh is the source of truth; the table is the floor.
type pricing struct {
	region string
	seed   map[string]float64 // pinned fallback on-demand table, resolved for `region`
	ec2    ec2Client
	logger *slog.Logger

	mu       sync.Mutex
	onDemand map[string]float64 // instanceType -> last live-refreshed USD/hr
	spot     map[string]float64 // "instanceType|zone" -> last fetched USD/hr
}

// onDemandBaselineRegion supplies the seed/fallback on-demand prices when a
// region has no pinned table of its own. Live refresh is still authoritative
// for that region; the baseline only floors the price until the first refresh.
const onDemandBaselineRegion = "us-east-1"

// onDemandByRegion holds the pinned on-demand seed/fallback prices (USD/hr) per
// region. These are NOT the runtime source of truth — the live Price List Bulk
// API refresh is (see pricing.refreshOnDemand). The table seeds a price before
// the first refresh and backstops a refresh that fails or omits a type, so the
// provider never emits 0 for a priced offering. us-east-1 is the baseline;
// us-west-2 is priced identically for these families (AWS's lowest US tier).
// Add a region's seed with cmd/genpricing and drop it in here.
var onDemandByRegion = map[string]map[string]float64{
	"us-east-1": onDemandUSEast1,
	// Oregon matches N. Virginia for these standard families.
	"us-west-2": onDemandUSEast1,
}

// onDemandUSEast1 is a pinned seed/fallback snapshot of us-east-1 on-demand
// prices, overlaid at runtime by the live Price List Bulk API refresh.
var onDemandUSEast1 = map[string]float64{
	"m6i.large": 0.096, "m6i.xlarge": 0.192, "m6i.2xlarge": 0.384, "m6i.4xlarge": 0.768, "m6i.8xlarge": 1.536,
	"m7g.large": 0.0816, "m7g.xlarge": 0.1632, "m7g.2xlarge": 0.3264, "m7g.4xlarge": 0.6528,
	"c6i.large": 0.085, "c6i.xlarge": 0.17, "c6i.2xlarge": 0.34, "c6i.4xlarge": 0.68,
	"c7g.large": 0.0725, "c7g.xlarge": 0.145, "c7g.2xlarge": 0.29, "c7g.4xlarge": 0.58,
	"r6i.large": 0.126, "r6i.xlarge": 0.252, "r6i.2xlarge": 0.504, "r6i.4xlarge": 1.008,
	"g5.xlarge": 1.006, "g5.2xlarge": 1.212, "g5.4xlarge": 1.624, "g5.12xlarge": 5.672,
}

func newPricing(region string, ec2 ec2Client, logger *slog.Logger) *pricing {
	seed, ok := onDemandByRegion[region]
	if !ok {
		// No pinned table for this region; fall back to the baseline. Stay quiet
		// for the empty region (the fake/dev backend doesn't price-rank).
		seed = onDemandByRegion[onDemandBaselineRegion]
		if region != "" && logger != nil {
			logger.Warn("pricing: no pinned on-demand seed table for region; live refresh is authoritative, baseline used as fallback",
				"region", region, "baseline", onDemandBaselineRegion)
		}
	}
	return &pricing{
		region:   region,
		seed:     seed,
		ec2:      ec2,
		logger:   logger,
		onDemand: make(map[string]float64),
		spot:     make(map[string]float64),
	}
}

func spotKey(instanceType, zone string) string { return instanceType + "|" + zone }

// price returns USD/hour for a machine of the given shape. Reads only cached
// state (never blocks on the network), so it is safe on the List/seed path.
func (p *pricing) price(instanceType, zone string, capacity providerkit.CapacityType) float64 {
	switch capacity {
	case providerkit.CapacityBareMetal:
		return 0 // already paid for
	case providerkit.CapacitySpot:
		p.mu.Lock()
		v, ok := p.spot[spotKey(instanceType, zone)]
		p.mu.Unlock()
		if ok {
			return v
		}
		// Cold cache: a conservative fraction of on-demand until refresh runs.
		return 0.3 * p.onDemandPrice(instanceType)
	default: // ON_DEMAND / RESERVED — reserved is a billing construct; price it
		// at on-demand unless you model a real reservation discount.
		return p.onDemandPrice(instanceType)
	}
}

// onDemandPrice returns the live-refreshed on-demand price for an instance type,
// falling back to the pinned seed table when no live price has been fetched yet
// (or the type was absent from the last refresh). Lock-guarded, non-blocking.
func (p *pricing) onDemandPrice(instanceType string) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.onDemand[instanceType]; ok {
		return v
	}
	return p.seed[instanceType]
}

// refresh fetches the current spot price for each (instanceType, zone) pair and
// caches it. Best-effort: a fetch error leaves the prior (or fallback) value.
// Call it once at startup and on a timer; never on the List hot path.
// Returns the number of pairs that failed to refresh.
func (p *pricing) refresh(ctx context.Context, pairs []spotPair) int {
	failures := 0
	for _, pr := range pairs {
		v, err := p.ec2.SpotPriceUSD(ctx, pr.instanceType, pr.zone)
		if err != nil {
			failures++
			p.logger.Warn("pricing: spot price fetch failed; keeping fallback",
				"instance_type", pr.instanceType, "zone", pr.zone, "err", err)
			continue
		}
		p.mu.Lock()
		p.spot[spotKey(pr.instanceType, pr.zone)] = v
		p.mu.Unlock()
	}
	return failures
}

// spotPair identifies one (instanceType, zone) to refresh spot pricing for.
type spotPair struct {
	instanceType string
	zone         string
}

// refreshOnDemand fetches live on-demand prices for the given instance types
// from the public AWS Price List Bulk API and overlays them on the live cache.
// It is the source of truth for on-demand pricing; the pinned seed table is
// kept only as a fallback. Best-effort and fail-closed: on a fetch error the
// prior (live or seed) values are untouched, and only strictly-positive prices
// overwrite — so a type the offer file omits or prices at 0 keeps its seed
// value rather than dropping to 0 (0 would win the cost-ranking signal). Call
// it once at startup and on a timer; never on the List hot path. Returns 1 if
// the fetch failed, else 0.
func (p *pricing) refreshOnDemand(ctx context.Context, instanceTypes []string) int {
	prices, err := p.ec2.OnDemandPricesUSD(ctx, instanceTypes)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("pricing: on-demand price fetch failed; keeping last-known prices", "err", err)
		}
		return 1
	}
	p.mu.Lock()
	for t, v := range prices {
		if v > 0 {
			p.onDemand[t] = v
		}
	}
	p.mu.Unlock()
	return 0
}
