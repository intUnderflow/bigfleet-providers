package main

import (
	"sync"

	"github.com/intUnderflow/bigfleet-providers/internal/providerkit"
)

// interruption supplies Machine.interruption_probability for SPOT machines.
// This is correctness-critical: the engine ranks capacity by
// effective_cost = price + interruption_probability × penalty, so a SPOT
// machine that reports 0 looks free-and-safe and is handed workloads it should
// never run. A SPOT machine here therefore NEVER reports 0 — an unknown type
// falls back to a non-zero middle bucket.
//
// Two signals, per the author guide:
//   - Forecast (advisor): the AWS Spot Instance Advisor publishes a per-
//     (region, instance-type) interruption-frequency bucket. advisorBucket is
//     a pinned snapshot of those buckets; refresh it from the advisor JSON feed
//     on a timer in production.
//   - Observed: once a running instance receives a rebalance recommendation or
//     the 2-minute spot-interruption notice, markWarning raises its probability
//     toward 1.0. Wire markWarning to an EventBridge rule / the instance's IMDS
//     `spot/instance-action` in production.
type interruption struct {
	mu       sync.Mutex
	observed map[string]float64 // machineID -> raised probability from a notice
}

func newInterruption() *interruption {
	return &interruption{observed: make(map[string]float64)}
}

// bucketProbability maps the five advisor frequency buckets (<5%, 5–10%,
// 10–15%, 15–20%, >20%) to a representative hourly probability (bucket
// midpoint; the open-ended top bucket uses 0.30).
var bucketProbability = [5]float64{0.025, 0.075, 0.125, 0.175, 0.30}

// unknownSpotBucket is used for a SPOT type not in advisorBucket — the middle
// (10–15%) bucket. Deliberately non-zero: never report 0 for spot.
const unknownSpotBucket = 2

// advisorBucket is a pinned snapshot of Spot Instance Advisor interruption
// buckets (0=<5% … 4=>20%) for the pinned instance types. Refresh from the
// advisor feed; values drift.
var advisorBucket = map[string]int{
	"m6i.large": 0, "m6i.xlarge": 0, "m6i.2xlarge": 1, "m6i.4xlarge": 1, "m6i.8xlarge": 2,
	"m7g.large": 0, "m7g.xlarge": 0, "m7g.2xlarge": 1, "m7g.4xlarge": 1,
	"c6i.large": 1, "c6i.xlarge": 1, "c6i.2xlarge": 2, "c6i.4xlarge": 2,
	"c7g.large": 0, "c7g.xlarge": 1, "c7g.2xlarge": 1, "c7g.4xlarge": 2,
	"r6i.large": 1, "r6i.xlarge": 1, "r6i.2xlarge": 2, "r6i.4xlarge": 3,
	"g5.xlarge": 3, "g5.2xlarge": 3, "g5.4xlarge": 4, "g5.12xlarge": 4,
}

// forecast returns the advisor-derived hourly interruption probability for a
// SPOT machine of the given type. 0 for non-spot. Never 0 for spot.
func (in *interruption) forecast(instanceType string, capacity providerkit.CapacityType) float64 {
	if capacity != providerkit.CapacitySpot {
		return 0
	}
	b, ok := advisorBucket[instanceType]
	if !ok {
		b = unknownSpotBucket
	}
	return bucketProbability[b]
}

// probability returns the interruption probability to publish for a machine:
// the observed value once a notice has been seen (closer to 1.0), otherwise the
// forecast. Always the real value, never 0 for spot.
func (in *interruption) probability(machineID, instanceType string, capacity providerkit.CapacityType) float64 {
	base := in.forecast(instanceType, capacity)
	if capacity != providerkit.CapacitySpot {
		return base
	}
	in.mu.Lock()
	obs, ok := in.observed[machineID]
	in.mu.Unlock()
	if ok && obs > base {
		return obs
	}
	return base
}

// markWarning records that a running spot instance has received an
// interruption / rebalance notice, raising its observed probability. Wire this
// to EventBridge spot-interruption events in production.
func (in *interruption) markWarning(machineID string, probability float64) {
	if probability <= 0 {
		probability = 1.0
	}
	if probability > 1.0 {
		probability = 1.0 // keep interruption_probability within [0,1]
	}
	in.mu.Lock()
	in.observed[machineID] = probability
	in.mu.Unlock()
}

// clear drops any observed escalation for a machine (e.g. after it is deleted).
func (in *interruption) clear(machineID string) {
	in.mu.Lock()
	delete(in.observed, machineID)
	in.mu.Unlock()
}
