---
title: Troubleshooting
description: Diagnose common issues with the BigFleet OVHcloud Public Cloud provider — stuck CREATING, FAILED after Configure, fencing rejections, and the fake backend in production.
sidebar:
  order: 7
  label: Troubleshooting
---

This page maps the symptoms you'll actually see to their cause. Start with the
metrics (`bigfleet_ovh_*`) and the structured logs — every RPC logs `method`,
`code`, and `dur_ms`, and lifecycle errors log at WARN.

## "It's creating real nothing" — `ovh_backend=fake` in production

**Symptom:** no instances appear in the OVH console; the startup log says
`ovh_backend=fake` and warns about the in-memory backend.

**Cause:** the provider resolved to the fake backend because `--region` was not
set (or `--ovh-backend=auto` with no region). The fake backend creates no real
instances.

**Fix:** set `--region` (and the OS_* credentials). With `--ovh-backend=auto`,
a set `--region` selects the real backend; or force it with `--ovh-backend=ovh`.

## A machine sticks in `CREATING`, then goes `FAILED`

**Cause:** `servers.Create` succeeded but the instance never reached `ACTIVE`
within the Create timeout (the provider blocks on ACTIVE so Idle means reachable),
or the create itself errored.

**Check:**

- `bigfleet_ovh_api_calls_total{op="CreateServer",outcome="error"}` and the WARN
  log line — a bad `--image` UUID, an unknown flavor in the region, a missing
  `--network`, or quota exhaustion all surface here.
- The instance's status in the OVH console — `ERROR` means the hypervisor rejected
  it (image/flavor/quota); the provider fails fast on `ERROR`.
- `last_error` on the machine (`Get`) carries the reason.

**Fix:** correct the image UUID / flavor / network, or raise the project quota.

## A machine goes `FAILED` right after `Configure`

**Cause:** the bootstrap delivery over SSH failed. Most common reasons:

- **No `--ssh-key`.** Without it, Configure cannot deliver the blob and fails. Set
  `--ssh-key` and the matching `--key-name` keypair.
- **The keypair public key isn't authorised on the instance.** Ensure
  `--key-name` names the OpenStack keypair whose public key the base image's cloud
  user (`--ssh-user`, default `ubuntu`) accepts.
- **Host-key mismatch.** The presented SSH host key didn't match the pin from
  create — the connection aborts as a possible MITM. Look for `host key mismatch`
  in the logs. Legitimately, this happens if the instance was rebuilt out of band;
  delete and re-create the slot.
- **The bootstrap hook exited non-zero.** Your image's `--bootstrap-hook` failed
  to join the cluster. The provider waits for the hook to succeed, so a broken join
  becomes `FAILED` (by design — not a falsely-Idle node). Check the hook's logs on
  the instance; the blob is at `<hook>.blob`.

**Fix:** address the cause above; the shard re-drives Configure on the next
reconcile.

## `FailedPrecondition` errors on Create/Configure/Drain/Delete

**Cause:** these are **fencing rejections**, not bugs. A mutating RPC arrived with
a token that is not strictly newer than the per-`(shard_id, machine_id)` high-water mark — a zombie or
out-of-order shard. The provider reserves `FAILED_PRECONDITION` exclusively for
fencing.

**Check:** `bigfleet_ovh_grpc_requests_total{code="FailedPrecondition"}`. A steady
stream points at a shard epoch/sequence problem on the BigFleet side, not the
provider. A few during a failover are expected.

## Inventory looks wrong after a restart

**Cause:** the provider was running **without `--state`**, so fence marks, the
idempotency map, inventory, and bindings were in memory only and lost on restart.

**Fix:** always run with `--state` on a PersistentVolume in production
(`state.enabled=true` + `state.persistence.enabled=true`). The FileStore is the
primary restart path; the background reconcile (`--reconcile-interval`) then
re-reconciles against live OpenStack truth. With `--state`, a kill+restart recovers
marks, ops, and bindings exactly (this is the durability lane of certification).

## `unknown flavor "…"` at Create

**Cause:** the offering names a flavor that does not exist in the region (or is not
available to the project). The provider resolves flavor names to ids from
`flavors.ListDetail`.

**Fix:** use a valid OVH flavor for the region (e.g. `b2-7`, `c2-15`); list them
with `openstack flavor list`. The pinned table covers `allocatable` for common
flavors, but the real flavor must exist in the region to launch.

## `network "Ext-Net" not found`

**Cause:** `--network` named a network that doesn't exist in the region, or the
user can't see it.

**Fix:** pass a valid network name or UUID (`openstack network list`), or leave
`--network` empty to use the project default.

## Drain takes a long time / times out

**Cause:** `kubectl drain` is honouring PodDisruptionBudgets and
`grace_period_seconds`. The Drain timeout is generous (15m) for exactly this.

**Fix:** usually none needed — let it complete. If it consistently times out,
inspect PDBs on the workloads; a too-strict PDB can block drain indefinitely.
