---
title: Pricing & interruption
description: How the Hetzner provider sources price_per_hour (EUR→USD) and why interruption_probability is a genuine zero on Hetzner Cloud.
sidebar:
  order: 4
  label: Pricing & interruption
---

BigFleet ranks capacity by **effective cost** = `price_per_hour +
interruption_probability × penalty`. So a provider has to report both honestly.
On Hetzner Cloud the story is unusually simple, and this page explains exactly
why.

## `price_per_hour` — published Hetzner rates, in USD

Hetzner publishes a fixed hourly on-demand price per server type per location, in
**EUR**. The provider sources that rate from the Hetzner ServerType API
(`ServerType.Pricings`), picks the entry for the machine's location, takes the
**hourly** (not monthly) gross figure, and converts it to **USD** with the
`--eur-usd` rate:

```
price_per_hour (USD) = hetzner_hourly_gross_EUR × --eur-usd
```

- **The rate is configurable** (`--eur-usd`, default `1.08`). The cost field is a
  *relative* ranking signal, so an approximate rate is fine, but pin a current
  one and refresh it periodically — a stale rate skews effective-cost across the
  whole fleet. Set it per deployment.
- **Prices are cached and refreshed off the hot path.** At startup, and every
  `--price-refresh` (default 30m), the provider refreshes the price for each
  offered `(server_type, location)` pair. `List`/`Get` read the cache and never
  block on the pricing API.
- **A pinned EUR table is the fallback.** Common cx/cpx/cax/ccx types have pinned
  EU-baseline hourly prices, so the fake backend, credential-free conformance,
  and a pricing-API outage all still produce a sensible `price_per_hour`. Live
  Hetzner data overlays it (and picks up the small US-location premium for `ash`
  / `hil`).

## `interruption_probability` — a genuine zero

`interruption_probability` is the hourly chance the **provider** reclaims the
machine out from under the workload. It is **provider-declared only** — no
cluster can override it.

**Hetzner Cloud is on-demand only. There is no spot/preemptible market.** Hetzner
does not reclaim a running on-demand server to satisfy other demand. So the
correct, real, provider-declared value is exactly **`0.0`** for every machine.

This is the important distinction the conformance program checks: a zero here is
**not** a skipped or forgotten field — it is the *true* value for this substrate.
That is different from a spot machine declared at 0, which would be a bug
(`effective_cost` would understate the real risk and the machine would win
high-penalty workloads it should never get). Because Hetzner Cloud has no spot
tier, the provider:

- declares `capacity_type = ON_DEMAND` for every machine,
- sets `interruption_probability = 0.0`,
- and **does not claim the `spot` conformance profile** — so the
  SPOT-`interruption_probability > 0` behaviors skip-as-pass rather than apply.

The provider also **rejects** a `spot` `capacity_type` in an offering at startup,
rather than silently mis-declaring a zero interruption probability for capacity
that doesn't exist.

### If Hetzner ever ships a spot tier

The contract is ready for it. If Hetzner introduces a real preemptible market,
the correct change is to set `capacity_type = SPOT` and a **real, non-zero**
interruption forecast for those machines (observed where possible, forecast for
Speculative slots), and to claim the `spot` profile. Never leave a spot machine
at `0`.

## Bare metal (Robot) — out of scope here

This provider serves **Hetzner Cloud**. If you build the Hetzner dedicated /
Robot substrate instead, the cost story flips: `capacity_type = BARE_METAL`,
`price_per_hour = 0` (the hardware is already owned/paid for), and
`interruption_probability = 0` (owned hardware is not reclaimed). That is a
separate substrate with its own profile (`bare-metal`, where `Delete` is
`Unimplemented`); it is not what the Cloud provider on this page does.
