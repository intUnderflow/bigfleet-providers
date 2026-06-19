package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// pricing supplies Machine.price_per_hour (USD/hour). On-demand comes from a
// pinned table (deterministic, no runtime pricing-API dependency on the List
// hot path); spot comes from DescribeSpotPriceHistory, cached and refreshed on
// a timer rather than per-List.
//
// Source of truth and refresh cadence are documented in the README. Expand
// onDemandUSD for the region/types you actually offer, or replace it with a
// cached pricing:GetProducts lookup.
type pricing struct {
	region      string
	onDemandUSD map[string]float64 // instanceType -> USD/hr (region-scoped)
	ec2         ec2Client
	logger      *slog.Logger

	mu   sync.Mutex
	spot map[string]float64 // "instanceType|zone" -> last fetched USD/hr
}

// onDemandUSEast1 is a pinned snapshot of us-east-1 on-demand prices for the
// pinned instance types. A real deployment regenerates this per region with a
// small offline script; it is not load-bearing for correctness (it feeds the
// engine's relative cost ranking), but keep it roughly accurate.
var onDemandUSEast1 = map[string]float64{
	"m6i.large": 0.096, "m6i.xlarge": 0.192, "m6i.2xlarge": 0.384, "m6i.4xlarge": 0.768, "m6i.8xlarge": 1.536,
	"m7g.large": 0.0816, "m7g.xlarge": 0.1632, "m7g.2xlarge": 0.3264, "m7g.4xlarge": 0.6528,
	"c6i.large": 0.085, "c6i.xlarge": 0.17, "c6i.2xlarge": 0.34, "c6i.4xlarge": 0.68,
	"c7g.large": 0.0725, "c7g.xlarge": 0.145, "c7g.2xlarge": 0.29, "c7g.4xlarge": 0.58,
	"r6i.large": 0.126, "r6i.xlarge": 0.252, "r6i.2xlarge": 0.504, "r6i.4xlarge": 1.008,
	"g5.xlarge": 1.006, "g5.2xlarge": 1.212, "g5.4xlarge": 1.624, "g5.12xlarge": 5.672,
}

func newPricing(region string, ec2 ec2Client, logger *slog.Logger) *pricing {
	return &pricing{
		region:      region,
		onDemandUSD: onDemandUSEast1,
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
func (p *pricing) refresh(ctx context.Context, pairs []spotPair) {
	for _, pr := range pairs {
		v, err := p.ec2.SpotPriceUSD(ctx, pr.instanceType, pr.zone)
		if err != nil {
			p.logger.Warn("pricing: spot price fetch failed; keeping fallback",
				"instance_type", pr.instanceType, "zone", pr.zone, "err", err)
			continue
		}
		p.mu.Lock()
		p.spot[spotKey(pr.instanceType, pr.zone)] = v
		p.mu.Unlock()
	}
}

// spotPair identifies one (instanceType, zone) to refresh spot pricing for.
type spotPair struct {
	instanceType string
	zone         string
}
