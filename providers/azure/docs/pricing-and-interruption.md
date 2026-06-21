---
title: Pricing & interruption
description: How the Azure provider sources price_per_hour and SPOT interruption_probability, why spot is never zero, and how to keep the pinned tables accurate for your region.
sidebar:
  order: 4
  label: Pricing & interruption
---

Spot capacity is how you make your fleet cheaper — but only if BigFleet can tell
which machines are safe to put work on. This provider gives BigFleet two honest
numbers per machine so it ranks capacity by real cost: the hourly price, and how
likely a Spot machine is to be evicted. Crucially, this provider **never** claims
a Spot machine has zero eviction risk — so BigFleet can't be fooled into piling
critical work onto capacity Azure may reclaim. This page covers where both numbers
come from, how they stay fresh, and how to keep the pinned tables accurate for
your region.

Under the hood the engine combines the two into an effective cost — a Spot
machine reporting zero eviction risk would look both cheap *and* safe and get
workloads it should never run, which is exactly the trap this provider avoids:

```
effective_cost = price_per_hour + interruption_probability × penalty
```

Both values are read from in-memory caches on the `List`/seed hot path
(`speculativeSlots` and `Describe`) — neither ever blocks on an Azure API call
while the engine is waiting. The network work happens on background timers.

## `price_per_hour`

Price is sourced per capacity type (`pricing.go`):

| Capacity type | Source |
|---|---|
| `on_demand` | Pinned per-region pay-as-you-go table (`onDemandByRegion`). |
| `reserved` | Priced at pay-as-you-go unless you model a real reservation discount. |
| `spot` | Current Spot price from the **Azure Retail Prices API**, cached + refreshed on a timer. |

### On-demand: a pinned, region-keyed table

On-demand prices come from `onDemandByRegion`, a pinned table keyed by region then
VM size. On-demand prices are stable, so a pinned table is the right model: it is
deterministic and has no runtime pricing-API dependency on the `List` hot path. It
is **not** load-bearing for correctness — it feeds the engine's *relative* cost
ranking — but keep it roughly accurate.

`eastus` and `westeurope` ship with their own pinned snapshots. A region with
**no** pinned table of its own falls back to the `eastus` baseline and logs a
warning (the prices are then approximate). The empty region — the fake/dev
backend, which does not price-rank — falls back silently.

#### Regenerating a region's table

Regenerate a region's prices from the **public** Azure Retail Prices API (no
credentials) and paste the result into `onDemandByRegion`:

```sh
curl -s "https://prices.azure.com/api/retail/prices?currencyCode=USD&\$filter=\
armRegionName eq 'westus2' and armSkuName eq 'D4s_v5' and priceType eq 'Consumption'" \
  | jq '.Items[] | select(.productName | test("Windows|Spot") | not) | .unitPrice'
```

Filter to the Linux consumption meter (exclude Windows / Spot / low-priority). The
API is public but rate-limited, so this is an occasional offline maintenance step,
never a runtime dependency.

### Spot: refreshed from the Retail Prices API

Spot prices come from the Azure Retail Prices API (the Spot consumption meter for
each offered size in the configured region), cached in memory. The cache is warmed
once at startup (a bounded, best-effort 20s warm before the first `List`) and then
refreshed on a timer set by `--price-refresh` (default `1h`). Refresh is
best-effort: a failed fetch keeps the prior (or fallback) value and logs a
warning, rather than zeroing the price.

Before the first successful refresh, a SPOT read falls back to a conservative
`0.4 × pay-as-you-go` for that size, so a cold cache still ranks Spot below
on-demand without ever reading `0`.

The background refresher records its outcome on
`bigfleet_azure_price_refresh_total{outcome}`, and each underlying call shows up as
`bigfleet_azure_api_calls_total{op="RetailPrices"}`. See
[Observability](/providers/azure/observability/).

## SPOT `interruption_probability`

For SPOT machines the provider publishes a real eviction probability built from
two signals (`interruption.go`): a **forecast** that always applies, raised by an
**observed** notice when one arrives.

### Forecast: Azure Spot eviction-rate bands

Azure publishes a per-`(VM size, region)` **eviction-rate band** on the Spot
advisor / pricing surfaces, as a 30-day eviction fraction: `0-5%`, `5-10%`,
`10-15%`, `15-20%`, `20%+`. `evictionBand` is a pinned snapshot of those bands
(`0` … `4`). Each band's representative monthly fraction `m` is converted to an
**hourly** probability — the contract wants hourly, and a 30-day band is a monthly
figure:

```
p_hour = 1 - (1 - m)^(1/720)        # 720 hours ≈ 30 days
```

| Band | Eviction rate (30-day) | Representative `m` | Published hourly probability |
|---|---|---|---|
| 0 | `0-5%`   | `0.025` | ≈ 0.0000352 |
| 1 | `5-10%`  | `0.075` | ≈ 0.000108 |
| 2 | `10-15%` | `0.125` | ≈ 0.000185 |
| 3 | `15-20%` | `0.175` | ≈ 0.000267 |
| 4 | `20%+`   | `0.25`  | ≈ 0.000399 |

The hourly figures are small (an eviction over a month is a low per-hour rate),
but **strictly positive** — which is the whole point: combined with the price they
keep Spot ranked correctly relative to on-demand.

### Why SPOT is never `0`

A SPOT VM size that is **not** in `evictionBand` falls back to the middle
`10-15%` band — deliberately non-zero. Combined with the rule that there is no
`0` band, every SPOT machine carries a real, non-zero `interruption_probability`.
On-demand and reserved machines report `0` (the `forecast` function returns `0`
for any non-spot capacity), which is correct: they are not reclaimable.

### Observed: raised by a real eviction notice

Azure signals an impending Spot eviction via the **Scheduled Events** endpoint,
which lives on the **per-VM** IMDS endpoint
(`http://169.254.169.254/metadata/scheduledevents`, event type `Preempt`) — there
is no central queue the provider control plane can read (unlike AWS's
EventBridge→SQS). So the observed signal has two halves:

1. A small **node-side agent** — the reference
   [`deploy/agent/scheduled-events-agent.sh`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/azure/deploy/agent/scheduled-events-agent.sh),
   installed via `--base-user-data` — polls the VM's Scheduled Events endpoint,
   reads its own `bigfleet-machine-id` IMDS tag, and `POST`s any `Preempt` event
   to the provider.
2. The provider's **eviction ingest endpoint**, `POST /internal/eviction` on the
   metrics port, authenticated by a bearer token (`BIGFLEET_EVICTION_TOKEN` /
   `--eviction-token`). It is fail-closed — registered only when a token is set —
   so configure one and restrict the metrics port with a NetworkPolicy. On a
   `Preempt` it raises that
   machine's observed probability to `0.99`, increments
   `bigfleet_azure_spot_evictions_total`, and kicks a reconcile so the raised
   value lands in inventory promptly (the periodic `--reconcile-interval` loop
   also propagates it).

`probability` publishes the observed value whenever it exceeds the forecast. The
observed value is held per machine id, clamped to `[0, 1]`, and cleared only once
a `Delete` actuates — so a machine about to be evicted keeps its raised
probability until it is gone. Independently, the reconcile loop notices a VM that
has been evicted-and-deleted out from under the provider (Spot
`evictionPolicy=Delete`) and returns its slot to Speculative, so `Get`/`List`
reflect reality.

## What is region-shaped, and what to verify

| Fact | Source | Region handling |
|---|---|---|
| `allocatable` (vCPU/mem) | Resource SKUs API | **Authoritative** — resolved live for any region; the pinned table is only an offline fallback. |
| Spot `price_per_hour` | Retail Prices API | **Authoritative** — fetched live per region, correct everywhere. |
| On-demand `price_per_hour` | `onDemandByRegion` table | Tabulated for `eastus`/`westeurope`; other regions fall back to the `eastus` baseline (approximate) until you regenerate. |
| Spot `interruption_probability` bands | `evictionBand` (`interruption.go`) | **Pinned approximations** for every region. |

When the `azure` backend serves a region with no pinned price table, it logs a
startup warning. Both the price table and the eviction bands drift over time;
refresh them periodically. A size present in your offerings but absent from
`onDemandByRegion` prices at the zero value of the map; absent from `evictionBand`
it falls back to the non-zero middle band — so keep both tables in sync with your
offerings.
