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
- **`capacity_type: bare_metal`** reports `0` — it is held, already-paid-for
  capacity. (This is driven by the declared capacity type, not the shape prefix: a
  `BM.*` shape offered as `on_demand` is priced from `fixed_hourly`, not 0.)
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
