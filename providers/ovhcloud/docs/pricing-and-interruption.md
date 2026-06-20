---
title: Pricing & interruption
description: How the OVHcloud provider sources price_per_hour (a pinned EUR table → USD) and why interruption_probability is a genuine zero on OVH Public Cloud.
sidebar:
  order: 4
  label: Pricing & interruption
---

BigFleet ranks capacity by **effective cost** = `price_per_hour +
interruption_probability × penalty`. So a provider has to report both honestly.
On OVH Public Cloud the story is unusually simple, and this page explains exactly
why.

## `price_per_hour` — a pinned EUR table, in USD

OVH publishes a fixed hourly on-demand price per flavor, in **EUR**. Unlike a
hyperscaler, **OVH exposes no reliable real-time pricing API** for v1, so the
provider sources prices from a **pinned, version-controlled EUR table** in the
repo (`pricing.go`), keyed by flavor, and converts it to **USD** with the
`--eur-usd` rate:

```
price_per_hour (USD) = pinned_hourly_EUR[flavor] × --eur-usd
```

- **Deterministic, no hot-path dependency.** The table is read in memory on
  `List`/`Get`; there is no pricing call to block on. That is the right model for
  a substrate with no price API.
- **The rate is configurable** (`--eur-usd`, default `1.08`). The cost field is a
  *relative* ranking signal, so an approximate rate is fine, but pin a current one
  and refresh it periodically — a stale rate skews effective-cost across the whole
  fleet. Set it per deployment.
- **Refresh the table manually.** The pinned table is not load-bearing for
  correctness (it feeds the engine's relative cost ranking), but keep it roughly
  accurate. When OVH changes its catalogue, regenerate the table by hand from the
  [public OVH Public Cloud price page](https://www.ovhcloud.com/en/public-cloud/prices/)
  and bump the date in the table comment. An operator can also pin an explicit
  per-flavor USD via the in-process override (e.g. a negotiated rate) without
  touching the table.

A flavor that is not in the pinned table (and has no override) reports a
`price_per_hour` of `0`. List the flavors you actually offer in the table (or set
an override) so cost ranking is meaningful.

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
