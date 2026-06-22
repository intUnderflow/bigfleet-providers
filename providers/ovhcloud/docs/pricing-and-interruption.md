---
title: Pricing & interruption
description: How the OVHcloud provider live-refreshes price_per_hour from OVH's public order catalog (EUR → USD), with a dated seed table as fallback, and why interruption_probability is a genuine zero on OVH Public Cloud.
sidebar:
  order: 4
  label: Pricing & interruption
---

BigFleet ranks capacity by **effective cost** = `price_per_hour +
interruption_probability × penalty`. So a provider has to report both honestly.
On OVH Public Cloud the story is unusually simple, and this page explains exactly
why.

## `price_per_hour` — live from the OVH order catalog, in USD

OVH publishes a fixed hourly on-demand price per flavor, in **EUR**. It turns out
there **is** a usable, credential-free price source: the public **order catalog**

```
GET https://api.ovh.com/1.0/order/catalog/public/cloud?ovhSubsidiary=<sub>
```

carries the Public Cloud instance hourly rate for each flavor as the addon with
plan code `<flavor>.consumption` (the pricing entry with `intervalUnit: "hour"`
and a `consumption` capacity, ex-VAT). The provider pulls those prices and
converts to **USD** with the `--eur-usd` rate:

```
price_per_hour (USD) = catalog_hourly_EUR[flavor] × --eur-usd
```

- **Refreshed off the hot path.** A background loop refreshes the prices into a
  mutex-guarded in-memory map every `--price-refresh` (default `45m`). `List`/`Get`
  only ever read that map — the catalog is **never** fetched on the hot path.
- **Seeded + fallback, never silently drifting.** A **dated EUR seed table** in
  `pricing.go` warms the cache at startup and serves as the fallback if a refresh
  fails or the catalog omits a flavor. It is *not* the source of truth — the live
  refresh overlays it. Staleness is observable: the metric
  `bigfleet_ovh_price_last_success_timestamp_seconds` records the last successful
  refresh (alert on its age), and the provider logs a loud `source=manual` warning
  whenever it falls back to the seed.
- **EUR subsidiaries only.** `--price-subsidiary` (default `FR`) selects the
  catalog subsidiary; it must be a **EUR** one (FR, DE, IE, ES, IT, NL, PT, FI, …)
  because `--eur-usd` assumes EUR. A non-EUR catalog (GBP/PLN, or the separate
  US/CA API roots) is **rejected** rather than mis-converted — price those flavors
  with `--flavor-price` instead.
- **The rate is configurable** (`--eur-usd`, default `1.08`). The cost field is a
  *relative* ranking signal, so an approximate rate is fine, but pin a current one
  — a stale rate skews effective-cost across the whole fleet. Set it per deployment.
- **Overrides win.** An operator can pin an explicit per-flavor USD with
  **`--flavor-price flavor=USD/hour`** (a negotiated rate, or a flavor the catalog
  doesn't carry); it takes precedence over both the live and seed prices.

The **fake backend** (dev / credential-free conformance) makes **no live call** at
all: it serves the deterministic seed table, so conformance is offline and
reproducible.

A flavor that has neither a seed-table entry nor a `--flavor-price` override would
publish `price_per_hour = 0` — the global minimum of the cost-ranking signal, so
it would always win. The provider therefore **fails closed**: it refuses to start
if any offering references such a flavor (checked against the guaranteed sources —
seed + override — not the live catalog, which may be momentarily unreachable at
startup). Add the flavor to the seed table in `pricing.go`, or pass
`--flavor-price <flavor>=<USD/hour>`.

## `interruption_probability` — a genuine zero

`interruption_probability` is the hourly chance the **provider** reclaims the
machine out from under the workload. It is **provider-declared only** — no
cluster can override it.

**OVH Public Cloud is on-demand only. There is no spot/preemptible market.** OVH
does not reclaim a running on-demand instance to satisfy other demand. So the
correct, real, provider-declared value is exactly **`0.0`** for every machine.

This is the important distinction the conformance program checks: a zero here is
**not** a skipped or forgotten field — it is the *true* value for this substrate.
That is different from a spot machine declared at 0, which would be a bug
(`effective_cost` would understate the real risk and the machine would win
high-penalty workloads it should never get). Because OVH Public Cloud has no spot
tier, the provider:

- declares `capacity_type = ON_DEMAND` for every machine,
- sets `interruption_probability = 0.0`,
- and **does not claim the `spot` conformance profile** — so the
  SPOT-`interruption_probability > 0` behaviors skip-as-pass rather than apply.

The provider also **rejects** a `spot` `capacity_type` in an offering at startup,
rather than silently mis-declaring a zero interruption probability for capacity
that doesn't exist.

### If OVH ever ships a spot tier

The contract is ready for it. If OVH introduces a real preemptible market, the
correct change is to set `capacity_type = SPOT` and a **real, non-zero**
interruption forecast for those machines (observed where possible, forecast for
Speculative slots), and to claim the `spot` profile. Never leave a spot machine at
`0`.

## Bare metal (Dedicated Servers) — out of scope here

This provider serves **OVH Public Cloud** (OpenStack instances). OVH's bare-metal
**Dedicated Servers** are a separate substrate (the OVH API, not OpenStack), where
the cost story flips: `capacity_type = BARE_METAL`, `price_per_hour = 0` (the
hardware is already owned/paid for), and `interruption_probability = 0` (owned
hardware is not reclaimed). That substrate has its own profile (`bare-metal`,
where `Delete` is `Unimplemented`); it is not what the Public Cloud provider on
this page does.
