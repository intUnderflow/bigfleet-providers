---
title: Troubleshooting
description: Common failure modes for the BigFleet Scaleway provider and how to diagnose them from logs, metrics, and Get/List.
sidebar:
  order: 7
  label: Troubleshooting
---

Most problems show up as a machine landing in `FAILED` (read `last_error` via
`Get`) or a Scaleway API error in the logs/metrics. Work from those two signals.

## The provider won't start

| Symptom | Cause | Fix |
|---|---|---|
| `--image is required for the scaleway backend` | Real backend with no base image. | Pass `--image` (e.g. `ubuntu_jammy`). |
| `requires --bootstrap-addr, --bootstrap-tls-cert and --bootstrap-tls-key` | Real backend without the bootstrap channel configured (the blob is a join secret and must travel over TLS). | Pass `--bootstrap-addr` plus the channel `--bootstrap-tls-cert`/`--bootstrap-tls-key`. |
| `--bootstrap-endpoint is required` | Real backend with no externally-reachable channel URL to inject into `user_data`. | Set `--bootstrap-endpoint` to a URL the on-host agent can dial (SAN matching the channel cert). |
| credentials required for the scaleway backend | `--scaleway-backend=scaleway` (or credential-implied auto) without a complete access/secret key + project id. | Set `SCW_ACCESS_KEY` / `SCW_SECRET_KEY` / `SCW_DEFAULT_PROJECT_ID` (or the flags), or use `--scaleway-backend=fake` for dev. |
| `both --tls-cert and --tls-key are required` / `--tls-ca set without --tls-cert/--tls-key` | Half-configured TLS. | Provide cert **and** key (and a CA for mTLS), or none. |
| Comes up on the **fake** backend unexpectedly (log: "using the IN-MEMORY fake Scaleway backend") | Incomplete credentials, so `auto` resolved to `fake`. | Set all three `SCW_*` values to opt into the real backend. |
| `capacity_type "spot" is not offered by Scaleway` | A `spot` `capacity_type` in offerings. | Remove it — Scaleway has no spot/preemptible market. |
| offering's `capacity_type` doesn't match `--substrate` | `on_demand` offering on an `elastic-metal` process (or vice versa). | Make the offering's `capacity_type` match the substrate (`on_demand` for `instances`, `bare_metal` for `elastic-metal`). |

## A machine reaches FAILED

`Get` the machine and read `last_error`:

| `last_error` mentions | Cause | Fix |
|---|---|---|
| `create server …` | `CreateServer` failed (bad type/image/zone, quota, key). | Check the type/image/zone exist in the project; check the project's quota; verify the API key has the right permission set. |
| `agent did not apply configure for … context deadline exceeded` | Configure blocked on the agent's ack and the Configure transition timed out — the agent never dialled the channel, never received its command, or never acked. | Confirm the image installs and starts the agent from `--base-user-data`; confirm the agent can reach `--bootstrap-endpoint` from the node (the agent dials the provider — check the network path and that the endpoint URL/port is reachable); confirm `--bootstrap-addr` is actually serving. |
| agent can't verify the provider / CA pin mismatch / TLS handshake failed | The agent pins the provider CA (`--bootstrap-ca`, the server cert by default) and the channel's served cert doesn't chain to it, or its SAN doesn't match `--bootstrap-endpoint`. | Use a bootstrap TLS cert whose SAN matches `--bootstrap-endpoint`; make sure the CA the agent pins matches the served cert (set `--bootstrap-ca`, or let it default to the server cert). |
| agent gets `401 unauthorized` from the channel / token rejected | The agent's per-machine bearer token doesn't match `base64(HMAC-SHA256(--bootstrap-secret, machine_id))` — usually `--bootstrap-secret` changed (or was the random default) and the provider restarted after the token was baked into `user_data`. | Pin `--bootstrap-secret` (or `BIGFLEET_BOOTSTRAP_SECRET`) so tokens survive restarts; if it was unset the provider logs a warning at startup. A machine baked before the secret changed must be recreated. |
| `agent bootstrap failed for …` | The agent acked a **failure** — its join (or, for drain, `kubectl drain`) exited non-zero. | Inspect the agent on the image; confirm it consumes the blob and joins the cluster; for drain, confirm `kubectl` is present and the node name resolves. |
| `transition interrupted by a provider restart` | The process was killed mid-transition. | Expected after a kill without graceful drain; the shard re-drives on a fresh slot. Enable `--state` so fence marks/bindings survive. |

A `FAILED` machine is terminal-pending-cleanup: the shard recovers on a different
slot, never in place. Don't re-issue mutations against it.

## Quota and slow commissioning

| Symptom | Cause | Fix |
|---|---|---|
| `create server …` mentions quota/limit | The project's Instances or Elastic Metal quota is exhausted. | Request a quota increase in the Scaleway console, or lower the offering `count`. |
| Elastic Metal Creates sit in transition for a long time | Physical commissioning (`CreateServer` + install) takes tens of minutes to hours. | This is expected — the Elastic Metal Create timeout is 2h (vs 5m for Instances). Watch the `bigfleet_scaleway_api_duration_seconds{op="CreateServer"}` tail; don't lower the timeout. |

## Placement / packing looks wrong

| Symptom | Cause | Fix |
|---|---|---|
| Pods won't schedule on a type that should fit | `resources` set to the hardware total, forcing density = 1. | `resources` is the **per-replica** request (e.g. `{cpu:"1"}`); leave `allocatable` to the provider (derived from the commercial type). See [Configuration](/providers/scaleway/configuration/). |
| `topology.kubernetes.io/zone` selectors don't match | Zone not surfaced. | The provider sets `zone` from the offering's `zone` automatically; confirm the offering's `zone` is the one you select on. |
| Arm/GPU workloads land on the wrong hardware | Missing arch/accelerator label. | `COPARM1-*` types get `kubernetes.io/arch=arm64` and `RENDER-*`/`H100-*`/`L4-*` get an accelerator label automatically; for other constraints add a `labels` entry in the offering. |

## Cost ranking looks off

| Symptom | Cause | Fix |
|---|---|---|
| Prices look too high/low | `--eur-usd` stale or wrong. | Pin a current EUR→USD rate (`--eur-usd`). |
| Prices are the pinned fallback, not live | Catalogue not refreshed (cold cache, or `--price-refresh 0`). | Check `bigfleet_scaleway_price_refresh_total{outcome="error"}` and the "price fetch failed; keeping fallback" WARN logs; ensure the key can read the catalogue; leave `--price-refresh` non-zero. |
| Elastic Metal prices show `0` | Expected. | Elastic Metal is owned hardware — `price_per_hour = 0`. See [Pricing](/providers/scaleway/pricing-and-interruption/). |

## Fencing alerts

A spike of `FailedPrecondition` on `bigfleet_scaleway_grpc_requests_total{code="FailedPrecondition"}`
means a **zombie shard** (an old shard process) is being correctly rejected. This
is the provider doing its job — investigate the shard side (a restart that didn't
take over cleanly), not the provider. `FailedPrecondition` is reserved for fencing;
any other rejection uses a different code.

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
curl -s localhost:9090/metrics | grep bigfleet_scaleway_
```
