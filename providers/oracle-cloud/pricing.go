package main

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

//go:embed prices.yaml
var embeddedPrices []byte

// rate is a flexible-shape (per-OCPU, per-GB) hourly rate pair.
type rate struct {
	OCPU     float64 `yaml:"ocpu"`
	MemoryGB float64 `yaml:"memory_gb"`
}

// priceTable is the parsed prices.yaml. It is the startup SEED and the FALLBACK
// only: it primes prices before the first live refresh and stands in for any
// shape a refresh cannot price. The live OCI price list is the source of truth.
type priceTable struct {
	PricedAt        string             `yaml:"priced_at"`
	Source          string             `yaml:"source"`
	Currency        string             `yaml:"currency"`
	FlexRates       map[string]rate    `yaml:"flex_rates"`
	DefaultFlexRate rate               `yaml:"default_flex_rate"`
	FixedHourly     map[string]float64 `yaml:"fixed_hourly"`
	SpotDiscount    float64            `yaml:"spot_discount"`
}

// pricing supplies Machine.price_per_hour (USD/hour). The rate tables are seeded
// from prices.yaml and then refreshed out-of-band from the live OCI price list
// (refresh, on a timer — never on the List hot path) into the same mutex-guarded
// maps that price() reads, so List/Describe always read live-or-seed prices
// without ever blocking on a pricing call.
type pricing struct {
	source       priceSource
	logger       *slog.Logger
	spotDiscount float64 // immutable after load; the seed table's discount

	mu          sync.Mutex
	flexRates   map[string]rate    // flexible-shape family -> per-OCPU/per-GB USD rate
	defaultFlex rate               // fallback per-OCPU/per-GB for an unlisted flex family
	fixedHourly map[string]float64 // fixed shape -> whole-instance USD/hour
	lastSuccess time.Time          // wall time of the last successful refresh (zero = never)
}

// newPricing loads the seed/fallback price table and wires the live source. When
// path is empty it uses the table embedded at build time (prices.yaml), so the
// provider always has a deterministic seed — including the credential-free
// fake/certify run. The returned pricing serves seed prices until the first
// refresh; call refresh at startup (and on a timer) to pull live prices.
func newPricing(path string, source priceSource, logger *slog.Logger) (*pricing, error) {
	if source == nil {
		return nil, fmt.Errorf("pricing: nil price source")
	}
	data := embeddedPrices
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read price table %s: %w", path, err)
		}
		data = b
	}
	var t priceTable
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse price table: %w", err)
	}
	if t.SpotDiscount <= 0 || t.SpotDiscount > 1 {
		t.SpotDiscount = 0.5
	}
	return &pricing{
		source:       source,
		logger:       logger,
		spotDiscount: t.SpotDiscount,
		flexRates:    cloneFlexRates(t.FlexRates),
		defaultFlex:  t.DefaultFlexRate,
		fixedHourly:  cloneFixedHourly(t.FixedHourly),
	}, nil
}

// price returns USD/hour for a machine of the given shape, sizing, and capacity.
// Bare metal is always 0 (owned/fixed capacity, already paid for); preemptible
// applies the table's spot discount to the on-demand price. Reads only the
// in-memory (live-or-seed) tables under a short lock, so it never blocks on the
// network — safe on the List/seed hot path.
func (p *pricing) price(shape string, ocpus, memGiB float64, capacity providerkit.CapacityType) float64 {
	if capacity == providerkit.CapacityBareMetal {
		return 0
	}
	base := p.onDemand(shape, ocpus, memGiB)
	if capacity == providerkit.CapacitySpot {
		base *= p.spotDiscount
	}
	return base
}

// onDemand returns the on-demand USD/hour for a shape: flexible shapes are priced
// per-OCPU + per-GB (× the launch sizing), fixed shapes by their whole-instance
// rate. An unknown flexible shape falls back to the default flex rate; an unknown
// fixed shape yields 0 (caught by the fail-closed startup check for any offered,
// non-bare-metal shape).
func (p *pricing) onDemand(shape string, ocpus, memGiB float64) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if isFlexShape(shape) {
		r, ok := p.flexRates[shape]
		if !ok {
			r = p.defaultFlex
		}
		return ocpus*r.OCPU + memGiB*r.MemoryGB
	}
	return p.fixedHourly[shape]
}

// refresh pulls the live OCI price list and merges it into the rate tables. Call
// it once at startup and on a timer; never on the List hot path. A live rate of 0
// (e.g. the always-free Ampere A1 SKUs) is skipped so the shape keeps its seed
// value rather than being ranked as free. A fetch error leaves all prior values
// in place (the seed remains the fallback) and is returned to the caller.
func (p *pricing) refresh(ctx context.Context) error {
	live, err := p.source.Fetch(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for family, r := range live.flexRates {
		if r.OCPU <= 0 && r.MemoryGB <= 0 {
			if p.logger != nil {
				p.logger.Warn("pricing: live flex rate is zero; keeping seed/fallback", "shape", family)
			}
			continue
		}
		p.flexRates[family] = r
	}
	for shape, hourly := range live.fixedHourly {
		if hourly <= 0 {
			if p.logger != nil {
				p.logger.Warn("pricing: live fixed rate is zero; keeping seed/fallback", "shape", shape)
			}
			continue
		}
		p.fixedHourly[shape] = hourly
	}
	p.lastSuccess = time.Now()
	return nil
}

// lastRefresh returns the wall time of the last successful refresh (zero if a
// refresh has never succeeded — the tables still hold the prices.yaml seed).
func (p *pricing) lastRefresh() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSuccess
}

// stalenessSeconds reports how long ago the last successful refresh was, in
// seconds. It returns -1 when no refresh has ever succeeded, so a never-refreshed
// provider is distinguishable from a fresh one.
func (p *pricing) stalenessSeconds() float64 {
	last := p.lastRefresh()
	if last.IsZero() {
		return -1
	}
	return time.Since(last).Seconds()
}

func cloneFlexRates(in map[string]rate) map[string]rate {
	out := make(map[string]rate, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneFixedHourly(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
