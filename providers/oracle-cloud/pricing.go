package main

import (
	_ "embed"
	"fmt"
	"os"

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

// priceTable is the parsed prices.yaml. It is read-only after load, so price()
// needs no lock and is safe on the List/seed hot path.
type priceTable struct {
	PricedAt        string             `yaml:"priced_at"`
	Source          string             `yaml:"source"`
	Currency        string             `yaml:"currency"`
	FlexRates       map[string]rate    `yaml:"flex_rates"`
	DefaultFlexRate rate               `yaml:"default_flex_rate"`
	FixedHourly     map[string]float64 `yaml:"fixed_hourly"`
	SpotDiscount    float64            `yaml:"spot_discount"`
}

// pricing supplies Machine.price_per_hour (USD/hour) from the pinned table.
type pricing struct {
	table priceTable
}

// newPricing loads the pinned price table. When path is empty it uses the
// table embedded at build time (prices.yaml), so the provider always has a
// deterministic table — including the credential-free fake/certify run.
func newPricing(path string) (*pricing, error) {
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
	return &pricing{table: t}, nil
}

// price returns USD/hour for a machine of the given shape, sizing, and capacity.
// Bare metal is always 0 (owned/fixed capacity, already paid for); preemptible
// applies the table's spot discount to the on-demand price. Reads only the
// in-memory table, so it never blocks — safe on the List/seed hot path.
func (p *pricing) price(shape string, ocpus, memGiB float64, capacity providerkit.CapacityType) float64 {
	if capacity == providerkit.CapacityBareMetal {
		return 0
	}
	base := p.onDemand(shape, ocpus, memGiB)
	if capacity == providerkit.CapacitySpot {
		base *= p.table.SpotDiscount
	}
	return base
}

// onDemand returns the on-demand USD/hour for a shape: flexible shapes are priced
// per-OCPU + per-GB (× the launch sizing), fixed shapes by their whole-instance
// rate. An unknown flexible shape falls back to the default flex rate; an unknown
// fixed shape yields 0 (the cost field is a relative ranking signal).
func (p *pricing) onDemand(shape string, ocpus, memGiB float64) float64 {
	if isFlexShape(shape) {
		r, ok := p.table.FlexRates[shape]
		if !ok {
			r = p.table.DefaultFlexRate
		}
		return ocpus*r.OCPU + memGiB*r.MemoryGB
	}
	return p.table.FixedHourly[shape]
}
