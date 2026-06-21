package main

import (
	"strings"
	"sync"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// interruption supplies Machine.interruption_probability for SPOT machines.
// This is correctness-critical: the engine ranks capacity by
// effective_cost = price + interruption_probability × penalty, so a SPOT machine
// that reports 0 looks free-and-safe and is handed workloads it should never
// run. A SPOT machine here therefore NEVER reports 0 — an unknown family falls
// back to a non-zero default bucket.
//
// GCE exposes no clean per-instance preemption-probability API, so the value is
// provider-declared from two signals (per the author guide):
//   - Forecast: a pinned, per-machine-family hourly preemption rate, seeded from
//     GCE Spot guidance. Used for Speculative slots and cold start.
//   - Observed: once a running Spot VM is preempted (GCE terminates it with the
//     preemption reason), markPreempted raises its probability toward 1.0. Wire
//     markPreempted to the reconciliation loop / a preemption-event signal in
//     production; persist+update from observed instance-hours for accuracy.
type interruption struct {
	mu       sync.Mutex
	observed map[string]float64 // machineID -> raised probability from a preemption
}

func newInterruption() *interruption {
	return &interruption{observed: make(map[string]float64)}
}

// defaultSpotProbability is the hourly preemption probability for a SPOT machine
// of an unknown family — deliberately non-zero (GCE Spot VMs can be preempted at
// any time). A conservative middle estimate.
const defaultSpotProbability = 0.05

// observedPreemptionProbability is the elevated hourly interruption probability
// recorded for a SPOT slot once a real GCE preemption of it has been observed.
// A completed preemption is a near-certain signal, so this is raised toward 1.0
// (well above every forecast bucket): the cost engine then treats a slot with
// proven preemption history as far riskier than one carrying only a forecast.
const observedPreemptionProbability = 0.9

// familyProbability is a pinned snapshot of hourly preemption-probability
// estimates per GCE machine family for Spot VMs. Refine from observed
// preemptions / instance-hours; values drift by family and zone demand. Keyed by
// the family prefix (the bit before the first '-').
var familyProbability = map[string]float64{
	"e2":  0.03, // cost-optimised, broad supply
	"n2":  0.04,
	"n2d": 0.04,
	"c2":  0.07, // compute-optimised, tighter supply
	"c3":  0.06,
	"m1":  0.10, // memory-optimised, scarce
	"a2":  0.15, // accelerator, scarcest
	"a3":  0.20,
	"g2":  0.12,
}

// family extracts the machine family prefix (e.g. "n2" from "n2-standard-8").
func family(machineType string) string {
	if i := strings.IndexByte(machineType, '-'); i > 0 {
		return machineType[:i]
	}
	return machineType
}

// forecast returns the pinned hourly interruption probability for a SPOT machine
// of the given type. 0 for non-spot. Never 0 for spot.
func (in *interruption) forecast(machineType string, capacity providerkit.CapacityType) float64 {
	if capacity != providerkit.CapacitySpot {
		return 0
	}
	if p, ok := familyProbability[family(machineType)]; ok {
		return p
	}
	return defaultSpotProbability
}

// probability returns the interruption probability to publish for a machine: the
// observed value once a preemption has been seen (closer to 1.0), otherwise the
// forecast. Always the real value, never 0 for spot.
func (in *interruption) probability(machineID, machineType string, capacity providerkit.CapacityType) float64 {
	base := in.forecast(machineType, capacity)
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

// markPreempted records that a running Spot VM has been (or is being) preempted,
// raising its observed probability. Wire this to the reconciliation loop in
// production.
func (in *interruption) markPreempted(machineID string, probability float64) {
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
