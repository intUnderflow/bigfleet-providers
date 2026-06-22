---
title: Certification
description: How the Latitude.sh provider is certified — make certify-latitude runs the upstream conformance baseline plus the extension suite against a live endpoint.
sidebar:
  order: 8
  label: Certification
---

This provider is **certified**: it passes every behavior in the BigFleet
conformance program — the same bar every provider must clear — so you can trust
it to deploy, configure, drain, and deprovision machines correctly under load,
failure, and restart. You do not need to run anything here to use it in
production; this page exists if you want to reproduce that verdict yourself.

"Certified" here means exactly what it means in the
[conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md):
the provider passes **both** the upstream authoritative baseline **and** this
repo's extension suite — 93 behaviors across 11 areas — with no failures and no
skipped-as-failed behaviors.

## One command

```sh
make certify-latitude
```

That target runs fully **credential-free**. It:

1. Resolves the bigfleet checkout that owns the authoritative contract — reusing
   `$BIGFLEET_SRC` if set, otherwise cloning the exact version pinned in the
   provider's `go.mod`.
2. Builds the `latitude` binary and boots it with `--provider=certify
   --seed-count=256`. With **no token and no project**, the provider's
   `--latitude-backend` resolves to `fake`, so **no Latitude project is touched
   and no money is spent** — the extension suite consumes a fresh machine per
   behavior, hence the generous seed.
3. Runs the **upstream baseline**, then the **extension suite**, both dialing that
   one endpoint.
4. Prints `CERTIFIED: latitude passed the upstream baseline + the extension
   suite` — or fails non-zero on the first failing behavior, tearing the provider
   down.

Override the port with `PORT=...`.

## Profiles the Latitude provider claims

The harness certifies a provider against the **profiles** it advertises;
behaviors outside a claimed profile skip-as-pass. The harness probes the
provider's `Capabilities` over the wire and skips inapplicable behaviors with a
reason (never failing them).

- **core** — every provider (lifecycle, errors, fencing, concurrency, metadata,
  field-shape, list, property).
- **cloud** — implements `Delete` (Idle → Speculative). Latitude.sh has a **real
  Delete** (`DELETE /servers/{id}` deprovisions the physical box), so the provider
  claims **cloud**. This is the decision that keeps capacity reclaimable: since
  M73 the shard only emits `Delete` for `ON_DEMAND`/`SPOT`, so the provider
  declares `capacity_type = ON_DEMAND` rather than `BARE_METAL`.
- **spot** — exposes SPOT capacity. **Latitude bare metal is on-demand only**, so
  the provider does **not** claim `spot`; the SPOT-`interruption_probability > 0`
  behaviors skip-as-pass. See
  [Pricing & interruption](/providers/latitude/pricing-and-interruption/) for why
  a zero interruption probability is the *correct* value here.
- **bare-metal** — an *owned* hardware free pool where `Delete` is
  `Unimplemented`. Latitude is on-demand with a real Delete, so the provider does
  **not** claim `bare-metal` — and rejects a `bare_metal` `capacity_type` in an
  offering rather than suppressing Delete.
- **fault / durable / scale** — failure→`FAILED`, restart recovery, and scale
  lanes. These come from `providerkit` and pass by construction for any kit-based
  provider; run them through the report runner.

The **complete** certification — every applicable lane — runs through the report
runner and emits a JUnit + JSON report with a verdict:

```sh
make report-latitude PROFILE=core,cloud
# -> VERDICT: CERTIFIED, writes the report.json + junit.xml
```

## Certifying a real endpoint

`make certify-latitude` certifies the fake backend in CI. To certify the provider
*talking to real Latitude.sh*, run it yourself against your project and point the
extension suite at it:

```sh
# 1. Boot the provider against real Latitude (see Install & deploy / Configuration).
./latitude \
  --addr 127.0.0.1:9099 \
  --token "$LATITUDESH_API_TOKEN" \
  --project proj_yourprojectid \
  --operating-system ubuntu_22_04_x64_lts \
  --ssh-key ./id_ed25519 \
  --offerings ./offerings.json

# 2. In another shell, run the extension suite against that endpoint.
go -C conformance test -tags=certify -count=1 ./suite/... -target=127.0.0.1:9099
```

A real run exercises the full lifecycle — `Servers.Create` → wait-for-powered-on
→ SSH `Configure`/`Drain` → `Servers.Delete` — so the endpoint needs a token with
full project access (see [Credentials](/providers/latitude/credentials/)) and an
OS image that authorises `--ssh-key` and ships the bootstrap hook. **It will
deploy and deprovision real bare-metal servers, billed by the hour** — certify in
a throwaway project and tear the servers down promptly.

## Why this provider does not opt out of the CI gate

A provider that cannot stand up without cloud credentials may add an empty
`.ci-no-conformance` marker to skip the CI `certify` job. The Latitude provider
**does not** carry this marker, and must not: its `fake` backend stands up with
no token and no project, so `make certify-latitude` runs and stays green on every
PR. Adding the opt-out here would forfeit that credential-free certification gate.

## See also

- [Conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md) — the behavior registry, profiles, and how to add a behavior.
- [Pricing & interruption](/providers/latitude/pricing-and-interruption/) — why `interruption_probability` is a genuine zero on Latitude bare metal.
- [Credentials & auth](/providers/latitude/credentials/) — the token and project a real-endpoint certification run needs.
