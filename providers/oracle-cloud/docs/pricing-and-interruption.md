---
title: Pricing & interruption
description: How the OCI provider sets price_per_hour from a pinned table and why preemptible (spot) machines always carry a non-zero interruption_probability.
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

OCI exposes no clean per-shape hourly price via a simple Compute API call, so
prices come from a **pinned, versioned price table** (`prices.yaml`), embedded in
the binary and overridable with `--prices-file`. It is read off the hot path
(never a live pricing call on `List`).

- **Flexible shapes** (name ends `.Flex`) are priced per-OCPU-hour + per-GB-hour,
  keyed by shape family, times the launch OCPU/memory:
  `price = ocpus × ocpu_rate + memory_gb × ram_rate`.
- **Fixed shapes** (e.g. GPU VM shapes) use a whole-instance hourly rate.
- **Bare metal** (`BM.*`) reports `0` — it is fixed, already-paid-for capacity.
- **Preemptible (spot)** applies the table's `spot_discount` (default 0.5; OCI
  Preemptible Instances are ~50% off on-demand) to the on-demand price.

The table pins a `priced_at` date and a `source` URL. It feeds the engine's
*relative* cost ranking, so approximate rates are fine — but keep them roughly
accurate; refresh from Oracle's published Compute price list out-of-band.

## `interruption_probability`

This is hourly, in `[0, 1]`, **provider-declared only** (clusters can never
override it), and it is **correctness-critical**: a preemptible machine that
reports `0` looks free-and-safe and would be handed workloads it should never run.
**So a SPOT machine here never reports 0.**

- **On-demand / bare metal:** `0` (no provider-side interruption).
- **Preemptible (spot):** OCI Preemptible Instances are genuinely reclaimed when
  capacity is needed (OCI emits a preemption-action event ahead of stop/terminate
  via the Events service). The provider declares:
  - a **forecast** — a conservative per-shape hourly prior (a tunable table;
    scarcer / larger shapes carry a higher prior, default `0.10`), used for
    Speculative slots and instances with no observed preemption; and
  - an **observed** value — once a preemption signal is seen for a running
    instance, its probability is raised toward `1.0`. Wire `markPreemption` to an
    OCI Events/Notifications subscription for preemption-action events in
    production; the published value is always the current, real one.

Because every preemptible offering declares a non-zero forecast, the conformance
**SPOT `interruption_probability` > 0** invariant holds by construction.
