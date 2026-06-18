---
title: BigFleet Providers
description: Out-of-tree capacity providers for BigFleet, and the shared library every provider is built on.
template: splash
hero:
  tagline: Out-of-tree capacity providers for BigFleet — one shared library, one conformance harness, one place to read.
  actions:
    - text: Provider author guide
      link: https://bigfleet.lucy.sh/provider-author-guide/
      icon: right-arrow
      variant: primary
      attrs:
        target: _blank
        rel: noopener
    - text: GitHub
      link: https://github.com/intUnderflow/bigfleet-providers
      icon: external
      variant: secondary
    - text: BigFleet
      link: https://bigfleet.lucy.sh
      icon: external
      variant: minimal
---

:::note
This site is an early stub. The repository and its shared library are live; the
provider list and per-provider docs will grow here as real providers land.
:::

## What this is

[BigFleet](https://bigfleet.lucy.sh) is a fleet-level infrastructure autoscaler. It decides which machines should serve which Kubernetes clusters and provisions, reclaims, and rebalances them through pluggable **`CapacityProvider`** backends. A *provider* is the component that actually creates, configures, drains, and deletes machines on a specific substrate — AWS, GCP, libvirt, bare metal, and so on.

**BigFleet ships zero real providers in its own repo, on purpose.** Kubernetes spent years undoing in-tree CCM/CSI providers; BigFleet does not repeat that. Every real provider lives here, in `bigfleet-providers` — a repository kept deliberately separate from the main BigFleet repo.

## Why one repository

Rather than one repo per provider, every provider lives together in a mono-repo so they share:

- **one correctness-critical library** — `providerkit` — that gets fencing, idempotency, async dispatch, `shard_metadata`, and the machine field shape right *once*, so each provider only writes substrate-specific logic;
- **one conformance harness** — pointed at the canonical acceptance suite in the BigFleet repo, so "BigFleet-compatible" means a passing run, not a promise;
- **one CI pipeline**, and one place to read.

Each provider is still an independently buildable binary that can ship on its own cadence.

## The contract

A provider is a gRPC **server** implementing `bigfleet.v1alpha1.CapacityProvider`; the BigFleet shard is the **client** that dials it. The contract is six RPCs — no `Watch`; reconciliation is `List` + `Get`:

| RPC | Lifecycle |
|---|---|
| `Create` | Speculative → Creating → Idle |
| `Configure` | Idle → Configuring → Configured |
| `Drain` | Configured → Draining → Idle |
| `Delete` | Idle → Deleting → Speculative |
| `Get` / `List` | read inventory |

Every provider owes the same cross-cutting obligations — async dispatch, idempotency, transition timeouts, fencing, the `shard_metadata` lifecycle, and the machine field shape. `providerkit` implements all of them so a provider author cannot get them subtly wrong. The authoritative wire contract and author guide live in the BigFleet repo and are consumed here from the Go module, never vendored — so the contract can't drift.

## Providers

| Provider | Capacity types | Status |
|---|---|---|
| `_template` | on-demand + spot (example) | copy-me skeleton — passes conformance against an in-memory backend |

Real providers (AWS, GCP, libvirt, …) are added by copying `_template`; this table grows as they land.

## Build a provider

1. Read the [provider author guide](https://bigfleet.lucy.sh/provider-author-guide/) — the spine.
2. `cp -r providers/_template providers/<name>` and implement the substrate `Backend`.
3. Declare `capacity_type`, `price_per_hour`, and `interruption_probability` honestly (a real probability for spot).
4. Get `make conformance-<name>` green.

The step-by-step recipe is in [CONTRIBUTING.md](https://github.com/intUnderflow/bigfleet-providers/blob/main/CONTRIBUTING.md).

## Where to go next

- Building a provider → the [provider author guide](https://bigfleet.lucy.sh/provider-author-guide/) and the conformance suite.
- The source → [GitHub](https://github.com/intUnderflow/bigfleet-providers).
- The bigger picture → [BigFleet](https://bigfleet.lucy.sh).
