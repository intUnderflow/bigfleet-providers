---
title: Certification
description: How the libvirt provider is certified — make certify-libvirt runs the upstream conformance baseline plus the extension suite against a live, credential-free endpoint.
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
make certify-libvirt
```

That target (`hack/run-certify.sh libvirt`) is fully **credential-free**. It:

1. Resolves the bigfleet checkout that owns the authoritative contract — reusing
   `$BIGFLEET_SRC` if set, otherwise cloning the exact version pinned in the
   provider's `go.mod` into `.cache/bigfleet-src`.
2. Builds `./bin/libvirt` and boots it with `--provider=certify --seed-count=256`.
   With no `--connect`, the provider's `--libvirt-backend` resolves to `fake`, so
   no hypervisor is touched — the extension suite consumes a fresh machine per
   behavior, hence the generous seed.
3. Runs the **upstream baseline** (`test/conformance/` in the bigfleet repo), then
   the **extension suite** (`conformance/suite`, build-tagged `certify`), both
   dialing that one endpoint.
4. Prints `CERTIFIED: libvirt passed the upstream baseline + the extension suite`
   — or fails non-zero on the first failing behavior, tearing the provider down.

Override the port with `PORT=...`.

## Profiles the libvirt provider claims

The harness certifies a provider against the **profiles** it advertises;
behaviors outside a claimed profile skip-as-pass.

- **core** — every provider (lifecycle, errors, fencing, concurrency, metadata,
  field-shape, list, property).
- **cloud** — implements `Delete` (Idle → Speculative). The default `on_demand`
  libvirt pool does (`Delete` = `virDomainDestroy` + `virDomainUndefine` + overlay
  delete is meaningful), so it claims **cloud**. If you run a fixed
  `--capacity-type bare_metal` pool instead, you claim **bare-metal** and `Delete`
  is never sent (M73).
- **spot** — exposes SPOT capacity. **Local KVM has no preemption market**, so the
  provider does **not** claim `spot`; the SPOT-`interruption_probability > 0`
  behaviors skip-as-pass. See
  [Pricing & interruption](/providers/libvirt/pricing-and-interruption/) for why a
  zero interruption probability is the *correct* value here.
- **fault / durable / scale** — failure→`FAILED`, restart recovery, and scale
  lanes. These come from `providerkit` and pass by construction for any kit-based
  provider; run them through the report runner.

`make certify-libvirt` runs the credential-free core gate (baseline + the
black-box extension). The **complete** certification — every applicable lane —
runs through the `bfconformance` runner and emits a JUnit + JSON report with a
verdict:

```sh
make report-libvirt PROFILE=core,cloud
# -> VERDICT: CERTIFIED, writes conformance-report/libvirt/{report.json,junit.xml}
```

Running the full multi-lane set confirms the kit-provided lanes pass too:

```sh
make report-libvirt PROFILE=core,cloud,fault,durable,scale
# behaviors: 92 total — 92 passed, 0 failed, 0 skipped, 0 not-implemented
# -> VERDICT: CERTIFIED
```

## Certifying a real endpoint

`make certify-libvirt` certifies the fake backend in CI. To certify the provider
*talking to real libvirt*, run it yourself against your hosts and point the
extension suite at it:

```sh
# 1. Boot the provider against real libvirt (see Install & deploy / Configuration).
./bin/libvirt \
  --addr 127.0.0.1:9099 \
  --connect 'rack1=qemu+ssh://bigfleet@host-a/system' \
  --image ubuntu-24.04.qcow2 \
  --offerings ./offerings.json

# 2. In another shell, run the extension suite against that endpoint.
go -C conformance test -tags=certify -count=1 ./suite/... -target=127.0.0.1:9099
```

A real run exercises the full lifecycle — define + start → wait-running →
guest-agent `Configure`/`Drain` → destroy + undefine — so the endpoint needs a
reachable host (see [Credentials](/providers/libvirt/credentials/)) and a base
image that runs `qemu-guest-agent` and ships the bootstrap hook. It will create
and destroy real domains; certify against a throwaway host and clean up
afterwards.

## Why this provider does not opt out of the CI gate

A provider that cannot stand up without credentials may add an empty
`providers/libvirt/.ci-no-conformance` marker to skip the CI `certify` job. The
libvirt provider **does not** carry this marker, and must not: its `fake` backend
stands up with no hypervisor, so `make certify-libvirt` runs and stays green on
every PR. Adding the opt-out here would forfeit that credential-free certification
gate.

## See also

- [Conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md) — the behavior registry, profiles, and how to add a behavior.
- [Pricing & interruption](/providers/libvirt/pricing-and-interruption/) — why `interruption_probability` is a genuine zero on local KVM.
- [Credentials & auth](/providers/libvirt/credentials/) — the libvirt connection a real-endpoint certification run needs.
