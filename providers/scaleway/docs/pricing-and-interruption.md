---
title: Pricing & interruption
description: How the Scaleway provider sources price_per_hour (EUR→USD), why Elastic Metal is priced 0, and why interruption_probability is a genuine zero on Scaleway.
sidebar:
  order: 4
  label: Pricing & interruption
---

BigFleet ranks capacity by **effective cost** = `price_per_hour +
interruption_probability × penalty`. So a provider has to report both honestly. On
Scaleway the story is unusually simple, and this page explains exactly why.

## `price_per_hour` — published Scaleway rates, in USD

Scaleway publishes a fixed hourly on-demand price per commercial type, in **EUR**.
The provider sources that rate from a pinned EUR table (`onDemandEURHourly` in
`pricing.go`), optionally refreshed out-of-band from the Scaleway product
catalogue, and converts it to **USD** with the `--eur-usd` rate:

```
price_per_hour (USD) = scaleway_hourly_EUR × --eur-usd
```

- **A pinned EUR table is the source of truth on the hot path.** `pricing.go`
  carries `onDemandEURHourly`, a pinned snapshot of hourly on-demand prices in EUR
  for the common DEV1 / GP1 / PLAY2 / PRO2 / COPARM1 / GPU lines (server-only,
  excluding the flexible-IP surcharge). `List`/`Get` read the converted cache and
  **never block on the pricing API**. Scaleway prices a given type identically
  across its EU zones, so the table is zone-agnostic.
- **The FX rate is configurable** (`--eur-usd`, default `1.08`). The cost field is a
  *relative* ranking signal, so an approximate rate is fine, but pin a current one
  and refresh it periodically — a stale rate skews effective-cost across the whole
  fleet. Set it per deployment.
- **Live catalogue refresh is optional and off the hot path.** At startup, and
  every `--price-refresh` (default 30m), the provider can refresh the price for each
  offered `(commercial_type, zone)` pair from the public catalogue, overlaying any
  zone-specific deviation on the pinned table. A fetch failure keeps the prior (or
  pinned) value and logs a WARN — so a catalogue outage never breaks the List path.

## Elastic Metal — price 0 (owned hardware)

Elastic Metal capacity is **owned, already-paid-for hardware**, not a metered
hourly VM. So for the `BARE_METAL` substrate the provider reports
`price_per_hour = 0` for every machine. There is no per-hour rate to convert, and
the cost ranking treats it as the free-pool resource it is.

## `interruption_probability` — a genuine zero

`interruption_probability` is the hourly chance the **provider** reclaims the
machine out from under the workload. It is **provider-declared only** — no cluster
can override it.

**Scaleway has no spot/preemptible market.** Neither Instances nor Elastic Metal is
reclaimed by Scaleway to satisfy other demand. So the correct, real,
provider-declared value is exactly **`0.0`** for every machine.

This is the important distinction the conformance program checks: a zero here is
**not** a skipped or forgotten field — it is the *true* value for this substrate.
That is different from a spot machine declared at 0, which would be a bug
(`effective_cost` would understate the real risk and the machine would win
high-penalty workloads it should never get). Because Scaleway has no spot tier, the
provider:

- declares `capacity_type = ON_DEMAND` (Instances) or `BARE_METAL` (Elastic Metal)
  for every machine,
- sets `interruption_probability = 0.0`,
- and **does not claim the `spot` conformance profile** — so the
  SPOT-`interruption_probability > 0` behaviors skip-as-pass rather than apply.

The provider also **rejects** a `spot` `capacity_type` in an offering at startup,
rather than silently mis-declaring a zero interruption probability for capacity
that doesn't exist.

### If Scaleway ever ships a spot tier

The contract is ready for it. If Scaleway introduces a real preemptible market, the
correct change is to set `capacity_type = SPOT` and a **real, non-zero** interruption
forecast for those machines (observed where possible, forecast for Speculative
slots), and to claim the `spot` profile. Never leave a spot machine at `0`.
