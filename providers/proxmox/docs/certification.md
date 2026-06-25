---
title: Certification
description: How the Proxmox VE provider is certified — make certify-proxmox runs the upstream conformance baseline plus the extension suite credential-free against the fake backend.
sidebar:
  order: 8
  label: Certification
---

This provider is **certified**: it passes every behavior in the BigFleet
conformance program — the same bar every provider must clear — for the **core**
and **cloud** profiles, so it launches, configures, drains, and deletes machines
correctly under load, failure, and restart. You do not need to run anything here
to use it in production; this page exists if you want to reproduce that verdict
yourself, locally or in your own CI.

"Certified" here means exactly what it means in the
[conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md):
the provider passes **both** the upstream authoritative baseline **and** this
repo's extension suite, with no failures and no skipped-as-failed behaviors.

## One command

```sh
make certify-proxmox
```

That target is fully **credential-free**. It:

1. Resolves the bigfleet checkout that owns the authoritative contract — reusing
   `$BIGFLEET_SRC` if set, otherwise cloning the exact version pinned in the
   provider's `go.mod`.
2. Builds the provider and boots it with `--provider=certify` and a generous
   `--seed-count`. It uses `--use-fake-backend`, so no Proxmox cluster is touched — the
   extension suite consumes a fresh machine per behavior, hence the generous seed.
3. Runs the **upstream baseline** (`test/conformance/` in the bigfleet repo), then
   the **extension suite** (`conformance/suite`, build-tagged `certify`), both
   dialing that one endpoint.
4. Prints `CERTIFIED: proxmox passed the upstream baseline + the extension suite`
   — or fails non-zero on the first failing behavior, tearing the provider down.

## What the two suites check

The certification harness is a pure black-box gRPC client: it dials `--addr` and
uses only the wire RPCs of `bigfleet.v1alpha1.CapacityProvider` — no
`providerkit` imports, no process introspection. It detects what the provider
supports through a `Capabilities` probe and **skips inapplicable behaviors with a
reason** (never failing them).

**Upstream baseline** — the immovable, authoritative contract maintained in the
bigfleet repo. We run it verbatim and never modify it; it is the floor every
certified provider clears.

**Extension suite** — the BigFleet conformance program: a frozen registry of
**93 behaviors across 11 areas** that deepens the baseline (stronger invariants
under distinct, append-only ids, never forking the upstream tests):

| Area | What it certifies |
|---|---|
| Lifecycle & state machine | residue-free round-trips; per-edge transitional, cluster, and host invariants |
| Transition matrix / errors | the out-of-position matrix, idempotent no-ops, code discipline, edge inputs |
| Fencing | fence-before-everything, per-`(shard_id, machine_id)` isolation, exhaustive `(epoch, sequence)` ordering |
| Concurrency & idempotency | N parallel retries collapse to one `operation_id` and exactly one effect |
| Metadata | `shard_metadata` verbatim echo, clear-on-drain, clean replace |
| Field shape & cost | top-level `instance_type`/`zone`/`capacity_type`; price ≥ 0; `interruption_probability` ∈ [0,1] |
| List, revision & pagination | filters, `max_results`, `since_revision` deltas, completeness at scale |
| Timeouts & failure | actuator error / timeout → `FAILED` + `last_error`; a late completion is discarded |
| Durability / restart | fence marks, idempotency, bindings, and inventory survive a kill + restart |
| Scale & soak | large inventory, churn-soak, latency budgets, parallel throughput |
| Property / fuzz | seeded-random lifecycle / fencing / metadata oracles |

The full, frozen registry of all 93 behaviors — every assertion, profile, and id —
is the [conformance program](/conformance/). This provider clears every one
applicable to the profiles it claims.

The Proxmox provider's `interruption_probability` is exactly `0` for every
machine (these VMs are not preemptible), so the field-shape area's `[0,1]` bound
holds by construction. It does not claim the `spot` profile, so the SPOT-specific
`interruption_probability > 0` assertion skips-as-pass.

## Profiles the Proxmox provider claims

The harness certifies a provider against the **profiles** it advertises; behaviors
outside a claimed profile skip-as-pass:

- **core** — every provider (lifecycle, errors, fencing, concurrency, metadata,
  field-shape, list, property).
- **cloud** — implements `Delete` (Idle → Speculative). The Proxmox provider does
  (`Delete` = stop + destroy/purge the VM and its disks).

It does **not** claim `spot` (Proxmox VMs are not preemptible — every machine is
`ON_DEMAND` with `interruption_probability` of `0`), and a `spot` offering is
rejected at startup.

`make certify-proxmox` runs the credential-free core gate (baseline + the
black-box extension). The **complete** certification across the claimed lanes runs
through the `bfconformance` runner and emits a JUnit + JSON report:

```sh
make report-proxmox PROFILE=core,cloud
# -> VERDICT: CERTIFIED
```

## Certifying a real endpoint

`make certify-proxmox` certifies the fake backend in CI. To certify the provider
*talking to a real Proxmox cluster*, run it yourself against your cluster and
point the extension suite at it:

```sh
# 1. Boot the provider against real Proxmox (see Install & deploy / Configuration).
./bin/proxmox \
  --addr 127.0.0.1:9099 \
  --proxmox-api-url https://pve1:8006/api2/json \
  --proxmox-token-id 'bigfleet@pve!autoscaler' \
  --proxmox-token-file ./token \
  --proxmox-ca-file /etc/pve/pve-root-ca.pem \
  --proxmox-pool bigfleet \
  --nodes pve1,pve2 \
  --template-vmid 9000 \
  --offerings ./offerings.json

# 2. In another shell, run the extension suite against that endpoint.
go -C conformance test -tags=certify -count=1 ./suite/... -target=127.0.0.1:9099
```

A real run exercises the full lifecycle — clone → start → wait-for-agent →
guest-agent `Configure`/`Drain` → destroy — so the endpoint needs the API token
privileges documented on the [Credentials](/providers/proxmox/credentials/) page
and a template with `qemu-guest-agent` and `kubelet`. It will create and destroy
real VMs; certify against a throwaway pool or a dedicated test cluster.

## See also

- [Conformance program](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md) — the behavior registry, profiles, and how to add a behavior.
- [Credentials](/providers/proxmox/credentials/) — the token privileges a real-endpoint certification run needs.
- [Install & deploy](/providers/proxmox/install/) and [Configuration](/providers/proxmox/configuration/) — booting the provider against a real cluster.
