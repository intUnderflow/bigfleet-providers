package main

import (
	"math"
	"sync"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// interruption supplies Machine.interruption_probability for SPOT machines.
// This is correctness-critical: the engine ranks capacity by
// effective_cost = price + interruption_probability × penalty, so a SPOT machine
// that reports 0 looks free-and-safe and is handed workloads it should never
// run. A SPOT machine here therefore NEVER reports 0 — an unknown size falls
// back to a non-zero middle band.
//
// Two signals, per the author guide:
//   - Forecast (advisor): Azure publishes a per-(VM size, region) eviction-rate
//     BAND on the Spot advisor / pricing surfaces (0-5%, 5-10%, 10-15%, 15-20%,
//     20%+), expressed as a 30-day eviction fraction. evictionBand is a pinned
//     snapshot of those bands; refresh it on a timer in production. The band's
//     representative monthly fraction is converted to an hourly probability.
//   - Observed: once a running VM receives a Scheduled Events Preempt notice,
//     markWarning raises its probability toward 1.0. A node-side agent reads the
//     per-VM Scheduled Events endpoint
//     (http://169.254.169.254/metadata/scheduledevents) and POSTs Preempt events
//     to the provider's eviction ingest endpoint (eviction.go), which calls
//     markWarning; the background reconcile loop then propagates the raised value.
type interruption struct {
	mu       sync.Mutex
	observed map[string]float64 // machineID -> raised probability from a notice
}

func newInterruption() *interruption {
	return &interruption{observed: make(map[string]float64)}
}

// hoursPer30Days is the divisor used to convert a 30-day eviction fraction to an
// hourly probability: p_hour = 1 - (1 - m)^(1/720).
const hoursPer30Days = 720.0

// bandMonthlyFraction maps the five Azure eviction-rate bands (0-5%, 5-10%,
// 10-15%, 15-20%, 20%+) to a representative 30-day eviction fraction (the band
// midpoint; the open-ended top band uses 0.25).
var bandMonthlyFraction = [5]float64{0.025, 0.075, 0.125, 0.175, 0.25}

// hourlyFromBand converts an eviction-rate band index to an hourly interruption
// probability via p_hour = 1 - (1 - m)^(1/720), clamped to [0,1].
func hourlyFromBand(band int) float64 {
	if band < 0 {
		band = 0
	}
	if band > 4 {
		band = 4
	}
	m := bandMonthlyFraction[band]
	p := 1 - math.Pow(1-m, 1.0/hoursPer30Days)
	if p < 0 {
		return 0
	}
	if p > 1 {
		return 1
	}
	return p
}

// unknownSpotBand is used for a SPOT size not in evictionBand — the middle
// (10-15%) band. Deliberately non-zero: never report 0 for spot.
const unknownSpotBand = 2

// evictionBand is a pinned snapshot of Azure Spot eviction-rate bands
// (0=0-5% … 4=20%+) for the pinned VM sizes. Refresh from the Spot advisor;
// values drift.
var evictionBand = map[string]int{
	"Standard_D2s_v5": 0, "Standard_D4s_v5": 0, "Standard_D8s_v5": 1, "Standard_D16s_v5": 1,
	"Standard_D32s_v5": 2, "Standard_D48s_v5": 2, "Standard_D64s_v5": 3,
	"Standard_D2as_v5": 0, "Standard_D4as_v5": 0, "Standard_D8as_v5": 1, "Standard_D16as_v5": 1, "Standard_D32as_v5": 2,
	"Standard_F2s_v2": 1, "Standard_F4s_v2": 1, "Standard_F8s_v2": 1, "Standard_F16s_v2": 2,
	"Standard_F32s_v2": 2, "Standard_F48s_v2": 3, "Standard_F64s_v2": 3,
	"Standard_E2s_v5": 1, "Standard_E4s_v5": 1, "Standard_E8s_v5": 2, "Standard_E16s_v5": 2, "Standard_E32s_v5": 3,
	"Standard_NC24ads_A100_v4": 4, "Standard_NC48ads_A100_v4": 4, "Standard_NC96ads_A100_v4": 4,
}

// forecast returns the advisor-derived hourly interruption probability for a
// SPOT machine of the given size. 0 for non-spot. Never 0 for spot.
func (in *interruption) forecast(vmSize string, capacity providerkit.CapacityType) float64 {
	if capacity != providerkit.CapacitySpot {
		return 0
	}
	b, ok := evictionBand[vmSize]
	if !ok {
		b = unknownSpotBand
	}
	return hourlyFromBand(b)
}

// probability returns the interruption probability to publish for a machine: the
// observed value once a notice has been seen (closer to 1.0), otherwise the
// forecast. Always the real value, never 0 for spot.
func (in *interruption) probability(machineID, vmSize string, capacity providerkit.CapacityType) float64 {
	base := in.forecast(vmSize, capacity)
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

// markWarning records that a running spot VM has received a Scheduled Events
// Preempt notice, raising its observed probability. It is driven by the eviction
// ingest endpoint (eviction.go), which the node-side Scheduled Events agent
// POSTs Preempt notices to.
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
