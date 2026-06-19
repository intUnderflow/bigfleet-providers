---
title: Certification
description: How the AWS EC2 provider is certified — make certify-aws runs the upstream conformance baseline plus the extension suite against a live endpoint.
sidebar:
  order: 8
  label: Certification
---

The AWS EC2 provider is certified by *passing the same certification suites every
provider must pass* — against one running endpoint, as a black-box gRPC client.
This page is how you reproduce that verdict locally and in CI.

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

**Extension suite** — deepens coverage well beyond the baseline (it adds cases
and asserts stronger invariants under distinct behavior ids, rather than forking
the upstream tests):

| ID | Checks |
|----|--------|
| **C1** | Full and repeated lifecycle round-trips leave no residue — `cluster`, `shard_metadata`, and `last_error` all clear at a clean Idle; host/cluster invariants per state. |
| **C2** | The out-of-position matrix: every illegal (RPC × source-state) is rejected with a **non-`FAILED_PRECONDITION`** code and no partial transition; RPCs at their target state are idempotent no-ops; unknown id → `NotFound`; empty id → `InvalidArgument`. |
| **C3** | Fencing depth: the fence runs *before* not-found and before idempotency; per-`shard_id` isolation; exhaustive `(epoch, sequence)` ordering incl. new-epoch reset; reads never fence. |
| **C5** | `shard_metadata` stress: verbatim echo of large / unicode / control-byte / empty-value / many-key maps, stable across Get/List, cleared on Drain, replaced cleanly on re-Configure. |
| **C8** | Field shape & cost: `instance_type`/`zone`/`capacity_type` are top-level (never labels); `price_per_hour` ≥ 0 and finite; `interruption_probability` ∈ [0,1] and **> 0 for SPOT**. |
| **C9** | `List` filtering by every state and multi-state union; `max_results` bound; `since_revision` advances on mutation and a `since` delta includes the mutated machine. |

C8's SPOT rigor is where the AWS provider's design pays off: its spot machines
carry a real, non-zero forecast (see
[Pricing & interruption](/providers/aws/pricing-and-interruption/)), so the
`interruption_probability ∈ (0,1]` assertion holds by construction.

## Profiles the AWS provider claims

The harness certifies a provider against the **profiles** it advertises;
behaviors outside a claimed profile skip-as-pass:

- **core** — every provider (C1, C2, C3, C5, C8, C9).
- **cloud** — implements `Delete` (Idle → Speculative). The AWS provider does
  (`Delete` = `TerminateInstances`).
- **spot** — exposes SPOT capacity, so the SPOT interruption rigor applies. The
  AWS provider does.
- **scale** — supports `since_revision` deltas.

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
