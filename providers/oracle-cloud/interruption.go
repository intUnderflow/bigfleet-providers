package main

import (
	"sync"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// interruption supplies Machine.interruption_probability for SPOT (OCI
// Preemptible Instance) machines. This is correctness-critical: the shard ranks
// capacity by effective_cost = price + interruption_probability × penalty, so a
// SPOT machine that reports 0 looks free-and-safe and is handed workloads it
// should never run. A SPOT machine here therefore NEVER reports 0 — an unknown
// shape falls back to a non-zero default prior.
//
// Two signals, per the author guide (§4.4):
//   - Forecast: a conservative per-shape hourly prior (OCI does not publish a
//     spot-interruption-frequency feed the way AWS does), tunable via a prior
//     table. This is what the provider publishes today for every SPOT machine.
//   - Observed (live when --preemption-stream is set): markPreemption raises a
//     specific machine's probability toward 1.0 once a preemption signal is seen.
//     The preemptionPoller (preemption_poller.go) consumes OCI preemption-action
//     events from an OCI Streaming stream (fed by an Events rule) and calls
//     markPreemption ~2 minutes before the host is reclaimed. Without the stream
//     configured only the forecast prior is published (which already satisfies the
//     SPOT > 0 invariant).
type interruption struct {
	mu       sync.Mutex
	observed map[string]float64 // machineID -> raised probability from a preemption signal

	// priors maps a shape to a conservative hourly preemption prior; shapes
	// absent from the table use defaultPrior. Both are tunable so an operator can
	// reflect observed scarcity per (shape, AD, region).
	priors       map[string]float64
	defaultPrior float64
}

// defaultSpotPrior is the per-hour preemption prior for a SPOT shape with no
// configured prior. Deliberately non-zero and conservative: never report 0 for
// preemptible capacity.
const defaultSpotPrior = 0.10

// shapeSpotPriors is a pinned, tunable snapshot of conservative per-shape hourly
// preemption priors. Scarcer / larger shapes carry a higher prior. Refine these
// from observed preemption rates per (shape, AD, region).
var shapeSpotPriors = map[string]float64{
	"VM.Standard.E5.Flex": 0.08,
	"VM.Standard.E4.Flex": 0.08,
	"VM.Standard3.Flex":   0.10,
	"VM.Optimized3.Flex":  0.12,
	"VM.Standard.A1.Flex": 0.05,
	"VM.Standard.A2.Flex": 0.06,
	"VM.GPU.A10.1":        0.20,
	"VM.GPU.A10.2":        0.25,
}

func newInterruption() *interruption {
	priors := make(map[string]float64, len(shapeSpotPriors))
	for k, v := range shapeSpotPriors {
		priors[k] = v
	}
	return &interruption{
		observed:     make(map[string]float64),
		priors:       priors,
		defaultPrior: defaultSpotPrior,
	}
}

// forecast returns the prior hourly preemption probability for a SPOT machine of
// the given shape. 0 for non-spot. Never 0 for spot.
func (in *interruption) forecast(shape string, capacity providerkit.CapacityType) float64 {
	if capacity != providerkit.CapacitySpot {
		return 0
	}
	if p, ok := in.priors[shape]; ok && p > 0 {
		return p
	}
	return in.defaultPrior
}

// probability returns the interruption probability to publish for a machine: the
// observed value once a preemption signal has been seen (closer to 1.0),
// otherwise the forecast. Always the real value, never 0 for spot.
func (in *interruption) probability(machineID, shape string, capacity providerkit.CapacityType) float64 {
	base := in.forecast(shape, capacity)
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

// markPreemption records that a running preemptible instance has received a
// preemption signal, raising its observed probability. The preemptionPoller calls
// it from a live OCI Streaming subscription when --preemption-stream is set.
func (in *interruption) markPreemption(machineID string, probability float64) {
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
