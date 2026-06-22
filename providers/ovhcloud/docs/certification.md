---
title: Certification
description: How the OVHcloud Public Cloud provider is certified — make certify-ovhcloud runs the upstream conformance baseline plus the extension suite against a live endpoint, credential-free.
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
repo's extension suite, with no failures and no skipped-as-failed behaviors.

## One command

```sh
make certify-ovhcloud
```

That target (`hack/run-certify.sh ovhcloud`) is fully **credential-free**. It:

1. Resolves the bigfleet checkout that owns the authoritative contract — reusing
   `$BIGFLEET_SRC` if set, otherwise cloning the exact version pinned in the
   provider's `go.mod` into `.cache/bigfleet-src`.
2. Builds `./bin/ovhcloud` and boots it with `--provider=certify --seed-count=256`.
   With no `--region`, the provider's `--ovh-backend` resolves to `fake`, so no OVH
   project is touched — the extension suite consumes a fresh machine per behavior,
   hence the generous seed.
3. Runs the **upstream baseline** (`test/conformance/` in the bigfleet repo), then
   the **extension suite** (`conformance/suite`, build-tagged `certify`), both
   dialing that one endpoint.
4. Prints `CERTIFIED: ovhcloud passed the upstream baseline + the extension suite`
   — or fails non-zero on the first failing behavior, tearing the provider down.

Override the port with `PORT=...`.

## Profiles the OVHcloud provider claims

The harness certifies a provider against the **profiles** it advertises; behaviors
outside a claimed profile skip-as-pass. The harness probes the provider's
`Capabilities` over the wire and skips inapplicable behaviors with a reason (never
failing them).

- **core** — every provider (lifecycle, errors, fencing, concurrency, metadata,
  field-shape, list, property).
- **cloud** — implements `Delete` (Idle → Speculative). The OVH Public Cloud
  provider does (`Delete` = `servers.Delete`), so it claims **cloud**.
- **spot** — exposes SPOT capacity. **OVH Public Cloud is on-demand only**, so the
  provider does **not** claim `spot`; the SPOT-`interruption_probability > 0`
  behaviors skip-as-pass. See
  [Pricing & interruption](/providers/ovhcloud/pricing-and-interruption/) for why a
  zero interruption probability is the *correct* value here.
- **fault / durable / scale** — failure→`FAILED`, restart recovery, and scale
  lanes. These come from `providerkit` and pass by construction for any kit-based
  provider; run them through the report runner.

`make certify-ovhcloud` runs the credential-free core gate (baseline + the
black-box extension). The **complete** certification — every applicable lane —
runs through the `bfconformance` runner and emits a JUnit + JSON report with a
verdict:

```sh
make report-ovhcloud PROFILE=core,cloud,fault,durable,scale
# -> VERDICT: CERTIFIED, writes conformance-report/ovhcloud/{report.json,junit.xml}
```

A passing run reports **93/93 behaviors** across all eleven areas (Lifecycle,
Transition Matrix, Fencing, Concurrency, Metadata, Field Shape, List, Property,
Timeouts, Durability, Scale).

## Certifying a real endpoint

`make certify-ovhcloud` certifies the fake backend in CI. To certify the provider
*talking to real OVH Public Cloud*, run it yourself against your project and point
the extension suite at it:

```sh
# 1. Boot the provider against real OVH (OS_* sourced; see Install / Configuration).
./bin/ovhcloud \
  --addr 127.0.0.1:9099 \
  --region GRA \
  --image <BASE_IMAGE_UUID> \
  --key-name bigfleet-ovh \
  --ssh-key ./id_ed25519 \
  --offerings ./offerings.json

# 2. In another shell, run the extension suite against that endpoint.
go -C conformance test -tags=certify -count=1 ./suite/... -target=127.0.0.1:9099
```

A real run exercises the full lifecycle — `servers.Create` → wait-for-ACTIVE →
SSH `Configure`/`Drain` → `servers.Delete` — so the endpoint needs the OpenStack
user (see [Credentials](/providers/ovhcloud/credentials/)) and an image that
authorises the keypair and ships the bootstrap hook. It will create and destroy
real instances; certify in a throwaway project on a cheap flavor (e.g. `b2-7`) and
tear the instances down.

## Why this provider does not opt out of the CI gate

A provider that cannot stand up without cloud credentials may add an empty
`providers/ovhcloud/.ci-no-conformance` marker to skip the CI `certify` job. The
OVHcloud provider **does not** carry this marker, and must not: its `fake` backend
stands up with no credentials, so `make certify-ovhcloud` runs and stays green on
every PR. Adding the opt-out here would forfeit that credential-free certification
gate.

## See also

- [Conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md) — the behavior registry, profiles, and how to add a behavior.
- [Pricing & interruption](/providers/ovhcloud/pricing-and-interruption/) — why `interruption_probability` is a genuine zero on OVH Public Cloud.
- [Credentials & auth](/providers/ovhcloud/credentials/) — the OpenStack user a real-endpoint certification run needs.
