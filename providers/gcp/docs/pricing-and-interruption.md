---
title: Pricing & interruption
description: How the GCP provider sources price_per_hour (live-refreshed from the Cloud Billing Catalog, on-demand + Spot) and declares a real, non-zero interruption_probability for Spot.
sidebar:
  order: 4
  label: Pricing & interruption
---

BigFleet ranks capacity by **effective cost** = `price_per_hour +
interruption_probability × penalty`. So a provider has to report both honestly.
GCE has two capacity tiers that matter here — standard on-demand and **Spot** —
and this page explains exactly how each field is sourced.

## `price_per_hour` — live-refreshed on-demand rates

The provider reads on-demand prices **live** from the **Cloud Billing Catalog
API** and keeps them in an in-memory table. A background loop (every
`--price-refresh`, default 45m) pulls the current rates **off the `List` hot
path** into a mutex-guarded map; `List`/`Get` only ever read that map and never
call the pricing API. The live refresh is the source of truth.

- **On-demand:** composed from the GCE on-demand **core** and **memory** SKUs for
  the machine's family in its region (Compute Engine service id
  `6F81-5844-456A`): `price = vCPU × core-rate + memory_GiB × ram-rate`. Reached
  via Application Default Credentials, or an API key (`--pricing-api-key`).
- **Spot:** GCE Spot prices are dynamic and deeply discounted (~60–91% off). The
  provider models Spot as a fixed fraction (`0.4`) of the (live or fallback)
  on-demand rate — a conservative (high) estimate so the cost engine never
  under-prices Spot. The result is always **non-zero**.
- **Reserved:** priced at on-demand unless you model a real committed-use
  discount (a reservation is a billing construct; the instance is a regular VM).

### The pinned table is the seed + fallback only

A **pinned, region-keyed table** (`us-central1` is the authoritative baseline)
**seeds** `price_per_hour` before the first refresh lands and **backstops** a
refresh failure — so a billing-API outage, or a type the catalogue mapping
cannot resolve, never zeroes a price; it simply keeps the last-known / pinned
value. It is *not* the runtime source of truth. The cost field is a *relative*
ranking signal, so the seed only has to be roughly right for the cold-start
window.

`price_per_hour` is never `0` for a real VM: the provider **fails closed** at
startup, rejecting any offering whose machine type has no seed price (it would
otherwise have no safe value to publish before the first refresh). `0` is
reserved for bare metal, which this provider never creates.

### Staleness

Each refresh records its outcome (`bigfleet_gcp_price_refresh_total{outcome}`)
and stamps the last fully-successful refresh
(`bigfleet_gcp_price_last_refresh_timestamp_seconds`), and logs the age of the
last success — so an operator can alert on a price table that has stopped
refreshing while the provider keeps serving the (now stale) fallback.

## `interruption_probability` — a real, non-zero value for Spot

`interruption_probability` is the hourly chance the **provider** (here, GCE)
reclaims the machine out from under the workload. It is **provider-declared
only** — no cluster can override it.

GCE has two cases:

- **On-demand / reserved:** GCE does not preempt a running standard VM to satisfy
  other demand, so the correct value is exactly **`0.0`**.
- **Spot:** GCE Spot VMs **can be preempted at any time**, so a Spot machine must
  declare a **real, non-zero** probability. A Spot machine reported at `0` would
  be a correctness bug: `effective_cost` would understate the real risk and the
  machine would win high-penalty workloads it should never get.

GCE exposes no clean per-instance preemption-probability API, so the value is
declared from two signals (per the author guide):

- **Forecast (Speculative slots + cold start):** a pinned, per-machine-family
  hourly preemption rate, seeded from GCE Spot guidance. Broad-supply families
  (`e2`, `n2`) carry a low rate; tighter-supply ones (`c2`, `m1`) carry more;
  accelerator families (`a2`, `a3`, `g2`) carry the most. An unknown family falls
  back to a non-zero default (`0.05`) — **never zero for Spot**.
- **Observed (once real):** the reconcile loop watches for preemptions. Because
  the provider only ever *deletes* instances (never stops them), a Spot VM seen in
  `TERMINATED` status was stopped by GCE — a preemption. On observing one, that
  slot's interruption probability is raised **toward 1.0** (`0.9`, well above any
  forecast bucket): a completed preemption is a near-certain signal, so a slot
  with proven preemption history is treated as far riskier on its next
  Speculative/Idle description. The observed value always wins over the forecast,
  and each preemption is counted in the `bigfleet_gcp_spot_preemptions_total`
  metric.

Both are clamped to `[0, 1]`. The kit additionally **rejects at startup** any
Spot seed whose `interruption_probability` is `0`, so a mis-declared Spot
offering can never come up.

This is the distinction the conformance program's **spot** profile checks: a
non-zero value for every SPOT machine. The GCP provider claims the `spot`
profile and its default offerings include Spot slots, so the invariant actively
fires (it is not vacuously skipped).

## Why live, but off the hot path

A live Cloud Billing Catalog lookup is the most accurate source, but calling it
on `List` would put a network dependency on the read path. The provider gets
both: a background loop refreshes the in-memory table on a cadence, and
`List`/`Get` read only the cached map — never the pricing API. The pinned table
stays in the tree as the startup seed and the outage fallback, so the read path
is always non-blocking and a pricing-API failure degrades to a stale-but-sane
price rather than a zero. The fake backend uses a deterministic, credential-free
price source, so certification (`make certify-gcp`) never makes a live call and
stays reproducible.
