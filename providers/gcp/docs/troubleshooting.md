---
title: Troubleshooting
description: Common failure modes for the BigFleet GCP provider and how to diagnose them from logs, metrics, and Get/List.
sidebar:
  order: 7
  label: Troubleshooting
---

Most problems show up as a machine landing in `FAILED` (read `last_error` via
`Get`) or a GCE API error in the logs/metrics. Work from those two signals.

## The provider won't start

| Symptom | Cause | Fix |
|---|---|---|
| `--project is required for the gcp backend` | Real backend with no project. | Pass `--project` (and `--region`). |
| `--region is required for the gcp backend` | `--gcp-backend=gcp` (or region-implied auto) without a region. | Set `--region`, or use `--gcp-backend=fake` for dev. |
| `both --tls-cert and --tls-key are required` / `--tls-ca set without --tls-cert/--tls-key` | Half-configured TLS. | Provide cert **and** key (and a CA for mTLS), or none. |
| Comes up on the **fake** backend unexpectedly (log: "using the IN-MEMORY fake GCE backend") | No region set, so `auto` resolved to `fake`. | Set `--project`/`--region` to opt into the real backend. |
| `capacity_type "bare_metal" is not a GCE substrate` | A `bare_metal` `capacity_type` in offerings. | Use `on_demand`, `spot`, or `reserved` — GCE creates VMs. |
| `could not find default credentials` | ADC not configured. | On GKE enable Workload Identity and set `serviceAccount.gcpServiceAccount`; off-GKE mount a key as `GOOGLE_APPLICATION_CREDENTIALS`. See [Credentials](/providers/gcp/credentials/). |

## A machine reaches FAILED

`Get` the machine and read `last_error`:

| `last_error` mentions | Cause | Fix |
|---|---|---|
| `insert instance …` | `Instances.Insert` failed (bad type/image/zone, quota, permission). | Check the machine type/image exist in the zone; check the project's quota; verify the provider SA has `compute.instanceAdmin.v1` (and `serviceAccountUser` on the node SA). |
| `configure: SSH delivery disabled (set --ssh-key)` | Configure with no SSH key configured. | Set `--ssh-key` (and ensure its public key is authorised — the provider does this via `ssh-keys` metadata at Create). |
| `configure: … ssh dial/handshake/command …` | The provider can't reach the host on port 22, the host key didn't verify, or the bootstrap hook exited non-zero. | Check network reachability to the instance IP (same VPC, or `--use-external-ip`); confirm the image ships the hook at `--bootstrap-hook` and authorises `--ssh-user`; a host-key mismatch aborts as a possible MITM. |
| `drain: … ssh …` | Drain couldn't cordon/drain over SSH. | Same reachability/SSH checks; confirm `kubectl` is present on the node and the node name resolves. |
| `configure: record cluster binding …` / `drain: clear cluster binding …` | The post-SSH `SetMetadata` to record/clear the binding failed. | Verify `compute.instances.setMetadata` permission; the instance may have been deleted out-of-band (reconcile recovers the slot). |
| `transition interrupted by a provider restart` | The process was killed mid-transition. | Expected after a kill without graceful drain; the shard re-drives on a fresh slot. Enable `--state` so fence marks/bindings survive. |

A `FAILED` machine is terminal-pending-cleanup: the shard recovers on a different
slot, never in place. Don't re-issue mutations against it.

## Placement / packing looks wrong

| Symptom | Cause | Fix |
|---|---|---|
| Pods won't schedule on a machine type that should fit | `resources` set to the hardware total, forcing density = 1. | `resources` is the **per-replica** request (e.g. `{cpu:"2"}`); leave `allocatable` to the provider (derived from the machine type). See [Configuration](/providers/gcp/configuration/). |
| `topology.kubernetes.io/zone` selectors don't match | Zone not surfaced. | The provider sets `zone` from the GCE zone automatically; confirm the offering's `zone` is the one you select on. |
| Accelerator selectors don't match | Missing accelerator label. | `a2*`/`a3*`/`g2*` types get a `bigfleet.io/accelerator` label automatically; for other constraints add a `labels` entry in the offering. |

## Cost ranking looks off

| Symptom | Cause | Fix |
|---|---|---|
| Spot looks as expensive as on-demand | Spot price is a fixed fraction of on-demand in the pinned table. | The fraction (`0.4`) is conservative; pin real Spot rates if you need precision. See [Pricing](/providers/gcp/pricing-and-interruption/). |
| Prices look stale / wrong region | Pinned table has no entry for the region. | The provider falls back to the `us-central1` baseline; pin a per-region table and add it to `onDemandByRegion`. |
| A Spot machine shows `interruption_probability = 0` | Would be a bug — the kit rejects it at startup. | If you see it, file it; the provider declares a non-zero forecast for every Spot family. |

## Fencing alerts

A spike of `FailedPrecondition` on `bigfleet_gcp_grpc_requests_total{code="FailedPrecondition"}`
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
curl -s localhost:9090/metrics | grep bigfleet_gcp_
```
