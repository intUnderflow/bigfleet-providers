---
title: Pricing & interruption
description: How the GCP provider sources price_per_hour (pinned table, on-demand + Spot) and declares a real, non-zero interruption_probability for Spot.
sidebar:
  order: 4
  label: Pricing & interruption
---

BigFleet ranks capacity by **effective cost** = `price_per_hour +
interruption_probability × penalty`. So a provider has to report both honestly.
GCE has two capacity tiers that matter here — standard on-demand and **Spot** —
and this page explains exactly how each field is sourced.

## `price_per_hour` — a pinned USD table

The provider sources `price_per_hour` from a **pinned, region-keyed table** of
GCE on-demand rates (in USD). This is the v1 model the author guide recommends:
a version-controlled snapshot, sourced once from the Cloud Billing Catalog API /
the public pricing page and refreshed on a cadence, so there is no pricing-API
dependency on the `List` hot path and the numbers are deterministic for
certification.

- **On-demand:** the pinned per-`(machine_type, region)` rate. `us-central1` is
  the authoritative baseline; add a region by pinning its table.
- **Spot:** GCE Spot prices are dynamic and deeply discounted (~60–91% off). The
  provider models Spot as a fixed fraction (`0.4`) of the on-demand rate — a
  conservative (high) estimate so the cost engine never under-prices Spot. The
  result is always **non-zero**.
- **Reserved:** priced at on-demand unless you model a real committed-use
  discount (a reservation is a billing construct; the instance is a regular VM).

The cost field is a *relative* ranking signal, so an approximate table is
acceptable — but keep it roughly accurate, and regenerate it per region from the
Billing Catalog when prices move. `price_per_hour` is never `0` for a real VM
(`0` is reserved for bare metal, which this provider never creates).

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
- **Observed (once real):** the provider watches for preemptions on the reconcile
  timer. Because it only ever *deletes* instances (never stops them), a Spot VM
  seen in `TERMINATED` status was stopped by GCE — a preemption. On observing one,
  that slot's interruption probability is raised to a fixed elevated value (`0.5`,
  well above any forecast bucket) so a slot with proven preemption history is
  treated as materially riskier on its next Speculative/Idle description. The
  observed value always wins over the forecast, and each preemption is counted in
  the `bigfleet_gcp_spot_preemptions_total` metric.

Both are clamped to `[0, 1]`. The kit additionally **rejects at startup** any
Spot seed whose `interruption_probability` is `0`, so a mis-declared Spot
offering can never come up.

This is the distinction the conformance program's **spot** profile checks: a
non-zero value for every SPOT machine. The GCP provider claims the `spot`
profile and its default offerings include Spot slots, so the invariant actively
fires (it is not vacuously skipped).

## Why a pinned table, not a live lookup

A live Cloud Billing Catalog lookup is more accurate but adds a hot-path
dependency and more moving parts. For v1 the pinned table is deterministic,
dependency-free on the `List` path, and easy to reason about in certification.
If you need live pricing, regenerate the table on a cadence from the Billing
Catalog (`cloudbilling.googleapis.com`) offline and ship the updated snapshot —
the table is not load-bearing for correctness, only for relative cost ranking.
