package main

import (
	"log/slog"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// pricing supplies Machine.price_per_hour (USD/hour). UpCloud cloud servers are
// on-demand only — there is no spot market — so every price is the published
// hourly on-demand rate for a plan.
//
// UpCloud publishes a per-plan price; depending on the account it is billed in
// EUR or USD. To keep the cost field currency-consistent, prices come from a
// pinned per-plan EUR table converted to USD with a configurable rate
// (eurToUSD), or an operator-declared per-offering override
// (offering.PriceUSDPerHour). Either way the value reaches the engine as
// USD/hour. The table is not load-bearing for correctness (it feeds the engine's
// relative cost ranking), but keep it roughly accurate; pin a current rate via
// --eur-usd and document the conversion.
type pricing struct {
	eurToUSD float64
	logger   *slog.Logger
}

// defaultEURtoUSD is a reasonable fallback FX rate; operators should pin a
// current rate via --eur-usd. The cost field is a relative ranking signal, so an
// approximate rate is acceptable, but a stale one skews effective-cost.
const defaultEURtoUSD = 1.08

// onDemandEURHourly is a pinned snapshot of UpCloud hourly on-demand prices in
// EUR, keyed by plan name. UpCloud prices a plan close to identically across
// zones (a few zones carry a small premium), so this is the baseline; an
// operator can override per-offering via offering.PriceUSDPerHour. Derived from
// UpCloud's published simple-plan monthly prices (monthly / 730). Regenerate
// when UpCloud's catalogue changes.
var onDemandEURHourly = map[string]float64{
	// DEV plans (developer / burstable).
	"DEV-1xCPU-1GB": 0.0038,
	"DEV-1xCPU-2GB": 0.0076,
	"DEV-1xCPU-4GB": 0.0152,
	"DEV-2xCPU-4GB": 0.0229,
	"DEV-2xCPU-8GB": 0.0305,
	// General purpose plans.
	"1xCPU-1GB":    0.0063,
	"1xCPU-2GB":    0.0127,
	"2xCPU-4GB":    0.0254,
	"4xCPU-8GB":    0.0507,
	"6xCPU-16GB":   0.1015,
	"8xCPU-32GB":   0.2030,
	"12xCPU-48GB":  0.3045,
	"16xCPU-64GB":  0.4060,
	"20xCPU-96GB":  0.6090,
	"20xCPU-128GB": 0.8120,
}

func newPricing(eurToUSD float64, logger *slog.Logger) *pricing {
	if eurToUSD <= 0 {
		eurToUSD = defaultEURtoUSD
	}
	return &pricing{eurToUSD: eurToUSD, logger: logger}
}

// price returns USD/hour for an offering: the operator override if set, else the
// pinned plan table converted to USD. Reads only static state, so it is safe on
// the List/seed path.
func (p *pricing) price(off offering, capacity providerkit.CapacityType) float64 {
	if off.PriceUSDPerHour > 0 {
		return off.PriceUSDPerHour
	}
	return p.priceFor(off.Plan, off.Zone, capacity)
}

// priceFor returns USD/hour for a (plan, zone) with no operator override — the
// pinned EUR table converted to USD. Used by Describe when recovering an
// orphan/rebound machine for which the originating offering may be gone.
func (p *pricing) priceFor(plan, _ string, capacity providerkit.CapacityType) float64 {
	if capacity == providerkit.CapacityBareMetal {
		return 0 // owned hardware, already paid for (not an UpCloud shape)
	}
	eur, ok := onDemandEURHourly[plan]
	if !ok {
		// No pinned price. Returning 0 would make the plan look free and skew the
		// engine's relative cost ranking; warn once so an operator can add it to the
		// table or set a per-offering override. (0 stays a valid, fleet-pessimistic
		// price.)
		p.warnUnknown(plan)
		return 0
	}
	return eur * p.eurToUSD
}

var warnedPlans = map[string]struct{}{}

func (p *pricing) warnUnknown(plan string) {
	if _, seen := warnedPlans[plan]; seen {
		return
	}
	warnedPlans[plan] = struct{}{}
	if p.logger != nil {
		p.logger.Warn("pricing: no pinned price for plan; reporting 0 (skews cost ranking — add it to the pinned table or set price_usd_per_hour on the offering)", "plan", plan)
	}
}
