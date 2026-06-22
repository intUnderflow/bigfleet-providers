---
title: Pricing & interruption
description: How the OCI provider live-refreshes price_per_hour from the public OCI price list (with prices.yaml as seed/fallback) and why preemptible (spot) machines always carry a non-zero interruption_probability.
sidebar:
  order: 4
  label: Pricing & interruption
---

BigFleet ranks capacity with a locked cost formula:

```
effective_cost = price_per_hour + interruption_probability × penalty
```

So the OCI provider declares both honestly on every machine.

## `price_per_hour`

Prices are **live-refreshed** from the public OCI price list (the cost-estimator
API at `apexapps.oracle.com/.../cetools`, no credentials) into an in-memory table
on a timer (`--price-refresh`, default 45m). `List`/`Describe` read that table —
**never a live pricing call on the hot path**. The live price list is the source
of truth.

`prices.yaml` (embedded, overridable with `--prices-file`) is the **startup seed
and fallback only**: it primes prices before the first refresh, and stands in for
any shape a refresh cannot price. It is no longer a frozen snapshot that silently
drifts from the bill — the refresher keeps the table current, and the seed only
backstops a fetch failure.

How shapes map onto OCI's metered SKUs (part numbers):

- **Flexible shapes** (name ends `.Flex`) are priced per-OCPU-hour + per-GB-hour,
  from the family's OCPU and memory SKUs, times the launch OCPU/memory:
  `price = ocpus × ocpu_rate + memory_gb × ram_rate`.
- **Fixed GPU shapes** use the per-GPU-hour SKU × the shape's GPU count.
- **Fixed bare-metal Standard shapes** (offered as `on_demand`) use the family's
  per-OCPU/per-GB SKUs × the shape's fixed OCPU/memory.
- **`capacity_type: bare_metal`** reports `0` — it is held, already-paid-for
  capacity. (This is driven by the declared capacity type, not the shape prefix: a
  `BM.*` shape offered as `on_demand` is priced, not 0.)
- **Preemptible (spot)** applies the table's `spot_discount` (default 0.5; OCI
  Preemptible Instances are ~50% off on-demand) to the on-demand price.

### Fail closed on unpriced

A shape that bills hourly (`capacity_type` ≠ `bare_metal`) must carry a non-zero
price — emitting `price_per_hour = 0` would rank it as *free* and attract every
workload. So:

- **Startup is rejected** if any hourly-billed offering would price at 0 (no live
  SKU and no `prices.yaml` entry). Add a `prices.yaml` entry or a SKU mapping.
- **A live rate of 0** (e.g. the always-free Ampere A1 SKUs, which the price list
  reports at `0`) is **skipped**, so the shape keeps its non-zero seed value
  rather than being ranked free.
- A genuine `bare_metal` lane is exempt — its `0` is honest (already paid for).

### Staleness & observability

The refresher records its health so an operator can alert on a stale table:

- `bigfleet_oci_price_refresh_total{outcome}` — success / error counts.
- `bigfleet_oci_price_last_success_timestamp_seconds` — Unix time of the last
  successful refresh; **staleness = `time() - this`**.

A fetch error leaves the previous (live-or-seed) prices in place and logs a
warning with the current staleness. The cost field feeds the engine's *relative*
ranking, so an approximate rate is acceptable, but the live refresh keeps it
honest.

The `prices.yaml` seed pins a `priced_at` date and a `source` URL; refresh it
occasionally so the fallback doesn't drift, but the live list is what's served.

## `interruption_probability`

This is hourly, in `[0, 1]`, **provider-declared only** (clusters can never
override it), and it is **correctness-critical**: a preemptible machine that
reports `0` looks free-and-safe and would be handed workloads it should never run.
**So a SPOT machine here never reports 0.**

- **On-demand / bare metal:** `0` (no provider-side interruption).
- **Preemptible (spot):** OCI Preemptible Instances are genuinely reclaimed when
  capacity is needed (OCI emits a preemption-action event ahead of stop/terminate
  via the Events service). The provider publishes:
  - a **forecast** — a conservative per-shape hourly prior (a tunable table;
    scarcer / larger shapes carry a higher prior, default `0.10`). This is what is
    published today for every preemptible machine, and it already satisfies the
    SPOT > 0 invariant.
  - an **observed-escalation hook** (provided, not wired by default) —
    `markPreemption` raises a specific machine's probability toward `1.0` once a
    preemption signal is seen. The provider does **not** ship a live OCI
    Events/Notifications subscription, so this path is dormant until an operator
    wires `markPreemption` to OCI preemption-action events; until then the
    forecast prior is the published value.

Because every preemptible offering declares a non-zero forecast, the conformance
**SPOT `interruption_probability` > 0** invariant holds by construction.
