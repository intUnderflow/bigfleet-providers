---
title: Troubleshooting
description: Common failure modes for the BigFleet DigitalOcean provider and how to diagnose them from logs, metrics, and Get/List.
sidebar:
  order: 7
  label: Troubleshooting
---

Most problems show up as a machine landing in `FAILED` (read `last_error` via
`Get`) or a DigitalOcean API error in the logs/metrics. Work from those two
signals.

## The provider won't start

| Symptom | Cause | Fix |
|---|---|---|
| `--image is required for the digitalocean backend` | Real backend with no base image. | Pass `--image` (e.g. `ubuntu-24-04-x64`). |
| `--region is required for the digitalocean backend` | Real backend with no region. | Set `--region` (e.g. `nyc3`). |
| `token is required for the digitalocean backend` | `--do-backend=digitalocean` (or token+region-implied auto) without a token. | Set `--token` or `DIGITALOCEAN_TOKEN`, or use `--do-backend=fake` for dev. |
| `the digitalocean backend requires --bootstrap-addr, --bootstrap-tls-cert and --bootstrap-tls-key` | Real backend with no bootstrap channel. | Provide the bootstrap channel flags (the blob is a join secret and must travel over TLS). |
| `--bootstrap-endpoint is required` | No externally-reachable URL for the agent. | Set `--bootstrap-endpoint` to the URL the Droplets can reach. |
| `both --tls-cert and --tls-key are required` / `--tls-ca set without --tls-cert/--tls-key` | Half-configured gRPC TLS. | Provide cert **and** key (and a CA for mTLS), or none. |
| Comes up on the **fake** backend unexpectedly (log: "using the IN-MEMORY fake DigitalOcean backend") | No token **or** no region set, so `auto` resolved to `fake`. | Set both a token **and** `--region` to opt into the real backend. |
| `capacity_type ... is not offered by DigitalOcean (on-demand Droplets only)` | A `spot` `capacity_type` in offerings. | Remove it — DigitalOcean has no spot tier. |

## A machine reaches FAILED

`Get` the machine and read `last_error`:

| `last_error` mentions | Cause | Fix |
|---|---|---|
| `create droplet …` | `Droplets.Create` failed (bad size/image/region, account limit, token). | Check the size/image/region exist; check the account's Droplet limit; verify the token has read + write on Droplets. |
| `droplet … did not become active within …` | Create timed out waiting for the Droplet to reach `active`. | Check DigitalOcean status; the size/region may be temporarily out of capacity; the machine goes `FAILED` with this `last_error`. |
| `agent did not apply configure …` / `agent did not apply drain …` | The on-host agent never fetched/acked over the bootstrap channel → Configure/Drain timed out. | Confirm the Droplet can reach `--bootstrap-endpoint`; confirm the image ships and starts the agent; check the agent pins the right CA and presents its per-machine token. |
| `agent bootstrap failed for …` | The agent fetched the blob but applying it (the cluster join) failed. | Inspect the agent on the image; confirm it consumes the opaque blob and joins the cluster; the agent reports the error back in its ack. |
| `droplet … carries no machine id tag` | Configure/Drain against a Droplet missing its machine-id tag (manual edit, or an adopted orphan). | Don't hand-edit BigFleet tags; let the provider manage them. |
| `transition interrupted by a provider restart` | The process was killed mid-transition. | Expected after a kill without graceful drain; the shard re-drives on a fresh slot. Enable `--state` so fence marks/bindings survive. |

A `FAILED` machine is terminal-pending-cleanup: the shard recovers on a different
slot, never in place. Don't re-issue mutations against it.

## DigitalOcean API errors

| Symptom | Cause | Fix |
|---|---|---|
| `api_calls_total{outcome="error"}` rising; logs show 401/403 | Bad, revoked, or under-scoped PAT (an UNAUTHENTICATED-class error). | Verify `DIGITALOCEAN_TOKEN` is current and scoped to read + write on Droplets; rotate if expired. |
| Errors spike under load; logs show 429 | DigitalOcean API **rate limiting**. | Back off; reduce churn; the kit retries idempotently — transient 429s usually clear. Spread creates if you run many regions off one account. |
| `no pricing for size "…"` in price-refresh logs | The offered size isn't in `Sizes.List` for the account. | Confirm the size slug exists and is available to the account; the pinned table covers it as a fallback. |

## Placement / packing looks wrong

| Symptom | Cause | Fix |
|---|---|---|
| Pods won't schedule on a size that should fit | `resources` set to the hardware total, forcing density = 1. | `resources` is the **per-replica** request (e.g. `{cpu:"1"}`); leave `allocatable` to the provider (derived from the size). See [Configuration](configuration.md). |
| `topology.kubernetes.io/zone` selectors don't match | Region not surfaced as `zone`. | The provider sets `zone` from the Droplet's region automatically; confirm the offering's `region` is the one you select on. |

## Cost ranking looks off

| Symptom | Cause | Fix |
|---|---|---|
| Prices are the pinned fallback, not live | Pricing not refreshed (cold cache, or `--price-refresh 0`). | Check `bigfleet_digitalocean_price_refresh_total{outcome="error"}`; ensure the token can read `Sizes.List`; leave `--price-refresh` non-zero. |

## Fencing alerts

A spike of `FailedPrecondition` on
`bigfleet_digitalocean_grpc_requests_total{code="FailedPrecondition"}` means a
**zombie shard** (an old shard process) is being correctly rejected. This is the
provider doing its job — investigate the shard side (a restart that didn't take
over cleanly), not the provider. `FailedPrecondition` is reserved for fencing;
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
curl -s localhost:9090/metrics | grep bigfleet_digitalocean_
```
