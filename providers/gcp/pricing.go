package main

import (
	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// pricing supplies Machine.price_per_hour (USD/hour) from a pinned,
// region-keyed table. This is the v1 model recommended by the author guide: a
// version-controlled snapshot of the GCE on-demand rates (sourced once from the
// Cloud Billing Catalog API / public pricing page and refreshed on a cadence),
// so there is no pricing-API dependency on the List hot path and the numbers are
// deterministic for certification.
//
// Spot (preemptible) prices are dynamic and deeply discounted; we model them as
// a fixed fraction of the on-demand rate (spotFraction), which keeps the spot
// price non-zero and roughly accurate without a live lookup. The cost field is a
// relative ranking signal — effective_cost = price + interruption_probability ×
// penalty — so an approximate spot price is acceptable, but it must never be 0.
type pricing struct {
	region      string
	onDemandUSD map[string]float64 // machineType -> USD/hr, resolved for `region`
}

// spotFraction is the share of the on-demand price a Spot VM is billed at. GCE
// Spot discounts run ~60–91%; 0.4 is a conservative (high) estimate so the cost
// engine never under-prices spot.
const spotFraction = 0.4

// onDemandBaselineRegion supplies on-demand prices when a region has no pinned
// table of its own (the prices are then approximate — pin a per-region table).
const onDemandBaselineRegion = "us-central1"

// onDemandByRegion holds pinned on-demand prices (USD/hr) per region. us-central1
// is the authoritative baseline; add a region by pinning its table and dropping
// it in here.
var onDemandByRegion = map[string]map[string]float64{
	"us-central1": onDemandUSCentral1,
	// us-east1 matches us-central1 for these standard families.
	"us-east1": onDemandUSCentral1,
}

// onDemandUSCentral1 is a pinned snapshot of us-central1 on-demand prices (USD/hr)
// for the pinned machine types. Regenerate from the Cloud Billing Catalog.
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

func newPricing(region string) *pricing {
	table, ok := onDemandByRegion[region]
	if !ok {
		// No pinned table for this region; fall back to the baseline.
		table = onDemandByRegion[onDemandBaselineRegion]
	}
	return &pricing{region: region, onDemandUSD: table}
}

// price returns USD/hour for a machine of the given shape. Pure table lookup —
// never blocks on the network, so it is safe on the List/seed hot path.
func (p *pricing) price(machineType string, capacity providerkit.CapacityType) float64 {
	switch capacity {
	case providerkit.CapacityBareMetal:
		return 0 // GCE has no bare metal in this provider; defensive only
	case providerkit.CapacitySpot:
		return spotFraction * p.onDemandUSD[machineType]
	default: // ON_DEMAND / RESERVED — reserved is a billing construct; price it
		// at on-demand unless a real committed-use discount is modelled.
		return p.onDemandUSD[machineType]
	}
}
