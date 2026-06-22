---
title: Certification
description: How the OCI provider is certified — make certify-oracle-cloud runs the upstream conformance baseline plus the extension suite, credential-free, against the fake backend.
sidebar:
  order: 8
  label: Certification
---

This provider is **certified**: it passes every behavior in the BigFleet
conformance program — the same bar every provider must clear — which exercises
launch, configure, drain, and delete under load, failure, and restart. You don't
need to run anything here to use it; this page exists if you want to reproduce
that verdict yourself.

"Certified" means exactly what it means in the
[conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md):
the provider passes **both** the upstream authoritative baseline **and** this
repo's extension suite, with no failures and no skipped-as-failed behaviors.

## One command

```sh
make certify-oracle-cloud
```

That target (`hack/run-certify.sh oracle-cloud`) is fully **credential-free**. It:

1. Resolves the bigfleet checkout that owns the authoritative contract — reusing
   `$BIGFLEET_SRC` if set, otherwise cloning the exact version pinned in the
   provider's `go.mod`.
2. Builds `./bin/oracle-cloud` and boots it on `127.0.0.1:9099` with
   `--provider=certify --seed-count=256`. With no `--region`/`--compartment`, the
   `--oci-backend` resolves to `fake`, so no OCI tenancy is touched — the
   extension suite consumes a fresh machine per behavior, hence the generous seed.
3. Runs the **upstream baseline** (`test/conformance/` in the bigfleet repo), then
   the **extension suite** (`conformance/suite`), both dialing that one endpoint.
4. Prints `CERTIFIED: oracle-cloud passed the upstream baseline + the extension
   suite` — or fails non-zero on the first failing behavior.

Run the bare extension suite with `make conformance-oracle-cloud`.

## The full multi-lane report

The complete certification — all **93 behaviors** across every lane — runs through
the `bfconformance` runner and emits a JUnit + JSON report with a final verdict:

```sh
make report-oracle-cloud PROFILE=core,cloud,spot,fault,durable,scale
```

A representative run:

```
  baseline : 25 tests, 24 passed, 0 failed, 1 skipped
  extension: 93 tests, 93 passed, 0 failed, 0 skipped
  behaviors: 93 total — 93 passed, 0 failed, 0 skipped, 0 not-implemented
>> VERDICT: CERTIFIED
```

(The one baseline skip is `since_revision`: this provider returns full `List`
state each cycle — correct below ~10k machines/shard — so the incremental-cursor
behavior is skipped-as-pass, not failed.)

## Profiles the OCI provider claims

The harness certifies a provider against the **profiles** it advertises; behaviors
outside a claimed profile skip-as-pass:

- **core** — every provider (lifecycle, errors, fencing, concurrency, metadata,
  field-shape, list, property).
- **cloud** — implements `Delete` (Idle → Speculative). The OCI provider does
  (`Delete` = `TerminateInstance`).
- **spot** — exposes preemptible capacity, so the SPOT interruption rigor applies.
  The OCI provider does (OCI Preemptible Instances), so the
  SPOT-`interruption_probability` > 0 assertion holds **by construction** (see
  [Pricing & interruption](/providers/oracle-cloud/pricing-and-interruption/)).
- **fault** — failure / timeout → `FAILED` handling (via a reference fault provider).
- **durable** — fence marks, idempotency, bindings, and inventory survive a kill +
  restart against a `--state` file.
- **scale** — large inventory, churn-soak, latency budgets.

The OCI provider does **not** claim the `bare-metal` profile: capacity is set by
the declared `capacity_type` (a `BM.*` shape can be offered as `on_demand` —
hourly-billed, `Delete`-able — or as `bare_metal` — held, price 0), and `Delete`
is always implemented, so it never exposes the fixed free pool where `Delete` is
`Unimplemented` that the `bare-metal` profile certifies.

The fault, durable, and scale lanes come from `providerkit` and pass **by
construction** for any kit-based provider.

## Certifying a real endpoint

`make certify-oracle-cloud` certifies the fake backend in CI. To certify the
provider talking to **real OCI**, boot it against your tenancy and point the
extension suite at it:

```sh
./bin/oracle-cloud --addr 127.0.0.1:9099 \
  --region eu-frankfurt-1 \
  --compartment ocid1.compartment.oc1..bbbb \
  --subnet ocid1.subnet.oc1..dddd \
  --image ocid1.image.oc1..eeee \
  --offerings ./offerings.json

go -C conformance test -tags=certify -count=1 ./suite/... -target=127.0.0.1:9099
```

A real run launches and terminates real instances and runs Run Commands on them,
so the endpoint needs the permissions on the
[Credentials & auth](/providers/oracle-cloud/credentials/) page. Certify in a
throwaway compartment.

## No `.ci-no-conformance` opt-out

Because the `fake` backend stands up with no OCI credentials,
`make certify-oracle-cloud` runs and stays green on every PR. This provider does
**not** carry a `.ci-no-conformance` marker — adding one would forfeit the
credential-free certification gate.
