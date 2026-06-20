---
title: Pricing & interruption
description: How the libvirt provider sources price_per_hour (synthetic, from the instance type) and why interruption_probability is a genuine zero on local KVM.
sidebar:
  order: 4
  label: Pricing & interruption
---

BigFleet ranks capacity by **effective cost** = `price_per_hour +
interruption_probability × penalty`. So a provider has to report both honestly.
On libvirt there is no cloud bill, and this page explains exactly how the provider
sources each value.

## `price_per_hour` — synthetic, from the instance type

There is no cloud invoice for a libvirt VM. The provider sources price one of two
ways:

- **Synthetic (default).** A deterministic rate derived from the instance type's
  hardware: `price = vcpu × --price-per-vcpu-hour + gib × --price-per-gib-hour`
  (defaults `0.0030`/vCPU-hr and `0.0008`/GiB-hr, roughly cloud-comparable). A
  larger flavor costs proportionally more, so cost-based scheduling demos are
  meaningful.
- **Explicit.** Pin a per-type price with `--prices kvm.small=0.01,kvm.large=0.04`.
  An explicit price always wins over the synthetic rate.

The value is a *relative* ranking signal for the shard's effective-cost formula,
not a real invoice, so an approximate synthetic price is fine. It is read from
static state on the `List`/`Get` hot path and never blocks.

### Bare-metal pools price at zero

If you declare `--capacity-type bare_metal` (a fixed free pool of owned hardware),
`price_per_hour` is **0** — the hardware is already paid for. That is the honest
value for capacity you hold forever; the synthetic rate applies only to
`on_demand` pools.

## `interruption_probability` — a genuine zero

`interruption_probability` is the hourly chance the **provider** reclaims the
machine out from under the workload. It is **provider-declared only** — no cluster
can override it.

**Local KVM VMs have no preemption market.** Nothing reclaims a running libvirt
domain to satisfy other demand. So the correct, real, provider-declared value is
exactly **`0.0`** for every machine — and the provider sets it explicitly rather
than leaving it unset.

This is the important distinction the conformance program checks: a zero here is
**not** a skipped or forgotten field — it is the *true* value for this substrate.
That is different from a spot machine declared at 0, which would be a bug
(`effective_cost` would understate the real risk). Because a single libvirt host
has no spot tier, the provider:

- declares `capacity_type = ON_DEMAND` (or `BARE_METAL`) for every machine,
- sets `interruption_probability = 0.0`,
- and **does not claim the `spot` conformance profile** — so the
  SPOT-`interruption_probability > 0` behaviors skip-as-pass rather than apply.

The provider also **rejects** a `spot` `capacity_type` in an offering at startup,
rather than silently mis-declaring a zero interruption probability for capacity
that doesn't exist.

### If you model a preemptible pool

If you build a libvirt deployment that genuinely churns VMs under contention (an
oversubscribed host that may evict), the contract is ready: set
`capacity_type = SPOT`, a **real, non-zero** interruption forecast for those
machines, and claim the `spot` profile. Never leave a spot machine at `0`. This
provider, as shipped, models stable local VMs, so `0` is correct.

## On-demand vs bare-metal — which to declare

| | `on_demand` (default) | `bare_metal` |
|---|---|---|
| `price_per_hour` | synthetic (or `--prices`) | `0` |
| `interruption_probability` | `0` | `0` |
| `Delete` | implemented (destroys the VM) | never received (M73) |
| Conformance profile | core + **cloud** | core + **bare-metal** |
| Use when | the pool scales in/out | the VMs are long-lived |

See [Configuration](/providers/libvirt/configuration/#capacity-type-and-delete)
for how the choice drives the lifecycle.
