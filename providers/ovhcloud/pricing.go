package main

import (
	"sync"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// pricing supplies Machine.price_per_hour (USD/hour). OVH Public Cloud is
// on-demand only — there is no spot market — so every price is the published
// hourly on-demand rate for the flavor. OVH publishes prices in EUR; we convert
// to USD with a configurable rate (eurToUSD).
//
// Unlike substrates with a real-time pricing API, OVHcloud exposes no reliable
// price API for v1, so prices come from a PINNED, version-controlled EUR table
// (pricing.go) — deterministic, no runtime dependency on the List hot path —
// refreshed MANUALLY when OVH changes its catalogue (see docs/configuration.md
// and the table comment). The table is not load-bearing for correctness (it
// feeds the engine's relative cost ranking), but keep it roughly accurate.
type pricing struct {
	eurToUSD float64

	mu        sync.RWMutex
	overrides map[string]float64 // flavor -> USD/hr operator override (optional)
}

// defaultEURtoUSD is a reasonable fallback FX rate; operators should pin a
// current rate via --eur-usd. The cost field is a relative ranking signal, so
// an approximate rate is acceptable, but a stale one skews effective-cost.
const defaultEURtoUSD = 1.08

// onDemandEURHourly is a pinned snapshot of OVH Public Cloud hourly on-demand
// prices in EUR (ex-VAT), keyed by flavor. OVH prices a given flavor identically
// across its regions, so the table is region-agnostic. Sourced from the public
// OVH Public Cloud catalogue; regenerate by hand from
// https://www.ovhcloud.com/en/public-cloud/prices/ and bump the comment date.
//
// Last refreshed: 2026-06 (approximate; verify before relying on absolute cost).
var onDemandEURHourly = map[string]float64{
	// Discovery (shared) — b3 / d2.
	"d2-2": 0.0048, "d2-4": 0.0096, "d2-8": 0.0192,
	"b3-8": 0.0270, "b3-16": 0.0530, "b3-32": 0.1070, "b3-64": 0.2130,
	// General Purpose (balanced) — b2.
	"b2-7": 0.0260, "b2-15": 0.0520, "b2-30": 0.1040, "b2-60": 0.2080, "b2-120": 0.4160,
	// CPU-optimised — c2 / c3.
	"c2-7": 0.0290, "c2-15": 0.0580, "c2-30": 0.1160, "c2-60": 0.2320, "c2-120": 0.4640,
	"c3-4": 0.0190, "c3-8": 0.0390, "c3-16": 0.0770, "c3-32": 0.1540,
	// RAM-optimised — r2 / r3.
	"r2-15": 0.0330, "r2-30": 0.0660, "r2-60": 0.1320, "r2-120": 0.2640, "r2-240": 0.5280,
	"r3-16": 0.0340, "r3-32": 0.0680, "r3-64": 0.1360, "r3-128": 0.2720,
	// GPU — t1 / t2 (NVIDIA V100), a10 / l4 (carry an accelerator label too).
	"t1-45": 1.9900, "t1-90": 3.0700, "t1-180": 6.1400,
	"t2-45": 1.9900, "t2-90": 3.0700, "t2-180": 6.1400,
	"a10-45": 0.8600, "a10-90": 1.7200, "l4-90": 0.7500,
}

func newPricing(eurToUSD float64) *pricing {
	if eurToUSD <= 0 {
		eurToUSD = defaultEURtoUSD
	}
	return &pricing{eurToUSD: eurToUSD, overrides: make(map[string]float64)}
}

// setOverride pins an explicit USD/hour for a flavor (e.g. an operator's
// negotiated rate or a flavor missing from the pinned table). Safe to call at
// startup before serving.
func (p *pricing) setOverride(flavor string, usd float64) {
	p.mu.Lock()
	p.overrides[flavor] = usd
	p.mu.Unlock()
}

// price returns USD/hour for a machine of the given flavor. Reads only in-memory
// state (never blocks on the network), so it is safe on the List/seed path.
func (p *pricing) price(flavor string, capacity providerkit.CapacityType) float64 {
	if capacity == providerkit.CapacityBareMetal {
		return 0 // owned hardware, already paid for
	}
	p.mu.RLock()
	v, ok := p.overrides[flavor]
	p.mu.RUnlock()
	if ok {
		return v
	}
	return onDemandEURHourly[flavor] * p.eurToUSD
}
