---
title: Certification
description: How the Azure provider is certified — make certify-azure runs the upstream conformance baseline plus the extension suite against a live endpoint.
sidebar:
  order: 8
  label: Certification
---

This provider is **certified**: it passes every behavior in the BigFleet
conformance program — the same bar every provider must clear — so you can trust it
to create, configure, drain, and delete machines correctly under load, failure,
and restart. You do not need to run anything here to use it in production; this
page exists if you want to reproduce that verdict yourself, locally or in your own
CI.

"Certified" here means exactly what it means in the
[conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md):
the provider passes **both** the upstream authoritative baseline **and** this
repo's extension suite, with no failures and no skipped-as-failed behaviors.

## One command

```sh
make certify-azure
```

That target (`hack/run-certify.sh azure`) is fully **credential-free**. It:

1. Resolves the bigfleet checkout that owns the authoritative contract — reusing
   `$BIGFLEET_SRC` if set, otherwise cloning the exact version pinned in the
   provider's `go.mod` into `.cache/bigfleet-src`.
2. Builds `./bin/azure` and boots it on `127.0.0.1:9099` with `--provider=certify
   --seed-count=256`. With no `--location`, the provider's `--azure-backend`
   resolves to `fake`, so no Azure account is touched — the extension suite
   consumes a fresh machine per behavior, hence the generous seed.
3. Runs the **upstream baseline** (`test/conformance/` in the bigfleet repo), then
   the **extension suite** (`conformance/suite`, build-tagged `certify`), both
   dialing that one endpoint.
4. Prints `CERTIFIED: azure passed the upstream baseline + the extension suite` —
   or fails non-zero on the first failing behavior, tearing the provider down.

Override the port with `PORT=...`.

## What the two suites check

The certification harness is a pure black-box gRPC client: it dials `--addr` and
uses only the wire RPCs of `bigfleet.v1alpha1.CapacityProvider` — no `providerkit`
imports, no process introspection. It detects what the provider supports through a
`Capabilities` probe and **skips inapplicable behaviors with a reason** (never
failing them).

**Upstream baseline** — the immovable, authoritative contract maintained in the
bigfleet repo. We run it verbatim and never modify it.

**Extension suite** — the BigFleet conformance program: a frozen registry of
**92 behaviors across 11 areas** that deepens the baseline:

| Area | What it certifies |
|---|---|
| Lifecycle & state machine | residue-free round-trips; per-edge transitional, cluster, and host invariants |
| Transition matrix / errors | the out-of-position matrix, idempotent no-ops, code discipline, edge inputs |
| Fencing | fence-before-everything, per-`shard_id` isolation, exhaustive `(epoch, sequence)` ordering |
| Concurrency & idempotency | N parallel retries collapse to one `operation_id` and exactly one effect |
| Metadata | `shard_metadata` verbatim echo, clear-on-drain, clean replace |
| Field shape & cost | top-level `instance_type`/`zone`/`capacity_type`; price ≥ 0; `interruption_probability` ∈ [0,1], **> 0 for SPOT** |
| List, revision & pagination | filters, `max_results`, `since_revision` deltas, completeness at scale |
| Timeouts & failure | actuator error / timeout → `FAILED` + `last_error`; a late completion is discarded |
| Durability / restart | fence marks, idempotency, bindings, and inventory survive a kill + restart |
| Scale & soak | large inventory, churn-soak, latency budgets, parallel throughput |
| Property / fuzz | seeded-random lifecycle / fencing / metadata oracles |

The field-shape area is where the Azure provider's design pays off: its Spot
machines carry a real, non-zero eviction forecast (see
[Pricing & interruption](/providers/azure/pricing-and-interruption/)), so the
SPOT-`interruption_probability` > 0 assertion holds by construction.

## Profiles the Azure provider claims

The harness certifies a provider against the **profiles** it advertises; behaviors
outside a claimed profile skip-as-pass:

- **core** — every provider (lifecycle, errors, fencing, concurrency, metadata,
  field-shape, list, property).
- **cloud** — implements `Delete` (Idle → Speculative). The Azure provider does
  (`Delete` = `VirtualMachines.BeginDelete`).
- **spot** — exposes SPOT capacity, so the SPOT interruption rigor applies. The
  Azure provider offers Spot VMs.
- **fault** — failure / timeout → `FAILED` handling (from `providerkit`, by
  construction).
- **durable** — fence marks, idempotency, bindings, and inventory survive a kill +
  restart against a `--state` file.
- **scale** — large inventory, `since_revision` deltas, churn-soak, latency
  budgets.

The Azure provider does **not** claim **bare-metal**: standalone Azure VMs are
always billed, so it never serves a free pool where `Delete` is meaningless.

`make certify-azure` runs the credential-free core gate (baseline + the black-box
extension). The **complete** certification — all 92 behaviors across every lane —
runs through the `bfconformance` runner and emits a JUnit + JSON report:

```sh
make report-azure PROFILE=core,cloud,spot,fault,durable,scale
# -> VERDICT: CERTIFIED
```

## Certifying a real endpoint

`make certify-azure` certifies the fake backend in CI. To certify the provider
*talking to real Azure*, run it against your subscription and point the extension
suite at it:

```sh
# 1. Boot the provider against real Azure (see Install & deploy / Configuration).
./bin/azure \
  --addr 127.0.0.1:9099 \
  --location eastus \
  --subscription-id 00000000-0000-0000-0000-000000000000 \
  --resource-group bigfleet-eastus \
  --subnet-id /subscriptions/.../subnets/nodes \
  --offerings ./offerings.json

# 2. In another shell, run the extension suite against that endpoint.
go -C conformance test -tags=certify -count=1 ./suite/... -target=127.0.0.1:9099
```

To also run the upstream baseline against the same endpoint, point `go test` at
your bigfleet checkout:

```sh
go test -tags=conformance -count=1 -run '^TestConformance_' \
  ./test/conformance/... -target=127.0.0.1:9099   # run from your bigfleet repo
```

A real run exercises the full lifecycle — VM create → Configure/Drain extensions →
Delete — so the endpoint needs the role on the
[Credentials](/providers/azure/credentials/) page. It will create and destroy real
VMs; certify in a throwaway subscription or a dedicated test resource group.

## The `.ci-no-conformance` opt-out

CI runs `make certify-<provider>` per changed provider, credential-free. A provider
that **cannot stand up without cloud credentials** opts out with an empty marker
file. The Azure provider **does not** carry this marker, and must not: its `fake`
backend stands up with no credentials, so `make certify-azure` runs and stays green
on every PR. Adding the opt-out here would forfeit that credential-free
certification gate.

## See also

- [Conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md) — the behavior registry, profiles, and how to add a behavior.
- [Pricing & interruption](/providers/azure/pricing-and-interruption/) — why SPOT `interruption_probability` is always non-zero.
- [Credentials](/providers/azure/credentials/) — the permissions a real-endpoint certification run needs.
- [Install & deploy](/providers/azure/install/) and [Configuration](/providers/azure/configuration/) — booting the provider against real Azure.
