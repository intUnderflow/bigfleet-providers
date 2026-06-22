---
title: Certification
description: How the AWS EC2 provider is certified — make certify-aws runs the upstream conformance baseline plus the extension suite against a live endpoint.
sidebar:
  order: 8
  label: Certification
---

This provider is **certified**: it passes every behavior in the BigFleet
conformance program — the same bar every provider must clear — so you can trust
it to launch, configure, drain, and delete machines correctly under load,
failure, and restart. You do not need to run anything here to use it in
production; this page exists if you want to reproduce that verdict yourself,
locally or in your own CI.

"Certified" here means exactly what it means in the
[conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md):
the provider passes **both** the upstream authoritative baseline **and** this
repo's extension suite, with no failures and no skipped-as-failed behaviors.

## One command

```sh
make certify-aws
```

That target (`hack/run-certify.sh aws`) is fully **credential-free**. It:

1. Resolves the bigfleet checkout that owns the authoritative contract — reusing
   `$BIGFLEET_SRC` if set, otherwise cloning the exact version pinned in the
   provider's `go.mod` into `.cache/bigfleet-src`.
2. Builds `./bin/aws` and boots it on `127.0.0.1:9099` with `--provider=certify
   --seed-count=256`. With no `--region`, the provider's `--ec2-backend` resolves
   to `fake`, so no AWS account is touched — the extension suite consumes a fresh
   machine per behavior, hence the generous seed.
3. Runs the **upstream baseline** (`test/conformance/` in the bigfleet repo),
   then the **extension suite** (`conformance/suite`, build-tagged `certify`),
   both dialing that one endpoint.
4. Prints `CERTIFIED: aws passed the upstream baseline + the extension suite` —
   or fails non-zero on the first failing behavior, tearing the provider down.

Override the port with `PORT=...`, e.g. `make certify-aws PORT=9123`.

## What the two suites check

The certification harness is a pure black-box gRPC client: it dials `--addr` and
uses only the wire RPCs of `bigfleet.v1alpha1.CapacityProvider` — no
`providerkit` imports, no process introspection. It detects what the provider
supports through a `Capabilities` probe and **skips inapplicable behaviors with a
reason** (never failing them).

**Upstream baseline** — the immovable, authoritative contract maintained in the
bigfleet repo. We run it verbatim and never modify it; it is the floor every
certified provider clears.

**Extension suite** — the BigFleet conformance program: a frozen registry of
**93 behaviors across 11 areas** that deepens the baseline (stronger invariants
under distinct, append-only ids, never forking the upstream tests):

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

The full, frozen registry of all 93 behaviors — every assertion, profile, and id
— is the [conformance program](/conformance/). This provider clears every one.

The field-shape area is where the AWS provider's design pays off: its spot
machines carry a real, non-zero interruption forecast (see
[Pricing & interruption](/providers/aws/pricing-and-interruption/)), so the
SPOT-`interruption_probability` > 0 assertion holds by construction.

## Profiles the AWS provider claims

The harness certifies a provider against the **profiles** it advertises;
behaviors outside a claimed profile skip-as-pass:

- **core** — every provider (lifecycle, errors, fencing, concurrency, metadata,
  field-shape, list, property).
- **cloud** — implements `Delete` (Idle → Speculative). The AWS provider does
  (`Delete` = `TerminateInstances`).
- **spot** — exposes SPOT capacity, so the SPOT interruption rigor applies. The
  AWS provider does.
- **fault** — failure / timeout → `FAILED` handling (certified via a reference
  fault-injecting provider).
- **durable** — fence marks, idempotency, bindings, and inventory survive a kill
  + restart against a `--state` file.
- **scale** — large inventory, `since_revision` deltas, churn-soak, latency
  budgets.

`make certify-aws` runs the credential-free core gate (baseline + the black-box
extension). The **complete** certification — all 93 behaviors across every lane,
including the fault, durability, and scale lanes — runs through the
`bfconformance` runner and emits a JUnit + JSON report:

```sh
make report-aws PROFILE=core,cloud,spot,fault,durable,scale
```

## Certifying a real endpoint

`make certify-aws` certifies the fake backend in CI. To certify the provider
*talking to real EC2*, run the provider yourself against your account and point
the extension suite at it:

```sh
# 1. Boot the provider against real EC2 (see Install & deploy / Configuration).
./bin/aws \
  --addr 127.0.0.1:9099 \
  --region us-east-1 \
  --ami ami-0abcd... \
  --subnets us-east-1a=subnet-aaa,us-east-1b=subnet-bbb \
  --offerings ./offerings.json

# 2. In another shell, run the extension suite against that endpoint.
go -C conformance test -tags=certify -count=1 ./suite/... -target=127.0.0.1:9099
```

To also run the upstream baseline against the same endpoint, point
`go test` at your bigfleet checkout:

```sh
go test -tags=conformance -count=1 -run '^TestConformance_' \
  ./test/conformance/... -target=127.0.0.1:9099   # run from your bigfleet repo
```

A real run exercises the full lifecycle — `RunInstances` → wait-for-running →
SSM `Configure`/`Drain` → `TerminateInstances` — so the endpoint needs the IAM
permissions documented on the [IAM](/providers/aws/iam/) page and a node
instance profile with `AmazonSSMManagedInstanceCore`. It will create and destroy
real instances; certify in a throwaway account or a dedicated test cluster.

## The `.ci-no-conformance` opt-out

CI runs `make certify-<provider>` per changed provider, credential-free. A
provider that **cannot stand up without cloud credentials** opts out by adding an
empty marker file:

```sh
touch providers/<name>/.ci-no-conformance
```

When present, the CI `certify` job is **skipped (never failed)** for that
provider, and you are expected to certify it manually against a real endpoint.

The AWS provider **does not** carry this marker, and must not: its `fake` backend
stands up with no credentials, so `make certify-aws` runs and stays green on
every PR. Adding the opt-out here would forfeit that credential-free
certification gate.

## See also

- [Conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md) — the behavior registry, profiles, and how to add a behavior.
- [Pricing & interruption](/providers/aws/pricing-and-interruption/) — why SPOT `interruption_probability` is always non-zero (C8).
- [IAM](/providers/aws/iam/) — the permissions a real-endpoint certification run needs.
- [Install & deploy](/providers/aws/install/) and [Configuration](/providers/aws/configuration/) — booting the provider against real EC2.
