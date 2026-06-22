package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// pricing supplies Machine.price_per_hour (USD/hour). OVH Public Cloud is
// on-demand only — there is no spot market — so every price is the published
// hourly on-demand rate for the flavor. OVH publishes prices in EUR; we convert
// to USD with a configurable rate (eurToUSD).
//
// Prices come from OVHcloud's public order catalog (catalog.go), refreshed
// out-of-band on a timer into a mutex-guarded map — never on the List/Describe
// hot path (price() only reads cached state). The pinned EUR table below is no
// longer the source of truth: it is a dated startup SEED (so the fake backend
// and credential-free conformance are deterministic and offline) and a FALLBACK
// (so a catalog outage, or a flavor the catalog omits, still publishes a sane,
// non-zero price rather than 0 — which would always win the shard's cost
// ranking). An operator can also pin an explicit USD rate per flavor via a
// --flavor-price override, which wins over both.
//
// Staleness is observable: the live refresher records last-success on a metric
// and logs loudly when it falls back to the seed (source=manual), so a silently
// drifting price can never go unnoticed.
type pricing struct {
	eurToUSD float64
	source   priceSource // live catalog source; nil in fake/dev (seed only)
	logger   *slog.Logger

	mu        sync.RWMutex
	overrides map[string]float64 // flavor -> USD/hr operator override (optional)
	live      map[string]float64 // flavor -> USD/hr from the last successful catalog refresh
	lastOK    time.Time          // last successful live refresh; zero = never (source=manual)
}

// defaultEURtoUSD is a reasonable fallback FX rate; operators should pin a
// current rate via --eur-usd. The cost field is a relative ranking signal, so
// an approximate rate is acceptable, but a stale one skews effective-cost.
const defaultEURtoUSD = 1.08

// onDemandEURHourly is a DATED SEED of OVH Public Cloud hourly on-demand prices
// in EUR (ex-VAT), keyed by flavor — the startup default and the fallback when
// the live catalog is unreachable or omits a flavor. It is NOT the source of
// truth: the live catalog (catalog.go) overlays it on a timer. OVH prices a
// given flavor identically across its regions, so the table is region-agnostic.
//
// Seeded: 2026-06 from the public OVH order catalog. Treat these as approximate
// (they drift between refreshes of this file); rely on the live refresh for
// absolute cost, and keep this roughly current so a catalog outage degrades
// gracefully.
var onDemandEURHourly = map[string]float64{
	// Discovery (shared) — b3 / d2.
	"d2-2": 0.0104, "d2-4": 0.0206, "d2-8": 0.0372,
	"b3-8": 0.0512, "b3-16": 0.1023, "b3-32": 0.2046, "b3-64": 0.4092,
	// General Purpose (balanced) — b2.
	"b2-7": 0.0709, "b2-15": 0.1342, "b2-30": 0.2715, "b2-60": 0.5260, "b2-120": 1.0330,
	// CPU-optimised — c2 / c3.
	"c2-7": 0.1018, "c2-15": 0.1976, "c2-30": 0.3984, "c2-60": 0.7790, "c2-120": 1.5400,
	"c3-4": 0.0457, "c3-8": 0.0913, "c3-16": 0.1825, "c3-32": 0.3650,
	// RAM-optimised — r2 / r3.
	"r2-15": 0.1018, "r2-30": 0.1176, "r2-60": 0.2288, "r2-120": 0.4610, "r2-240": 0.9060,
	"r3-16": 0.0663, "r3-32": 0.1324, "r3-64": 0.2648, "r3-128": 0.5300,
	// GPU — t1 (V100), t2 (V100S), a10 (A10), l4 (L4) — carry an accelerator label too.
	"t1-45": 0.7000, "t1-90": 1.4000, "t1-180": 2.8000,
	"t2-45": 0.8000, "t2-90": 1.6000, "t2-180": 3.2000,
	"a10-45": 0.7600, "a10-90": 1.5200, "l4-90": 0.7500,
}

func newPricing(eurToUSD float64, source priceSource, logger *slog.Logger) *pricing {
	if eurToUSD <= 0 {
		eurToUSD = defaultEURtoUSD
	}
	return &pricing{
		eurToUSD:  eurToUSD,
		source:    source,
		logger:    logger,
		overrides: make(map[string]float64),
		live:      make(map[string]float64),
	}
}

// setOverride pins an explicit USD/hour for a flavor (e.g. an operator's
// negotiated rate or a flavor missing from the seed table and the catalog). Safe
// to call at startup before serving. Overrides win over live and seed prices.
func (p *pricing) setOverride(flavor string, usd float64) {
	p.mu.Lock()
	p.overrides[flavor] = usd
	p.mu.Unlock()
}

// price returns USD/hour for a flavor, in precedence order: an operator override,
// then the last live catalog price, then the dated EUR seed converted to USD.
// Reads only in-memory state (never blocks on the network), so it is safe on the
// List/Describe/seed hot path. This provider only ever produces ON_DEMAND
// machines (bare-metal/spot/reserved offerings are rejected at construction), so
// there is no capacity-type branch here.
func (p *pricing) price(flavor string) float64 {
	p.mu.RLock()
	if v, ok := p.overrides[flavor]; ok {
		p.mu.RUnlock()
		return v
	}
	if v, ok := p.live[flavor]; ok {
		p.mu.RUnlock()
		return v
	}
	p.mu.RUnlock()
	return onDemandEURHourly[flavor] * p.eurToUSD
}

// known reports whether a flavor has a guaranteed non-zero price WITHOUT relying
// on a live fetch — an override or a seed-table entry. newOVHBackend uses it to
// fail closed at startup: an offering whose flavor has neither would publish
// price_per_hour=0 (effectively free), the global minimum of the shard's cost
// ranking, so it always wins. Deliberately does NOT count live prices: the live
// catalog may be momentarily unreachable at startup, and we must never let a
// flavor's price collapse to 0 if a later refresh fails. The seed is the
// fail-closed guarantee; the catalog overlays it for accuracy.
func (p *pricing) known(flavor string) bool {
	p.mu.RLock()
	_, ov := p.overrides[flavor]
	p.mu.RUnlock()
	if ov {
		return true
	}
	_, ok := onDemandEURHourly[flavor]
	return ok
}

// lastRefresh returns the time of the last successful live catalog refresh, or
// the zero time if none has succeeded (so prices are coming from the seed table —
// source=manual). Used for the staleness metric/log.
func (p *pricing) lastRefresh() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastOK
}

// refresh pulls current hourly prices from the live catalog source and overlays
// them (converted EUR->USD) onto the live map. Best-effort and off the hot path:
// call once at startup and on a timer, never on List.
//
//   - With no source (fake backend / dev), it is a no-op: prices stay on the
//     deterministic seed table, so conformance is offline and reproducible.
//   - A fetch error leaves the prior live/seed prices in place and returns the
//     error, so the caller can mark the refresh failed and warn (source=manual).
//   - On success it returns the number of requested flavors the catalog did NOT
//     price (still covered by the seed/override) and logs them, so an operator
//     can see which offerings are silently on the fallback.
func (p *pricing) refresh(ctx context.Context, flavors []string) (missing int, err error) {
	if p.source == nil {
		return 0, nil
	}
	want := dedupeNonEmpty(flavors)
	if len(want) == 0 {
		return 0, nil
	}
	eur, err := p.source.HourlyEUR(ctx, want)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("pricing: live catalog refresh failed; keeping previous/seed prices (source=manual)", "err", err)
		}
		return 0, err
	}

	p.mu.Lock()
	for flavor, e := range eur {
		p.live[flavor] = e * p.eurToUSD
	}
	p.lastOK = time.Now()
	p.mu.Unlock()

	var absent []string
	for _, f := range want {
		if _, ok := eur[f]; !ok {
			absent = append(absent, f)
		}
	}
	if len(absent) > 0 && p.logger != nil {
		p.logger.Warn("pricing: OVH catalog did not price some offered flavors; those use the dated seed/override (source=manual for them)", "flavors", absent)
	}
	return len(absent), nil
}
