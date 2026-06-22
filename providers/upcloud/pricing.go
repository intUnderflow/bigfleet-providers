package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// pricing supplies Machine.price_per_hour (USD/hour). UpCloud cloud servers are
// on-demand only — there is no spot market — so every price is the published
// hourly on-demand rate for a plan.
//
// Live prices are the source of truth. A background refresher
// (runPriceRefresher) periodically pulls the UpCloud /price endpoint into a
// mutex-guarded map off the List hot path, so List/Describe read only cached
// state and never call the pricing API. The pinned EUR table below is a startup
// SEED + FALLBACK only: it warms the cold cache before the first refresh and
// covers a plan whenever a refresh fails or omits it, so the provider still
// produces a roughly-correct, currency-consistent price offline (the fake
// backend, a credential-free conformance / certification run) and survives a
// pricing-API outage. A frozen table would silently drift from the real bill;
// the live refresh keeps the cost field honest.
//
// UpCloud quotes a plan price in account-currency CREDITS per hour (1 credit =
// one cent), billed in EUR or USD depending on the account; the real client
// converts credits→EUR and this layer applies the configurable --eur-usd rate,
// so the value always reaches the engine as USD/hour. An operator can pin a
// per-offering USD figure via offering.PriceUSDPerHour, which wins over both.
type pricing struct {
	eurToUSD float64
	client   upcloudClient
	logger   *slog.Logger

	// mu guards live and warned. live holds the last successfully refreshed
	// USD/hour per plan; warned dedups the "no price anywhere" warning. Both are
	// read+written from the gRPC serving goroutines, the background reconciler, and
	// the price refresher, so guard them with mu.
	mu     sync.Mutex
	live   map[string]float64
	warned map[string]struct{}
}

// defaultEURtoUSD is a reasonable fallback FX rate; operators should pin a
// current rate via --eur-usd. The cost field is a relative ranking signal, so an
// approximate rate is acceptable, but a stale one skews effective-cost.
const defaultEURtoUSD = 1.08

// onDemandEURHourly is a pinned snapshot of UpCloud hourly on-demand prices in
// EUR, keyed by plan name. It seeds the cold price cache and is the fallback when
// a live refresh fails or omits a plan — the live UpCloud /price data is the
// source of truth and overlays it. UpCloud prices a plan close to identically
// across zones (a few zones carry a small premium), so this is the baseline; an
// operator can override per-offering via offering.PriceUSDPerHour. Derived from
// UpCloud's published simple-plan monthly prices (monthly / 730). Regenerate when
// UpCloud's catalogue changes.
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

func newPricing(eurToUSD float64, client upcloudClient, logger *slog.Logger) *pricing {
	if eurToUSD <= 0 {
		eurToUSD = defaultEURtoUSD
	}
	return &pricing{
		eurToUSD: eurToUSD,
		client:   client,
		logger:   logger,
		live:     map[string]float64{},
		warned:   map[string]struct{}{},
	}
}

// price returns USD/hour for an offering: the operator override if set, else the
// last live-refreshed price, else the pinned plan table converted to USD. Reads
// only cached state, so it is safe on the List/seed path.
func (p *pricing) price(off offering, capacity providerkit.CapacityType) float64 {
	if off.PriceUSDPerHour > 0 {
		return off.PriceUSDPerHour
	}
	return p.priceFor(off.Plan, off.Zone, capacity)
}

// priceFor returns USD/hour for a (plan, zone) with no operator override: the
// last live-refreshed price if present, else the pinned EUR table converted to
// USD. Reads only cached state (the live map and the static table), never the
// network, so it is safe on the List hot path. Used by Describe when recovering
// an orphan/rebound machine for which the originating offering may be gone.
func (p *pricing) priceFor(plan, _ string, capacity providerkit.CapacityType) float64 {
	if capacity == providerkit.CapacityBareMetal {
		return 0 // owned hardware, already paid for (not an UpCloud shape)
	}
	p.mu.Lock()
	v, ok := p.live[plan]
	p.mu.Unlock()
	if ok {
		return v
	}
	eur, ok := onDemandEURHourly[plan]
	if !ok {
		// No live price and no pinned fallback. Returning 0 would make the plan look
		// free and skew the engine's relative cost ranking; warn once so an operator
		// can add it to the table or set a per-offering override. Offered plans that
		// would land here are rejected at startup (requirePrices), so this path only
		// covers an orphan of an unknown plan recovered via Describe.
		p.warnUnknown(plan)
		return 0
	}
	return eur * p.eurToUSD
}

// refresh pulls live hourly prices for the given plans from the UpCloud API and
// overlays them (converted EUR→USD) on the live cache. Best-effort: an API error
// leaves the prior live values and the pinned fallback untouched. Call it once at
// startup (to seed live prices before the first List) and on a timer; never on
// the List hot path. Returns the number of plans left genuinely unpriced — an API
// failure (all requested plans) or a plan UpCloud did not price that also has no
// pinned fallback. A plan merely missing from the live result but covered by the
// pinned table is not counted (its fallback is correct).
func (p *pricing) refresh(ctx context.Context, plans []string) int {
	want := dedupeNonEmpty(plans)
	if len(want) == 0 {
		return 0
	}
	eur, err := p.client.DescribePlanPrices(ctx, want)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("pricing: live price refresh failed; keeping pinned fallback", "plans", len(want), "err", err)
		}
		return len(want)
	}
	p.mu.Lock()
	for plan, e := range eur {
		p.live[plan] = e * p.eurToUSD
	}
	p.mu.Unlock()
	unpriced := 0
	for _, plan := range want {
		if _, ok := eur[plan]; ok {
			continue
		}
		if _, pinned := onDemandEURHourly[plan]; pinned {
			continue // not live-priced, but the pinned fallback covers it
		}
		unpriced++
		p.warnUnknown(plan)
	}
	return unpriced
}

func (p *pricing) warnUnknown(plan string) {
	p.mu.Lock()
	if _, seen := p.warned[plan]; seen {
		p.mu.Unlock()
		return
	}
	p.warned[plan] = struct{}{}
	p.mu.Unlock()
	if p.logger != nil {
		p.logger.Warn("pricing: no live or pinned price for plan; reporting 0 (skews cost ranking — add it to the pinned table or set price_usd_per_hour on the offering)", "plan", plan)
	}
}
