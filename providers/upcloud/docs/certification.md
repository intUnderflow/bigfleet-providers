---
title: Certification
description: How the UpCloud provider is certified — make certify-upcloud runs the upstream conformance baseline plus the extension suite credential-free against the fake backend.
sidebar:
  order: 8
  label: Certification
---

This provider is **certified**: it passes every applicable behavior in the
BigFleet conformance program — the same bar every provider must clear — so you can
trust it to create, configure, drain, and delete machines correctly under load,
failure, and restart. You do not need to run anything here to use it in
production; this page exists if you want to reproduce that verdict yourself.

"Certified" here means exactly what it means in the
[conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md):
the provider passes **both** the upstream authoritative baseline **and** this
repo's extension suite, with no failures and no skipped-as-failed behaviors.

## One command

```sh
make certify-upcloud
```

That target (`hack/run-certify.sh upcloud`) is fully **credential-free**. It:

1. Resolves the bigfleet checkout that owns the authoritative contract — reusing
   `$BIGFLEET_SRC` if set, otherwise cloning the exact version pinned in the
   provider's `go.mod` into `.cache/bigfleet-src`.
2. Builds `./bin/upcloud` and boots it with `--provider=certify
   --seed-count=256`. With no credentials **and** no `--zone`, the provider's
   `--upcloud-backend` resolves to `fake`, so no UpCloud account is touched — the
   extension suite consumes a fresh machine per behavior, hence the generous seed.
3. Runs the **upstream baseline** (`test/conformance/` in the bigfleet repo), then
   the **extension suite** (`conformance/suite`, build-tagged `certify`), both
   dialing that one endpoint.
4. Prints `CERTIFIED: upcloud passed the upstream baseline + the extension suite` —
   or fails non-zero on the first failing behavior, tearing the provider down.

Override the port with `PORT=...`.

## What the two suites check

The certification harness is a pure black-box gRPC client: it dials `--addr` and
uses only the wire RPCs of `bigfleet.v1alpha1.CapacityProvider` — no `providerkit`
imports, no process introspection. It detects what the provider supports through a
`Capabilities` probe and **skips inapplicable behaviors with a reason** (never
failing them) — which is how the SPOT-only behaviors are handled here (see
[Profiles](#profiles-the-upcloud-provider-claims)).

**Upstream baseline** — the immovable, authoritative contract maintained in the
bigfleet repo. We run it verbatim and never modify it; it is the floor every
certified provider clears.

**Extension suite** — the BigFleet conformance program: a frozen registry of
behaviors across lifecycle, the transition matrix, fencing, concurrency, metadata,
field-shape, list/pagination, timeouts/failure, durability, scale/soak, and
property/fuzz. It deepens the baseline under distinct, append-only ids. This
provider clears every applicable one.

## Profiles the UpCloud provider claims

The harness certifies a provider against the **profiles** it advertises; behaviors
outside a claimed profile skip-as-pass. The harness probes the provider's
`Capabilities` over the wire and skips inapplicable behaviors with a reason (never
failing them).

- **core** — every provider (lifecycle, errors, fencing, concurrency, metadata,
  field-shape, list, property). The UpCloud provider claims **core**.
- **cloud** — implements `Delete` (Idle → Speculative). The UpCloud provider does
  (`Delete` = stop + `DeleteServerAndStorages`), so it claims **cloud**.
- **spot** — exposes SPOT capacity. **UpCloud cloud servers are on-demand only**,
  so the provider does **not** claim `spot`; the SPOT-`interruption_probability >
  0` behaviors skip-as-pass. The genuine, provider-declared
  `interruption_probability` is exactly `0.0`, which is the *correct* value here,
  not a forgotten field — see [Configuration](configuration.md).
- **bare-metal** — the UpCloud provider serves regular on-demand cloud servers, not
  bare metal, so it does **not** claim it.
- **fault / durable / scale** — failure→`FAILED`, restart recovery, and scale
  lanes. These come from `providerkit` and **pass by construction** for any
  kit-based provider; run them through the report runner.

So the UpCloud provider's claimed profiles are **core** and **cloud** (the
substrate-specific lanes), with fault/durable/scale passing by construction via the
kit. It does **not** claim `spot` (no spot market) or bare-metal.

`make certify-upcloud` runs the credential-free core gate (baseline + the black-box
extension). The **complete** certification — every applicable lane — runs through
the `bfconformance` runner and emits a JUnit + JSON report with a verdict:

```sh
make report-upcloud PROFILE=core,cloud
# -> VERDICT: CERTIFIED, writes conformance-report/upcloud/{report.json,junit.xml}
```

You can add the kit-provided lanes to the same run when you want the full sweep:

```sh
make report-upcloud PROFILE=core,cloud,fault,durable,scale
```

Note the profile list omits `spot` (UpCloud offers no spot product) and bare-metal.

## Certifying a real endpoint

`make certify-upcloud` certifies the fake backend in CI. To certify the provider
*talking to real UpCloud*, run it yourself against your account and point the
extension suite at it:

```sh
# 1. Boot the provider against real UpCloud (see Install & deploy / Configuration).
./bin/upcloud \
  --addr 127.0.0.1:9099 \
  --zone fi-hel1 \
  --template 0100...0200 \
  --offerings ./offerings.json \
  --ssh-key ./id --ssh-pubkey "$(cat ./id.pub)" \
  --base-user-data ./hook-init.yaml
# UPCLOUD_USERNAME / UPCLOUD_PASSWORD in the environment.

# 2. In another shell, run the extension suite against that endpoint.
go -C conformance test -tags=certify -count=1 ./suite/... -target=127.0.0.1:9099
```

A real run exercises the full lifecycle — `CreateServer` → wait-for-started →
SSH `Configure`/`Drain` → stop + `DeleteServerAndStorages` — so the endpoint needs
an API sub-account (see [Credentials](credentials.md)), a valid `--template`, an
image that ships the on-host hook, and SSH reachability to the servers. It will
create and destroy real servers (and their storage); certify in a throwaway
account and tear the servers down.

## Why this provider does not opt out of the CI gate

A provider that cannot stand up without cloud credentials may add an empty
`providers/upcloud/.ci-no-conformance` marker to skip the CI `certify` job. The
UpCloud provider **does not** carry this marker, and must not: its `fake` backend
stands up with no credentials and no zone, so `make certify-upcloud` runs and stays
green on every PR. Adding the opt-out here would forfeit that credential-free
certification gate.

## See also

- [Conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md) — the behavior registry, profiles, and how to add a behavior.
- [Configuration](configuration.md) — why `interruption_probability` is a genuine zero on UpCloud, and why the provider does not claim `spot`.
- [Credentials & auth](credentials.md) — the API sub-account a real-endpoint certification run needs.
