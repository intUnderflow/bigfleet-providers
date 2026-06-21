---
title: Pricing & interruption
description: How the Latitude.sh provider sources price_per_hour (USD, no FX) and why interruption_probability is a genuine zero on Latitude bare metal.
sidebar:
  order: 4
  label: Pricing & interruption
---

BigFleet ranks capacity by **effective cost** = `price_per_hour +
interruption_probability × penalty`. So a provider has to report both honestly.
On Latitude.sh the story is unusually simple, and this page explains exactly why.

## `price_per_hour` — published Latitude rates, in USD

Latitude.sh publishes a fixed hourly on-demand price per plan per site, **in
USD** — so unlike Hetzner's EUR prices, there is **no FX conversion**. The
provider sources that rate from the Plans API (`Plans.List` → the plan's
per-region `pricing.usd.hour`), picks the entry for the machine's site, and uses
the hourly figure directly:

```
price_per_hour (USD) = latitude_plan_hourly_USD[site]
```

- **Prices are cached and refreshed off the hot path.** At startup, and every
  `--price-refresh` (default 30m), the provider refreshes the price for each
  offered `(plan, site)` pair. `List`/`Get` read the cache and never block on the
  pricing API.
- **A pinned USD table is the fallback.** Common c/s/m/g series plans have pinned
  baseline hourly prices, so the fake backend, credential-free conformance, and a
  pricing-API outage all still produce a sensible `price_per_hour`. Live Latitude
  data overlays it (and picks up the small per-site premium).
- **The cost field is a relative ranking signal**, so the pinned table is not
  load-bearing for correctness — but keep `--price-refresh` non-zero so the live
  rates overlay the baseline, and a stale baseline doesn't skew effective-cost
  across the fleet.

## `interruption_probability` — a genuine zero

`interruption_probability` is the hourly chance the **provider** reclaims the
machine out from under the workload. It is **provider-declared only** — no
cluster can override it.

**Latitude.sh bare metal is on-demand only. There is no spot/preemptible
market.** Latitude does not reclaim a running on-demand server to satisfy other
demand. So the correct, real, provider-declared value is exactly **`0.0`** for
every machine.

This is the important distinction the conformance program checks: a zero here is
**not** a skipped or forgotten field — it is the *true* value for this substrate.
That is different from a spot machine declared at 0, which would be a bug
(`effective_cost` would understate the real risk and the machine would win
high-penalty workloads it should never get). Because Latitude has no spot tier,
the provider:

- declares `capacity_type = ON_DEMAND` for every machine,
- sets `interruption_probability = 0.0`,
- and **does not claim the `spot` conformance profile** — so the
  SPOT-`interruption_probability > 0` behaviors skip-as-pass rather than apply.

The provider also **rejects** a `spot` `capacity_type` in an offering at startup,
rather than silently mis-declaring a zero interruption probability for capacity
that doesn't exist.

## On-demand, not bare-metal — and why it matters for cost

It is tempting to model physical hardware as `capacity_type = BARE_METAL` with
`price_per_hour = 0` (the box is "already owned"). **That is wrong for
Latitude.sh**, because the box is *not* owned — it is rented by the hour and
released on `Delete`. So:

- the capacity type is `ON_DEMAND` (a real `Delete` deprovisions and stops
  billing the box), **not** `BARE_METAL`,
- `price_per_hour` is the real hourly USD rate (not zero — you pay for every hour
  the server is deployed), and
- declaring `BARE_METAL` would also suppress the shard's `Delete` (M73), so the
  provider would *keep* paying for a box it can no longer reclaim. See
  [Configuration → Why on_demand, not bare_metal](/providers/latitude/configuration/#why-on_demand-not-bare_metal).

A genuinely *owned* bare-metal substrate (where the hardware is already paid for
and `Delete` is `Unimplemented`) is the `bare-metal` profile, with
`price_per_hour = 0` and `interruption_probability = 0`. That is not what this
provider does: Latitude.sh is on-demand, billed by the hour, with a real Delete.

### If Latitude ever ships a spot tier

The contract is ready for it. If Latitude introduces a real preemptible market,
the correct change is to set `capacity_type = SPOT` and a **real, non-zero**
interruption forecast for those machines (observed where possible, forecast for
Speculative slots), and to claim the `spot` profile. Never leave a spot machine
at `0`.
