---
title: Certification
description: How the DigitalOcean provider is certified — make certify-digitalocean runs the upstream conformance baseline plus the extension suite against a live endpoint.
sidebar:
  order: 8
  label: Certification
---

This provider is **certified**: it passes every applicable behavior in the
BigFleet conformance program — the same bar every provider must clear — so you
can trust it to create, configure, drain, and delete machines correctly under
load, failure, and restart. You do not need to run anything here to use it in
production; this page exists if you want to reproduce that verdict yourself.

"Certified" here means exactly what it means in the
[conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md):
the provider passes **both** the upstream authoritative baseline **and** this
repo's extension suite, with no failures and no skipped-as-failed behaviors.

## One command

```sh
make certify-digitalocean
```

That target (`hack/run-certify.sh digitalocean`) is fully **credential-free**. It:

1. Resolves the bigfleet checkout that owns the authoritative contract — reusing
   `$BIGFLEET_SRC` if set, otherwise cloning the exact version pinned in the
   provider's `go.mod` into `.cache/bigfleet-src`.
2. Builds `./bin/digitalocean` and boots it with `--provider=certify
   --seed-count=256`. With no token **and** no `--region`, the provider's
   `--do-backend` resolves to `fake`, so no DigitalOcean account is touched — the
   extension suite consumes a fresh machine per behavior, hence the generous seed.
3. Runs the **upstream baseline** (`test/conformance/` in the bigfleet repo),
   then the **extension suite** (`conformance/suite`, build-tagged `certify`),
   both dialing that one endpoint.
4. Prints `CERTIFIED: digitalocean passed the upstream baseline + the extension
   suite` — or fails non-zero on the first failing behavior, tearing the provider
   down.

Override the port with `PORT=...`.

## What the two suites check

The certification harness is a pure black-box gRPC client: it dials `--addr` and
uses only the wire RPCs of `bigfleet.v1alpha1.CapacityProvider` — no
`providerkit` imports, no process introspection. It detects what the provider
supports through a `Capabilities` probe and **skips inapplicable behaviors with a
reason** (never failing them) — which is how the SPOT-only behaviors are handled
here (see [Profiles](#profiles-the-digitalocean-provider-claims)).

**Upstream baseline** — the immovable, authoritative contract maintained in the
bigfleet repo. We run it verbatim and never modify it; it is the floor every
certified provider clears.

**Extension suite** — the BigFleet conformance program: a frozen registry of
**92 behaviors across 11 areas** that deepens the baseline (stronger invariants
under distinct, append-only ids). The full registry is the
[conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md).
This provider clears every applicable one.

## Profiles the DigitalOcean provider claims

The harness certifies a provider against the **profiles** it advertises;
behaviors outside a claimed profile skip-as-pass. The harness probes the
provider's `Capabilities` over the wire and skips inapplicable behaviors with a
reason (never failing them).

- **core** — every provider (lifecycle, errors, fencing, concurrency, metadata,
  field-shape, list, property).
- **cloud** — implements `Delete` (Idle → Speculative). The DigitalOcean provider
  does (`Delete` = `Droplets.Delete`), so it claims **cloud**.
- **spot** — exposes SPOT capacity. **DigitalOcean Droplets are on-demand only**,
  so the provider does **not** claim `spot`; the SPOT-`interruption_probability >
  0` behaviors skip-as-pass. The genuine, provider-declared
  `interruption_probability` is exactly `0.0`, which is the *correct* value here,
  not a forgotten field — see [Configuration](configuration.md). This is the one
  profile difference from the AWS provider, which claims
  `core,cloud,spot,fault,durable,scale` because EC2 has a real spot market.
- **fault / durable / scale** — failure→`FAILED`, restart recovery, and scale
  lanes. These come from `providerkit` and pass by construction for any kit-based
  provider; run them through the report runner.

`make certify-digitalocean` runs the credential-free core gate (baseline + the
black-box extension). The **complete** certification — every applicable lane —
runs through the `bfconformance` runner and emits a JUnit + JSON report with a
verdict:

```sh
make report-digitalocean PROFILE=core,cloud,fault,durable,scale
# -> VERDICT: CERTIFIED, writes conformance-report/digitalocean/{report.json,junit.xml}
```

Note the profile list omits `spot` (contrast AWS's
`core,cloud,spot,fault,durable,scale`) because DigitalOcean offers no spot
product.

## Certifying a real endpoint

`make certify-digitalocean` certifies the fake backend in CI. To certify the
provider *talking to real DigitalOcean*, run it yourself against your account and
point the extension suite at it:

```sh
# 1. Boot the provider against real DigitalOcean (see Install & deploy / Configuration).
./bin/digitalocean \
  --addr 127.0.0.1:9099 \
  --region nyc3 \
  --token "$DIGITALOCEAN_TOKEN" \
  --image ubuntu-24-04-x64 \
  --base-user-data ./agent-init.yaml \
  --offerings ./offerings.json \
  --bootstrap-addr :9443 \
  --bootstrap-endpoint https://do-provider.example:9443 \
  --bootstrap-tls-cert boot.pem --bootstrap-tls-key boot-key.pem

# 2. In another shell, run the extension suite against that endpoint.
go -C conformance test -tags=certify -count=1 ./suite/... -target=127.0.0.1:9099
```

A real run exercises the full lifecycle — `Droplets.Create` → wait-for-active →
agent-channel `Configure`/`Drain` → `Droplets.Delete` — so the endpoint needs a
read + write Droplets token (see [Credentials](credentials.md)), an image that
ships the on-host agent, and a bootstrap channel the Droplets can reach. It will
create and destroy real Droplets; certify in a throwaway account and tear the
Droplets down.

## Why this provider does not opt out of the CI gate

A provider that cannot stand up without cloud credentials may add an empty
`providers/digitalocean/.ci-no-conformance` marker to skip the CI `certify` job.
The DigitalOcean provider **does not** carry this marker, and must not: its
`fake` backend stands up with no token and no region, so
`make certify-digitalocean` runs and stays green on every PR. Adding the opt-out
here would forfeit that credential-free certification gate.

## See also

- [Conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md) — the behavior registry, profiles, and how to add a behavior.
- [Configuration](configuration.md) — why `interruption_probability` is a genuine zero on DigitalOcean, and why the provider does not claim `spot`.
- [Credentials & auth](credentials.md) — the token a real-endpoint certification run needs.
