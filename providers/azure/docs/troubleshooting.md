---
title: Troubleshooting
description: A runbook for the Azure provider — diagnosing stuck/failed machines, Spot capacity, identity/role, the bootstrap extension, pricing, and readiness from metrics, logs, and Get.
sidebar:
  order: 7
  label: Troubleshooting
---

This is a runbook: a symptom, then the three places you look — the
`bigfleet_azure_*` metrics on `--metrics-addr`, the structured logs on stderr,
and a `Get` against the machine — and the fix.

```sh
# What's the provider doing right now?
curl -s localhost:9090/metrics | grep -E 'bigfleet_azure_(api_calls|grpc_requests|reconcile|price_refresh|spot_evictions|panics)_total'

# gRPC error rate, by method and code:
curl -s localhost:9090/metrics | grep bigfleet_azure_grpc_requests_total

# Azure API errors, by operation:
curl -s localhost:9090/metrics | grep 'bigfleet_azure_api_calls_total' | grep 'outcome="error"'
```

`op` on the Azure counters is the *logical* operation: `CreateVM`, `DeleteVM`,
`ListVMs`, `Configure` (the bootstrap extension), `Drain` (the drain extension),
`RetailPrices`, and `ResourceSkus`. A spike of `outcome="error"` on one `op`
localizes almost every failure below.

## Machines stuck or FAILED

`Configure`/`Drain`/`Create` run async under
[providerkit](/providers/azure/configuration/) transition timeouts (Create 8m,
Configure 8m, Drain 15m, Delete 8m). A machine that exceeds its timeout, or whose
backend call returns an error, lands in `FAILED` rather than a false
Idle/Configured — that is by design. To find *why*, correlate the failing RPC in
the logs with the Azure `op` that errored, and read `last_error` from `Get`.

### Create times out (VM never provisions)

`CreateInstance` creates the NIC then the VM and **blocks on the create poller**
before returning Idle. If that exceeds the kit's 8m Create timeout, the machine
goes `FAILED`.

- **Symptom:** `CreateVM` counter increments, but the machine never leaves
  Creating; a Create RPC with a non-OK `code`.
- **Diagnose:** look at the VM directly — it is usually a quota, image, or subnet
  problem.
  ```sh
  az vm show -g <rg> -n <vm-name> --query 'provisioningState'
  az vm list -g <rg> --query "[?tags.\"bigfleet-managed\"=='true'].[name,provisioningState]" -o table
  ```
- **Fix:** resolve the underlying problem — **regional vCPU quota** for the VM
  family (the most common cause; request an increase), a wrong image URN, or a
  subnet with no free addresses. For Spot, see
  [SkuNotAvailable / eviction](#spot-skunotavailable--evicted) below.

### Quota / throttling (429)

- **Symptom:** `op="CreateVM",outcome="error"` with `OperationNotAllowed`
  (quota) or `429`/`TooManyRequests` (ARM throttling) in the logs; rising
  `bigfleet_azure_api_duration_seconds{op="CreateVM"}`.
- **Diagnose:** check the family quota: `az vm list-usage -l <location> -o table`.
  Concurrent shards scaling at once is the usual throttling cause.
- **Fix:** request a quota increase, spread creates (smaller per-shard offering
  `count`, stagger scale-ups), and confirm the Create timeout comfortably exceeds
  worst-case retry backoff.

### Bootstrap extension failure (Configure → FAILED)

`Configure` runs a CustomScript extension that writes the opaque blob and runs the
`--bootstrap-hook`, then tags `bigfleet-cluster`. A failed extension returns an
error and the machine goes `FAILED`.

- **Symptom:** `op="Configure",outcome="error"`; `last_error` mentions the
  extension. 
- **Diagnose:** read the extension's status:
  ```sh
  az vm extension show -g <rg> --vm-name <vm> -n bigfleet-configure \
    --query 'instanceView.statuses'
  ```
- **Fix:** make the hook robust. The image must ship an executable at
  `--bootstrap-hook` (default `/opt/bigfleet/bootstrap`) that consumes the blob
  file and joins the cluster, and **exits non-zero on failure**. A hook that is
  missing, non-executable, or joins the wrong cluster is the usual culprit. Make
  sure the node has egress to your cluster API and to the CustomScript extension's
  package source.

### Drain times out (Drain → FAILED)

`Drain` runs the hook's `--drain` path (cordon + `kubectl drain`) and clears the
`bigfleet-cluster` tag. An incomplete drain surfaces as `FAILED`, never a false
Idle.

- **Symptom:** `op="Drain",outcome="error"`/`DeadlineExceeded`. Strict
  PodDisruptionBudgets are the classic cause (hence the generous 15m Drain
  timeout).
- **Diagnose:** `kubectl get pods --field-selector spec.nodeName=<node> -A` and
  check PDBs blocking eviction.
- **Fix:** relax the offending PDB or extend the grace period. Ensure the hook's
  drain uses the Kubernetes node name that matches the VM (with the Azure cloud
  provider the node name is typically the VM's computer name).

## Spot: SkuNotAvailable / evicted

Spot VMs are created with `priority=Spot`, `evictionPolicy=Delete`, `maxPrice=-1`.
When the pool is dry, Azure rejects the create; when capacity is reclaimed, the VM
is deleted out from under you.

- **Symptom:** Create FAILs quickly with `SkuNotAvailable` /
  `OverconstrainedAllocationRequest`; or a previously-Idle Spot machine returns to
  Speculative and `bigfleet_azure_spot_evictions_total` increments.
- **Diagnose:** Spot availability is size × zone specific. Cross-check the forecast
  you are already publishing — a higher `interruption_probability` for that size
  predicts exactly this.
- **Fix:** offer **more (size, zone) pairs** so the engine has fallbacks; spread
  across both `--zone-a`/`--zone-b` (and more). The provider does not silently
  fall back to pay-as-you-go — capacity type is a property of the offering slot,
  so diversify offerings rather than expecting automatic substitution.

## Identity / authorization (AuthorizationFailed)

- **Symptom:** any `op` with `outcome="error"` and `AuthorizationFailed` /
  `does not have authorization to perform action` in the logs; or a blanket
  failure on the very first `ListVMs`/`CreateVM`.
- **Diagnose:** match the denied action to the role on
  [Credentials](/providers/azure/credentials/). The provider calls
  `Microsoft.Compute/virtualMachines/*`, `.../extensions/*`, `.../disks/*`,
  `Microsoft.Network/networkInterfaces/*`, the subnet `join/action`, and the
  Resource SKUs read — all scoped to the resource group.
- **Fix:**
  - A blanket `AuthorizationFailed` on the first call usually means the federated
    credential subject doesn't match the ServiceAccount, or the role assignment
    hasn't propagated (give it a minute).
  - A denial on the subnet specifically means the role is missing
    `Microsoft.Network/virtualNetworks/subnets/join/action` — the NIC can't attach.
  - Confirm Workload Identity is wired: the pod must carry the
    `azure.workload.identity/use: "true"` label and the SA the `client-id` +
    `tenant-id` annotations.

## Cold Spot price

On startup (and for a freshly-offered size) the Spot cache is empty. `price` never
blocks on the network on the List hot path, so a cold size reports a
**conservative fallback of `0.4 × pay-as-you-go`** until a refresh fills the
cache.

- **Symptom:** Spot `price_per_hour` in `Get` looks like a round fraction of
  pay-as-you-go right after boot, and
  `bigfleet_azure_price_refresh_total{outcome="error"}` or
  `op="RetailPrices",outcome="error"` is non-zero.
- **Diagnose:** the startup warm-up is best-effort and bounded (20s). A failed
  refresh logs `pricing: spot price fetch failed; keeping fallback` per size. The
  background refresher retries every `--price-refresh` (default 1h).
- **Fix:** usually self-heals on the next refresh. If it persists, check egress to
  `prices.azure.com` and that the (size, region) actually has a Spot consumption
  meter (`no spot consumption meter for <size> in <region>`).

## Region-table mismatch

The pinned on-demand prices and Spot eviction bands ship for `eastus` and
`westeurope`. Spot prices and `allocatable` are **live**; on-demand prices and the
eviction *forecast* are pinned.

- **Symptom:** at startup, `no pinned on-demand price table for this region; cost
  ranking uses baseline approximations` for an untabulated region. On-demand
  `price_per_hour` looks off, or a size you offer reads 0.
- **Fix:** regenerate the on-demand table from the Retail Prices API per
  [Pricing & interruption](/providers/azure/pricing-and-interruption/), and add
  your sizes to `evictionBand`. These feed the engine's *relative* cost ranking,
  so keep them roughly right.

## Readiness never goes green

`/readyz` returns `503 not ready` until the server is serving; `/healthz` is
liveness only. The gRPC `grpc.health.v1` status flips to `SERVING` at the same
point.

- **Symptom:** `curl localhost:9090/readyz` ⇒ `not ready`; no
  `serving CapacityProvider` log line.
- **Diagnose:** readiness is set only **after** `run()` reaches the serving point.
  If the process exits during config load first, you'll see a startup error on
  stderr. Common blockers:
  - `--azure-backend=azure` (or `auto` with `--location`) but missing
    `--subscription-id`, `--resource-group`, or `--subnet-id`.
  - TLS misconfig (`both --tls-cert and --tls-key are required`, or `--tls-ca set
    without --tls-cert/--tls-key`).
  - `--addr` already in use; or `no offerings configured` / an offering with empty
    `vm_size` or `zone`.
- **Fix:** resolve the startup error in the logs. On shutdown (SIGINT/SIGTERM)
  readiness intentionally flips back to `not ready` — that is expected, not a
  fault.

## Panics

`bigfleet_azure_panics_total` should stay flat. A recovered panic is converted to
`codes.Internal` (the RPC fails, the process survives) and logged as
`recovered panic in gRPC handler`. Any non-zero value is a bug — capture the log
line and the request that triggered it.

## See also

- [Observability](/providers/azure/observability/) — the full metric/health/log surface
- [Credentials](/providers/azure/credentials/) — the exact role and Workload Identity wiring
- [Pricing & interruption](/providers/azure/pricing-and-interruption/) — how price and probability are sourced
- [Configuration](/providers/azure/configuration/) — every flag, backend modes, the bootstrap model
