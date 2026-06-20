---
title: Certification
description: How the Scaleway provider is certified — make certify-scaleway runs the upstream conformance baseline plus the extension suite against a live endpoint.
sidebar:
  order: 8
  label: Certification
---

This provider is **certified**: it passes every behavior in the BigFleet
conformance program — the same bar every provider must clear — so you can trust it
to create, configure, drain, and delete machines correctly under load, failure,
and restart. You do not need to run anything here to use it in production; this
page exists if you want to reproduce that verdict yourself.

"Certified" here means exactly what it means in the
[conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md):
the provider passes **both** the upstream authoritative baseline **and** this
repo's extension suite, with no failures and no skipped-as-failed behaviors. The
certified run: **baseline 25 tests (24 passed, 1 skipped), extension 92/92 passed,
all 11 behavior areas green — VERDICT: CERTIFIED.**

## One command

```sh
make certify-scaleway
```

That target (`hack/run-certify.sh scaleway`) is fully **credential-free**. It:

1. Resolves the bigfleet checkout that owns the authoritative contract — reusing
   `$BIGFLEET_SRC` if set, otherwise cloning the exact version pinned in the
   provider's `go.mod` into `.cache/bigfleet-src`.
2. Builds `./bin/scaleway` and boots it with `--provider=certify
   --seed-count=256`. With no credentials, the provider's `--scaleway-backend`
   resolves to `fake`, so no Scaleway project is touched — the extension suite
   consumes a fresh machine per behavior, hence the generous seed.
3. Runs the **upstream baseline** (`test/conformance/` in the bigfleet repo), then
   the **extension suite** (`conformance/suite`, build-tagged `certify`), both
   dialing that one endpoint.
4. Prints `CERTIFIED: scaleway passed the upstream baseline + the extension suite`
   — or fails non-zero on the first failing behavior, tearing the provider down.

Override the port with `PORT=...`.

## What the two suites check

The certification harness is a pure black-box gRPC client: it dials `--addr` and
uses only the wire RPCs of `bigfleet.v1alpha1.CapacityProvider` — no `providerkit`
imports, no process introspection. It detects what the provider supports through a
`Capabilities` probe and **skips inapplicable behaviors with a reason** (never
failing them).

**Upstream baseline** — the immovable, authoritative contract maintained in the
bigfleet repo (25 tests; one is skipped-as-pass for the unclaimed spot profile). We
run it verbatim and never modify it; it is the floor every certified provider
clears.

**Extension suite** — the BigFleet conformance program: a frozen registry of
**92 behaviors across 11 areas** that deepens the baseline (stronger invariants
under distinct, append-only ids). All 92 pass, all 11 areas green:

| Area | What it certifies |
|---|---|
| Lifecycle & state machine | residue-free round-trips; per-edge transitional, cluster, and host invariants |
| Transition matrix / errors | the out-of-position matrix, idempotent no-ops, code discipline, edge inputs |
| Fencing | fence-before-everything, per-`shard_id` isolation, exhaustive `(epoch, sequence)` ordering |
| Concurrency & idempotency | N parallel retries collapse to one `operation_id` and exactly one effect |
| Metadata | `shard_metadata` verbatim echo, clear-on-drain, clean replace |
| Field shape & cost | top-level `instance_type`/`zone`/`capacity_type`; price ≥ 0; `interruption_probability` ∈ [0,1] |
| List, revision & pagination | filters, `max_results`, `since_revision` deltas, completeness at scale |
| Timeouts & failure | actuator error / timeout → `FAILED` + `last_error`; a late completion is discarded |
| Durability / restart | fence marks, idempotency, bindings, and inventory survive a kill + restart |
| Scale & soak | large inventory, churn-soak, latency budgets, parallel throughput |
| Property / fuzz | seeded-random lifecycle / fencing / metadata oracles |

The full, frozen registry of all 92 behaviors is the
[conformance program](/conformance/). This provider clears every one.

## Profiles the Scaleway provider claims

The harness certifies a provider against the **profiles** it advertises; behaviors
outside a claimed profile skip-as-pass. The harness probes the provider's
`Capabilities` over the wire and skips inapplicable behaviors with a reason (never
failing them).

- **core** — every provider (lifecycle, errors, fencing, concurrency, metadata,
  field-shape, list, property).
- **cloud** — implements `Delete` (Idle → Speculative). The **Instances**
  substrate does (`Delete` = `DeleteServer`), so it claims **cloud**.
- **bare-metal** — `Delete` is `Unimplemented` (servers return to a free pool). The
  **Elastic Metal** substrate claims this profile instead of `cloud`.
- **spot** — exposes SPOT capacity. **Scaleway has no spot/preemptible market**, so
  the provider does **not** claim `spot`; the SPOT-`interruption_probability > 0`
  behaviors skip-as-pass. See
  [Pricing & interruption](/providers/scaleway/pricing-and-interruption/) for why a
  zero interruption probability is the *correct* value here.
- **fault / durable / scale** — failure→`FAILED`, restart recovery, and scale
  lanes. These come from `providerkit` and pass by construction for any kit-based
  provider; run them through the report runner.

`make certify-scaleway` runs the credential-free core gate (baseline + the
black-box extension). The **complete** certification — every applicable lane — runs
through the `bfconformance` runner and emits a JUnit + JSON report with a verdict:

```sh
make report-scaleway PROFILE=core,cloud,fault,durable,scale
# -> VERDICT: CERTIFIED, writes conformance-report/scaleway/{report.json,junit.xml}
```

(For an Elastic Metal deployment, swap `cloud` for `bare-metal` in the profile
list.)

## Certifying a real endpoint

`make certify-scaleway` certifies the fake backend in CI. To certify the provider
*talking to real Scaleway*, run it yourself against your project and point the
extension suite at it:

```sh
# 1. Boot the provider against real Scaleway (see Install & deploy / Configuration).
./bin/scaleway \
  --addr 127.0.0.1:9099 \
  --substrate instances \
  --image ubuntu_jammy \
  --bootstrap-addr :9443 \
  --bootstrap-endpoint https://127.0.0.1:9443 \
  --bootstrap-tls-cert bootstrap.pem --bootstrap-tls-key bootstrap-key.pem \
  --offerings ./offerings.json
# SCW_ACCESS_KEY / SCW_SECRET_KEY / SCW_DEFAULT_PROJECT_ID and
# BIGFLEET_BOOTSTRAP_SECRET from the environment

# 2. In another shell, run the extension suite against that endpoint.
go -C conformance test -tags=certify -count=1 ./suite/... -target=127.0.0.1:9099
```

A real run exercises the full lifecycle — `CreateServer` → wait-for-running →
agent `Configure`/`Drain` → `DeleteServer` — so the endpoint needs an API key with
the right permission sets (see [Credentials](/providers/scaleway/credentials/)) and
an image that installs the on-host agent. It will create and destroy real servers;
certify in a throwaway project and tear the servers down.

## Why this provider does not opt out of the CI gate

A provider that cannot stand up without cloud credentials may add an empty
`providers/scaleway/.ci-no-conformance` marker to skip the CI `certify` job. The
Scaleway provider **does not** carry this marker, and must not: its `fake` backend
stands up with no credentials, so `make certify-scaleway` runs and stays green on
every PR. Adding the opt-out here would forfeit that credential-free certification
gate.

## See also

- [Conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md) — the behavior registry, profiles, and how to add a behavior.
- [AWS certification](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/aws/docs/certification.md) — the same certification story for a spot-capable provider.
- [Pricing & interruption](/providers/scaleway/pricing-and-interruption/) — why `interruption_probability` is a genuine zero on Scaleway.
- [Credentials & auth](/providers/scaleway/credentials/) — the API key a real-endpoint certification run needs.
