---
title: Certification
description: How the Hetzner Cloud provider is certified — make certify-hetzner runs the upstream conformance baseline plus the extension suite against a live endpoint.
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
make certify-hetzner
```

That target (`hack/run-certify.sh hetzner`) is fully **credential-free**. It:

1. Resolves the bigfleet checkout that owns the authoritative contract — reusing
   `$BIGFLEET_SRC` if set, otherwise cloning the exact version pinned in the
   provider's `go.mod` into `.cache/bigfleet-src`.
2. Builds `./bin/hetzner` and boots it with `--provider=certify --seed-count=256`.
   With no token, the provider's `--hetzner-backend` resolves to `fake`, so no
   Hetzner project is touched — the extension suite consumes a fresh machine per
   behavior, hence the generous seed.
3. Runs the **upstream baseline** (`test/conformance/` in the bigfleet repo),
   then the **extension suite** (`conformance/suite`, build-tagged `certify`),
   both dialing that one endpoint.
4. Prints `CERTIFIED: hetzner passed the upstream baseline + the extension suite`
   — or fails non-zero on the first failing behavior, tearing the provider down.

Override the port with `PORT=...`.

## Profiles the Hetzner provider claims

The harness certifies a provider against the **profiles** it advertises;
behaviors outside a claimed profile skip-as-pass. The harness probes the
provider's `Capabilities` over the wire and skips inapplicable behaviors with a
reason (never failing them).

- **core** — every provider (lifecycle, errors, fencing, concurrency, metadata,
  field-shape, list, property).
- **cloud** — implements `Delete` (Idle → Speculative). The Hetzner Cloud provider
  does (`Delete` = `Server.Delete`), so it claims **cloud**.
- **spot** — exposes SPOT capacity. **Hetzner Cloud is on-demand only**, so the
  provider does **not** claim `spot`; the SPOT-`interruption_probability > 0`
  behaviors skip-as-pass. See
  [Pricing & interruption](/providers/hetzner/pricing-and-interruption/) for why a
  zero interruption probability is the *correct* value here.
- **fault / durable / scale** — failure→`FAILED`, restart recovery, and scale
  lanes. These come from `providerkit` and pass by construction for any kit-based
  provider; run them through the report runner.

`make certify-hetzner` runs the credential-free core gate (baseline + the
black-box extension). The **complete** certification — every applicable lane —
runs through the `bfconformance` runner and emits a JUnit + JSON report with a
verdict:

```sh
make report-hetzner PROFILE=core,cloud
# -> VERDICT: CERTIFIED, writes conformance-report/hetzner/{report.json,junit.xml}
```

## Certifying a real endpoint

`make certify-hetzner` certifies the fake backend in CI. To certify the provider
*talking to real Hetzner Cloud*, run it yourself against your project and point
the extension suite at it:

```sh
# 1. Boot the provider against real Hetzner (see Install & deploy / Configuration).
./bin/hetzner \
  --addr 127.0.0.1:9099 \
  --token "$HCLOUD_TOKEN" \
  --image ubuntu-24.04 \
  --ssh-key ./id_ed25519 \
  --offerings ./offerings.json

# 2. In another shell, run the extension suite against that endpoint.
go -C conformance test -tags=certify -count=1 ./suite/... -target=127.0.0.1:9099
```

A real run exercises the full lifecycle — `Server.Create` → wait-for-running →
SSH `Configure`/`Drain` → `Server.Delete` — so the endpoint needs a Read & Write
token (see [Credentials](/providers/hetzner/credentials/)) and an image that
authorises `--ssh-key` and ships the bootstrap hook. It will create and destroy
real servers; certify in a throwaway project and tear the servers down (the demo
in the repo README runs this end-to-end for cents).

## Why this provider does not opt out of the CI gate

A provider that cannot stand up without cloud credentials may add an empty
`providers/hetzner/.ci-no-conformance` marker to skip the CI `certify` job. The
Hetzner provider **does not** carry this marker, and must not: its `fake` backend
stands up with no token, so `make certify-hetzner` runs and stays green on every
PR. Adding the opt-out here would forfeit that credential-free certification gate.

## See also

- [Conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md) — the behavior registry, profiles, and how to add a behavior.
- [Pricing & interruption](/providers/hetzner/pricing-and-interruption/) — why `interruption_probability` is a genuine zero on Hetzner Cloud.
- [Credentials & auth](/providers/hetzner/credentials/) — the token a real-endpoint certification run needs.
