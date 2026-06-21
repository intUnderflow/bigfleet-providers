---
title: Troubleshooting
description: Common failure modes for the BigFleet UpCloud provider and how to diagnose them from logs, metrics, and Get/List.
sidebar:
  order: 7
  label: Troubleshooting
---

Most problems show up as a machine landing in `FAILED` (read `last_error` via
`Get`) or an UpCloud API error in the logs/metrics. Work from those two signals.

## The provider won't start

| Symptom | Cause | Fix |
|---|---|---|
| `UPCLOUD_USERNAME and UPCLOUD_PASSWORD (an API sub-account) are required for the upcloud backend` | Real backend with no credentials. | Set `UPCLOUD_USERNAME`/`UPCLOUD_PASSWORD` (or `--username`/`--password`), or use `--upcloud-backend=fake` for dev. |
| `--zone is required for the upcloud backend` | Real backend with no zone. | Set `--zone` (e.g. `fi-hel1`). |
| `--template (an OS template storage UUID) is required for the upcloud backend` | Real backend with no template. | Pass `--template` with a valid UpCloud OS-template storage UUID. |
| `both --tls-cert and --tls-key are required` / `--tls-ca set without --tls-cert/--tls-key` | Half-configured gRPC TLS. | Provide cert **and** key (and a CA for mTLS), or none. |
| `capacity_type ... is not offered by UpCloud (on-demand cloud servers only)` | A `spot`/`reserved`/`bare_metal` `capacity_type` in offerings. | Remove it — UpCloud has only on-demand cloud servers. |

## Provider boots in fake mode unexpectedly

If the log shows *"using the IN-MEMORY fake UpCloud backend"*, `auto` resolved to
`fake` because **credentials or `--zone` were missing**. The real backend needs
**both** `UPCLOUD_USERNAME`/`UPCLOUD_PASSWORD` **and** `--zone`; set all three (and
`--template`) to opt into the real backend. This is by design — a credential-free
run defaults to the simulator so it can never accidentally touch a real account.

## A machine reaches FAILED

`Get` the machine and read `last_error`:

| `last_error` mentions | Cause | Fix |
|---|---|---|
| `create server …` | `CreateServer` failed (bad plan/template/zone, account limit, credentials). | Check the plan/zone exist and the `--template` UUID is correct and visible to the sub-account; check the account's server limit. |
| `wait for server … to start` | Create timed out waiting for `started`. | Check UpCloud status; the zone may be temporarily out of capacity; the machine goes `FAILED`. |
| `host key mismatch: pinned … presented …` | The server's SSH host key did not match the fingerprint pinned at Create — a **possible MITM**, hard-failed. | Investigate the path to the server; do not disable verification. If the server was legitimately rebuilt out-of-band, delete the slot and let the provider re-create it (which re-pins). |
| `ssh dial …` / `ssh command on … failed` | Configure/Drain couldn't reach the server or the hook failed. | Confirm the provider can reach the server over SSH (`--ssh-user`, port 22); confirm the image ships `--bootstrap-hook` and it consumes the blob; confirm `--ssh-pubkey` matches `--ssh-key`. |
| `SSH delivery disabled (set --ssh-key)` | Configure with no SSH key configured. | Set `--ssh-key` (and `--ssh-pubkey`); without it Configure cannot deliver the blob. |
| `transition interrupted by a provider restart` | The process was killed mid-transition. | Expected after a kill without graceful drain; the shard re-drives on a fresh slot. Enable `--state` so fence marks/bindings survive. |

A `FAILED` machine is terminal-pending-cleanup: the shard recovers on a different
slot, never in place. Don't re-issue mutations against it.

## A server was stopped out-of-band

If someone stops a server in the UpCloud console while it still owns a slot, the
provider does **not** fail Configure/Drain on it: **`EnsureRunning` powers the
server back on (and waits for `started`)** before delivering the SSH bootstrap or
drain. You'll see a `EnsureRunning` API call in the metrics. No action needed — but
out-of-band stops add latency, so avoid them on managed servers.

## Delete leaves no orphaned storage

UpCloud storage (the OS disk) is a **separate, separately-billed resource** from
the server: deleting only the server would **leak the disk**. The provider's
`Delete` stops the server, then calls `DeleteServerAndStorages`, removing both in
one shot. It is idempotent — an already-gone server (404) is treated as success.
If you ever delete servers by hand, use the equivalent "delete with storages" path
so you don't accumulate orphaned disks.

## UpCloud API errors

| Symptom | Cause | Fix |
|---|---|---|
| `api_calls_total{outcome="error"}` rising; logs show 401/403 | Bad, disabled, or under-scoped API sub-account. | Verify `UPCLOUD_USERNAME`/`UPCLOUD_PASSWORD` are current and the sub-account has API + server/storage permission; rotate if needed. |
| Errors spike under load | UpCloud API rate limiting / transient errors. | Back off; reduce churn; the kit retries idempotently. Spread creates if you run many zones off one account. |
| `pricing: no pinned price for plan; reporting 0` | The offered plan isn't in the pinned EUR table and has no override. | Add the plan to the pinned table **or** set `price_usd_per_hour` on the offering. A `0` price is valid but skews the engine's cost ranking. |

## Placement / packing looks wrong

| Symptom | Cause | Fix |
|---|---|---|
| Pods won't schedule on a plan that should fit | `resources` set to the plan's hardware total, forcing density = 1. | `resources` is the **per-replica** request (e.g. `{cpu:"1"}`); leave `allocatable` to the provider (derived from the plan via the Plans API). See [Configuration](configuration.md). |
| `allocatable` is `0`/`nil` for a plan | The plan is neither in the pinned table nor resolvable from the Plans API. | Confirm the plan name is exactly an UpCloud plan; add it to the pinned table if UpCloud can't return it. A nil `allocatable` is treated as `allocatable == resources`. |
| `topology.kubernetes.io/zone` selectors don't match | Zone not surfaced as `zone`. | The provider sets `zone` from the server's zone automatically; confirm the offering's `zone` matches the one you select on (and this process's `--zone`). |

## Fencing alerts

A spike of `FailedPrecondition` on
`bigfleet_upcloud_grpc_requests_total{code="FailedPrecondition"}` means a **zombie
shard** (an old shard process) is being correctly rejected. This is the provider
doing its job — investigate the shard side (a restart that didn't take over
cleanly), not the provider. `FailedPrecondition` is reserved for fencing; any other
rejection uses a different code.

## Useful commands

```sh
# What state is a machine in, and why?
grpcurl -plaintext -d '{"id":"<machine-id>"}' localhost:9000 \
  bigfleet.v1alpha1.CapacityProvider/Get

# Inventory by state.
grpcurl -plaintext -d '{}' localhost:9000 \
  bigfleet.v1alpha1.CapacityProvider/List

# Probes and metrics.
curl localhost:9090/healthz
curl localhost:9090/readyz
curl -s localhost:9090/metrics | grep bigfleet_upcloud_
```
