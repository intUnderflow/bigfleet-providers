---
title: Certification
description: How the GCP (GCE) provider is certified — make certify-gcp runs the upstream conformance baseline plus the extension suite against a live endpoint.
sidebar:
  order: 8
  label: Certification
---

This provider is **certified**: it passes every behavior in the BigFleet
conformance program — the same bar every provider must clear — so you can trust
it to create, configure, drain, and delete machines correctly under load,
failure, and restart. You do not need to run anything here to use it in
production; this page exists if you want to reproduce that verdict yourself.

"Certified" here means exactly what it means in the
[conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md):
the provider passes **both** the upstream authoritative baseline **and** this
repo's extension suite, with no failures and no skipped-as-failed behaviors.

## One command

```sh
make certify-gcp
```

That target (`hack/run-certify.sh gcp`) is fully **credential-free**. It:

1. Resolves the bigfleet checkout that owns the authoritative contract — reusing
   `$BIGFLEET_SRC` if set, otherwise cloning the exact version pinned in the
   provider's `go.mod` into `.cache/bigfleet-src`.
2. Builds `./bin/gcp` and boots it with `--provider=certify --seed-count=256`.
   With no `--region`, the provider's `--gcp-backend` resolves to `fake`, so no
   GCP project is touched — the extension suite consumes a fresh machine per
   behavior, hence the generous seed.
3. Runs the **upstream baseline** (`test/conformance/` in the bigfleet repo),
   then the **extension suite** (`conformance/suite`, build-tagged `certify`),
   both dialing that one endpoint.
4. Prints `CERTIFIED: gcp passed the upstream baseline + the extension suite` —
   or fails non-zero on the first failing behavior, tearing the provider down.

Override the port with `PORT=...`.

## Profiles the GCP provider claims

The harness certifies a provider against the **profiles** it advertises;
behaviors outside a claimed profile skip-as-pass. The harness probes the
provider's `Capabilities` over the wire and skips inapplicable behaviors with a
reason (never failing them).

- **core** — every provider (lifecycle, errors, fencing, concurrency, metadata,
  field-shape, list, property).
- **cloud** — implements `Delete` (Idle → Speculative). The GCP provider does
  (`Delete` = `Instances.Delete`), so it claims **cloud**.
- **spot** — exposes SPOT capacity with a real `interruption_probability > 0`.
  The GCP provider offers Spot VMs, so it **claims `spot`**, and its default
  offerings include Spot slots so the invariant actively fires. See
  [Pricing & interruption](/providers/gcp/pricing-and-interruption/).
- **fault / durable / scale** — failure→`FAILED`, restart recovery, and scale
  lanes. These come from `providerkit` and pass by construction for any kit-based
  provider; run them through the report runner.

`make certify-gcp` runs the credential-free core gate (baseline + the black-box
extension). The **complete** certification — every applicable lane — runs through
the `bfconformance` runner and emits a JUnit + JSON report with a verdict:

```sh
make report-gcp PROFILE=core,cloud,spot,fault,durable,scale
# -> VERDICT: CERTIFIED, writes conformance-report/gcp/{report.json,junit.xml}
```

The certified run covers all **92 behaviors** across the 11 areas (lifecycle,
transition matrix/errors, fencing, concurrency & idempotency, metadata,
field-shape & cost, list/revision/pagination, timeouts & failure,
durability/restart, scale & soak, property/fuzz).

## Certifying a real endpoint

`make certify-gcp` certifies the fake backend in CI. To certify the provider
*talking to real GCE*, run it yourself against your project and point the
extension suite at it:

```sh
# 1. Boot the provider against real GCP (see Install & deploy / Configuration).
./bin/gcp \
  --addr 127.0.0.1:9099 \
  --project my-gcp-project \
  --region us-central1 \
  --image projects/debian-cloud/global/images/family/debian-12 \
  --offerings ./offerings.json

# 2. In another shell, run the extension suite against that endpoint.
go -C conformance test -tags=certify -count=1 ./suite/... -target=127.0.0.1:9099
```

A real run exercises the full lifecycle — `Instances.Insert` → wait-for-RUNNING →
in-band SSH Configure/Drain → `Instances.Delete` — so the endpoint needs a
provider service account with `compute.instanceAdmin.v1` (see
[Credentials](/providers/gcp/credentials/)), an `--ssh-key`, and an image whose
bootstrap hook joins the cluster. It will create and destroy real instances; certify in a
throwaway project and tear the instances down.

## Why this provider does not opt out of the CI gate

A provider that cannot stand up without cloud credentials may add an empty
`providers/gcp/.ci-no-conformance` marker to skip the CI `certify` job. The GCP
provider **does not** carry this marker, and must not: its `fake` backend stands
up with no credentials, so `make certify-gcp` runs and stays green on every PR.
Adding the opt-out here would forfeit that credential-free certification gate.

## See also

- [Conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md) — the behavior registry, profiles, and how to add a behavior.
- [Pricing & interruption](/providers/gcp/pricing-and-interruption/) — why every SPOT machine declares a real, non-zero interruption probability.
- [Credentials & auth](/providers/gcp/credentials/) — the service account a real-endpoint certification run needs.
