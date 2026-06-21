---
title: Troubleshooting
description: Common failure modes for the BigFleet Latitude.sh provider and how to diagnose them from logs, metrics, and Get/List.
sidebar:
  order: 7
  label: Troubleshooting
---

Most problems show up as a machine landing in `FAILED` (read `last_error` via
`Get`) or a Latitude API error in the logs/metrics. Work from those two signals.

## The provider won't start

| Symptom | Cause | Fix |
|---|---|---|
| `--token (or LATITUDESH_API_TOKEN) is required for the latitude backend` | Real backend with no token. | Set `--token` or `LATITUDESH_API_TOKEN`, or use `--latitude-backend=fake` for dev. |
| `--project (or LATITUDESH_PROJECT) is required for the latitude backend` | Token set but no project. | Set `--project` (id or slug) or `LATITUDESH_PROJECT`. |
| `both --tls-cert and --tls-key are required` / `--tls-ca set without --tls-cert/--tls-key` | Half-configured TLS. | Provide cert **and** key (and a CA for mTLS), or none. |
| Comes up on the **fake** backend unexpectedly (log: "using the IN-MEMORY fake Latitude backend") | Token **or** project missing, so `auto` resolved to `fake`. | Set **both** a token and a project to opt into the real backend. |
| `capacity_type "spot" is not offered by Latitude.sh (on-demand only)` | A `spot` `capacity_type` in offerings. | Remove it — Latitude has no spot tier. |
| `capacity_type "bare_metal" would suppress the shard's Delete (M73) and leak servers` | A `bare_metal`/`reserved` `capacity_type` in offerings. | Use `on_demand` — Latitude has a real Delete, so the capacity type is ON_DEMAND. See [Configuration](/providers/latitude/configuration/#why-on_demand-not-bare_metal). |

## A machine reaches FAILED

`Get` the machine and read `last_error`:

| `last_error` mentions | Cause | Fix |
|---|---|---|
| `deploy server …` / `create server …` | `Servers.Create` failed (bad plan/OS/site, no stock, token/project). | Check the plan/OS/site exist and have stock in the project; verify the token has full project access and the project id/slug is right. |
| `did not power on within …` | The bare-metal deploy (or an out-of-band power-on) never reached `on`. | Check the server in the Latitude dashboard; a stuck or failed deployment may need manual teardown. The Create timeout is 30m for this reason. |
| `configure: SSH delivery disabled (set --ssh-key)` | Configure with no SSH key configured. | Set `--ssh-key` (the provider registers + authorises the matching public key for you). |
| `ssh dial …` / `ssh handshake …` | The provider can't reach the server on port 22, or the key/user is wrong. | Check the network path to the server's public IPv4 and any firewall; check `--ssh-user`. |
| `host key mismatch: pinned … presented …` | The presented SSH host key does not match the pin (possible MITM, or the server was rebuilt out-of-band). | Investigate the network path; if the box was legitimately rebuilt, deprovision it so a fresh deploy re-pins. |
| `ssh command on … failed` | The bootstrap hook (or `kubectl drain`) exited non-zero. | Inspect the hook on the OS image; confirm it consumes `<hook>.blob` and joins the cluster; for drain, confirm `kubectl` is present and the node name resolves. |
| `transition interrupted by a provider restart` | The process was killed mid-transition. | Expected after a kill without graceful drain; the shard re-drives on a fresh slot. Enable `--state` so fence marks/bindings survive. |

A `FAILED` machine is terminal-pending-cleanup: the shard recovers on a different
slot, never in place. Don't re-issue mutations against it.

## A server is powered off

A tagged server tracked as Idle/bound may be powered off out-of-band. This is not
itself a failure: **Configure and Drain both power the server on and wait for
reachability** (EnsureRunning) before delivering the bootstrap or draining.
`Describe` does **not** power servers on — a tagged-but-stopped server stays Idle
and reapable in the inventory, owning its slot. If a Configure/Drain is slow,
check `bigfleet_latitude_api_calls_total{op="PowerOn"}` and the
`server is powered off; powering on before bootstrap/drain` log line.

## Placement / packing looks wrong

| Symptom | Cause | Fix |
|---|---|---|
| Pods won't schedule on a plan that should fit | `resources` set to the plan's hardware total, forcing density = 1. | `resources` is the **per-replica** request (e.g. `{cpu:"1"}`); leave `allocatable` to the provider (derived from the plan). On bare metal the density gap is the whole point. See [Configuration](/providers/latitude/configuration/). |
| `topology.kubernetes.io/zone` selectors don't match | Site not surfaced as `zone`. | The provider sets `zone` from the Latitude site automatically; confirm the offering's `site` is the one you select on. |
| GPU workloads don't match an accelerator selector | Missing accelerator label. | GPU plan families (`*h100*`/`*l40s*`/`*a100*`/`g3*`/`g4*`) get a `bigfleet.io/accelerator` label automatically; for other constraints add a `labels` entry in the offering. |

## Cost ranking looks off

| Symptom | Cause | Fix |
|---|---|---|
| Prices are the pinned fallback, not live | Pricing API not refreshed (cold cache, or `--price-refresh 0`). | Check `bigfleet_latitude_price_refresh_total{outcome="error"}`; ensure the token can read plans; leave `--price-refresh` non-zero. |
| A plan shows no price | Plan not in the pinned table and not resolved from the Plans API. | Confirm the plan slug is exactly right; the Plans API refresh covers offered plans, the pinned table covers common ones. |

## Fencing alerts

A spike of `FailedPrecondition` on `bigfleet_latitude_grpc_requests_total{code="FailedPrecondition"}`
means a **zombie shard** (an old shard process) is being correctly rejected. This
is the provider doing its job — investigate the shard side (a restart that didn't
take over cleanly), not the provider. `FailedPrecondition` is reserved for
fencing; any other rejection uses a different code.

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
curl -s localhost:9090/metrics | grep bigfleet_latitude_
```
