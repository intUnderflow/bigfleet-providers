package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// pricing supplies Machine.price_per_hour (USD/hour). On-demand comes from a
// pinned, region-keyed table (deterministic, no runtime pricing-API dependency
// on the List hot path); spot comes from DescribeSpotPriceHistory, cached and
// refreshed on a timer rather than per-List.
//
// On-demand prices are stable, so a pinned table is the right model: regenerate
// a region's table offline with `go run ./cmd/genpricing` (it reads the public
// AWS Price List Bulk API — no credentials) rather than calling a pricing API at
// runtime. The table is not load-bearing for correctness (it feeds the engine's
// relative cost ranking), but keep it roughly accurate.
type pricing struct {
	region      string
	onDemandUSD map[string]float64 // instanceType -> USD/hr, resolved for `region`
	ec2         ec2Client
	logger      *slog.Logger

	mu   sync.Mutex
	spot map[string]float64 // "instanceType|zone" -> last fetched USD/hr
}

// onDemandBaselineRegion supplies on-demand prices when a region has no pinned
// table of its own (the prices are then approximate — regenerate per region).
const onDemandBaselineRegion = "us-east-1"

// onDemandByRegion holds pinned on-demand prices (USD/hr) per region for the
// pinned instance types. us-east-1 is the authoritative baseline; us-west-2 is
// priced identically for these families (AWS's lowest US tier). Add a region by
// generating its table — see cmd/genpricing — and dropping it in here.
var onDemandByRegion = map[string]map[string]float64{
	"us-east-1": onDemandUSEast1,
	// Oregon matches N. Virginia for these standard families.
	"us-west-2": onDemandUSEast1,
}

// onDemandUSEast1 is a pinned snapshot of us-east-1 on-demand prices for the
// pinned instance types.
var onDemandUSEast1 = map[string]float64{
	"m6i.large": 0.096, "m6i.xlarge": 0.192, "m6i.2xlarge": 0.384, "m6i.4xlarge": 0.768, "m6i.8xlarge": 1.536,
	"m7g.large": 0.0816, "m7g.xlarge": 0.1632, "m7g.2xlarge": 0.3264, "m7g.4xlarge": 0.6528,
	"c6i.large": 0.085, "c6i.xlarge": 0.17, "c6i.2xlarge": 0.34, "c6i.4xlarge": 0.68,
	"c7g.large": 0.0725, "c7g.xlarge": 0.145, "c7g.2xlarge": 0.29, "c7g.4xlarge": 0.58,
	"r6i.large": 0.126, "r6i.xlarge": 0.252, "r6i.2xlarge": 0.504, "r6i.4xlarge": 1.008,
	"g5.xlarge": 1.006, "g5.2xlarge": 1.212, "g5.4xlarge": 1.624, "g5.12xlarge": 5.672,
}

func newPricing(region string, ec2 ec2Client, logger *slog.Logger) *pricing {
	table, ok := onDemandByRegion[region]
	if !ok {
		// No pinned table for this region; fall back to the baseline. Stay quiet
		// for the empty region (the fake/dev backend doesn't price-rank).
		table = onDemandByRegion[onDemandBaselineRegion]
		if region != "" && logger != nil {
			logger.Warn("pricing: no pinned on-demand table for region; using baseline approximations — regenerate with cmd/genpricing",
				"region", region, "baseline", onDemandBaselineRegion)
		}
	}
	return &pricing{
		region:      region,
		onDemandUSD: table,
		ec2:         ec2,
		logger:      logger,
		spot:        make(map[string]float64),
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
		return 0.3 * p.onDemandUSD[instanceType]
	default: // ON_DEMAND / RESERVED — reserved is a billing construct; price it
		// at on-demand unless you model a real reservation discount.
		return p.onDemandUSD[instanceType]
	}
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
