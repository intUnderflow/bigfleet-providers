---
title: Troubleshooting
description: Common failure modes for the BigFleet Hetzner Cloud provider and how to diagnose them from logs, metrics, and Get/List.
sidebar:
  order: 7
  label: Troubleshooting
---

Most problems show up as a machine landing in `FAILED` (read `last_error` via
`Get`) or a Hetzner API error in the logs/metrics. Work from those two signals.

## The provider won't start

| Symptom | Cause | Fix |
|---|---|---|
| `--image is required for the hetzner backend` | Real backend with no base image. | Pass `--image` (e.g. `ubuntu-24.04`). |
| `token is required for the hetzner backend` | `--hetzner-backend=hetzner` (or token-implied auto) without a token. | Set `--token` or `HCLOUD_TOKEN`, or use `--hetzner-backend=fake` for dev. |
| `both --tls-cert and --tls-key are required` / `--tls-ca set without --tls-cert/--tls-key` | Half-configured TLS. | Provide cert **and** key (and a CA for mTLS), or none. |
| Comes up on the **fake** backend unexpectedly (log: "using the IN-MEMORY fake Hetzner backend") | No token set, so `auto` resolved to `fake`. | Set a token to opt into the real backend. |
| `offering ... is not offered by Hetzner Cloud (on-demand only)` | A `spot` `capacity_type` in offerings. | Remove it — Hetzner Cloud has no spot tier. |

## A machine reaches FAILED

`Get` the machine and read `last_error`:

| `last_error` mentions | Cause | Fix |
|---|---|---|
| `create server …` | `Server.Create` failed (bad type/image/location, quota, token). | Check the type/image/location exist in the project; check the project's server limit; verify the token has Read & Write. |
| `configure: SSH delivery disabled (set --ssh-key)` | Configure with no SSH key configured. | Set `--ssh-key` (and authorise the public key in the image). |
| `ssh dial …` / `ssh handshake …` | The provider can't reach the server on port 22, or the key/user is wrong. | Check the image authorises `--ssh-key` for `--ssh-user`; check the network path to the server's public IP and any firewall. |
| `ssh command on … failed` | The bootstrap hook (or `kubectl drain`) exited non-zero. | Inspect the hook on the image; confirm it consumes `<hook>.blob` and joins the cluster; for drain, confirm `kubectl` is present and the node name resolves. |
| `transition interrupted by a provider restart` | The process was killed mid-transition. | Expected after a kill without graceful drain; the shard re-drives on a fresh slot. Enable `--state` so fence marks/bindings survive. |

A `FAILED` machine is terminal-pending-cleanup: the shard recovers on a different
slot, never in place. Don't re-issue mutations against it.

## Placement / packing looks wrong

| Symptom | Cause | Fix |
|---|---|---|
| Pods won't schedule on a server type that should fit | `resources` set to the hardware total, forcing density = 1. | `resources` is the **per-replica** request (e.g. `{cpu:"1"}`); leave `allocatable` to the provider (derived from the server type). See [Configuration](/providers/hetzner/configuration/). |
| `topology.kubernetes.io/zone` selectors don't match | Location not surfaced as `zone`. | The provider sets `zone` from the Hetzner location automatically; confirm the offering's `location` is the one you select on. |
| Arm workloads land on amd64 (or vice versa) | Missing arch label. | `cax*` types get `kubernetes.io/arch=arm64` automatically; for other arch constraints add a `labels` entry in the offering. |

## Cost ranking looks off

| Symptom | Cause | Fix |
|---|---|---|
| Prices look too high/low | `--eur-usd` stale or wrong. | Pin a current EUR→USD rate (`--eur-usd`). |
| Prices are the pinned fallback, not live | Pricing API not refreshed (cold cache, or `--price-refresh 0`). | Check `bigfleet_hetzner_price_refresh_total{outcome="error"}`; ensure the token can read server types; leave `--price-refresh` non-zero. |

## Fencing alerts

A spike of `FailedPrecondition` on `bigfleet_hetzner_grpc_requests_total{code="FailedPrecondition"}`
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
curl -s localhost:9090/metrics | grep bigfleet_hetzner_
```
