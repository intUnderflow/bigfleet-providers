---
title: Troubleshooting
description: A runbook for the Proxmox VE provider — diagnosing stuck/failed machines, guest-agent, token/TLS, template, and readiness problems from metrics, logs, and Get.
sidebar:
  order: 7
  label: Troubleshooting
---

This is a runbook: a symptom, then the three places you look — the
`bigfleet_proxmox_*` metrics on `--metrics-addr`, the structured logs on stderr,
and a `Get` against the machine — and the fix.

Keep these handy. The provider logs structured key/value lines (`method`, `code`,
`dur_ms`, `err`, …); grep them. The metrics live at `/metrics`, and the Proxmox
API call counters are the fastest way to see *which* operation is unhappy:

```sh
# What's the provider doing right now?
curl -s localhost:9090/metrics | grep -E 'bigfleet_proxmox_(api_calls|grpc_requests|reconcile|panics)_total'

# gRPC error rate, by method and code:
curl -s localhost:9090/metrics | grep bigfleet_proxmox_grpc_requests_total

# Proxmox API errors, by operation:
curl -s localhost:9090/metrics | grep 'bigfleet_proxmox_api_calls_total' | grep 'outcome="error"'
```

`op` on the API counters is the *logical* operation: `CloneVM` (Create),
`DeleteVM` (Delete), `DescribeManaged` (inventory read), `EnsureRunning` (power a
stopped VM on before Configure/Drain), `Configure` (the guest-agent bootstrap),
and `Drain` (the guest-agent kubelet drain). A spike of `outcome="error"` on one
`op` localizes almost every failure below.

## Machines stuck or FAILED

`Create`/`Configure`/`Drain`/`Delete` run async under providerkit transition
timeouts (Create 8m, Configure 8m, Drain 15m, Delete 5m). A machine that exceeds
its timeout, or whose backend call returns an error, lands in `FAILED` rather than
a false Idle/Configured — that is by design. To find *why*, correlate the failing
RPC in the logs with the Proxmox `op` that errored.

```sh
# The last lifecycle RPCs and their gRPC codes:
journalctl -u bigfleet-proxmox | grep '"rpc"' | grep -E 'Create|Configure|Drain|Delete'
```

### Create times out (clone never becomes agent-reachable)

`CloneVM` clones the template, sizes and tags the clone, starts it, and **waits
until the qemu guest agent is reachable** before returning Idle — so the machine
sits in its Create transition until the agent answers. If that exceeds the 8m
Create timeout, the machine goes `FAILED`.

- **Symptom:** `op="CloneVM"` increments but the machine never leaves Creating; a
  log line `wait for guest agent on <node>/<vmid>` then a Create RPC with a
  non-OK `code`.
- **Diagnose:** the usual cause is **the template has no working
  qemu-guest-agent**. Check on a node:
  ```sh
  qm guest cmd <vmid> ping       # works only if the agent is up in the guest
  qm config <vmid> | grep agent  # the VM must have agent enabled
  ```
- **Fix:** the template must ship `qemu-guest-agent` installed **and enabled** and
  start it at boot, and the VM config must have the agent device on. Other causes:
  the clone failed to get an IP / the agent has no time to start (slow storage —
  the clone is a full copy), or the node is out of resources.

### Configure fails (guest-agent bootstrap → FAILED)

`Configure` first powers the VM on if it was stopped (`EnsureRunning`), then writes
the opaque bootstrap blob to `--bootstrap-path` over the guest agent and runs
`--bootstrap-exec <path>`, **waiting for it to exit**. A non-zero exit returns an
error and the machine goes `FAILED`.

- **Symptom:** `op="Configure",outcome="error"`; logs include
  `run bootstrap on guest <node>/<vmid>` and `guest command exited <n>` with the
  hook's stderr tail.
- **Diagnose:** read the exit code and stderr in the log line. A non-zero exit
  means the in-image hook itself failed (wrong join args, kubelet missing, the
  cluster unreachable from the guest).
- **Fix:** the template's hook (run by default as `/bin/sh /run/bigfleet-bootstrap`)
  must consume the blob, join the cluster, and **exit non-zero on failure** — a
  hook that joins the wrong cluster or assumes a tool the template doesn't ship is
  the usual culprit. Confirm `kubelet` is preinstalled in the template.

### Drain times out (Drain → FAILED)

`Drain` powers the VM on if needed, then runs
`kubectl drain "$(hostname)" --ignore-daemonsets --delete-emptydir-data` over the
guest agent, bounded by the grace period. An incomplete drain surfaces as
`FAILED`, never a false Idle.

- **Symptom:** `op="Drain",outcome="error"`; logs `await agent exec` /
  `guest command exited`. Strict PodDisruptionBudgets are the classic cause (hence
  the generous 15m Drain timeout).
- **Diagnose:** on the node / via the guest, check pods blocking eviction and PDBs.
- **Fix:** relax the offending PDB or extend the grace period. Note the drain uses
  the guest's `hostname` as the Kubernetes node name — if your cluster names nodes
  differently, `kubectl drain` can't find the node and fails.

### A VM was stopped out of band

A VM the kit holds Idle may have been stopped (operator power-cycle, an HA event,
a maintenance reboot). The provider's `Describe` leaves a tagged-but-stopped VM's
slot Speculative (so it is not advertised as schedulable), and Configure/Drain
power it back on first (`EnsureRunning`). So an out-of-band stop self-heals; you
should see an `op="EnsureRunning"` call before the next Configure/Drain. If
`EnsureRunning` itself errors, the node/storage is the problem, not the provider.

## Token / TLS: 401 / 403 / cert errors

- **Symptom:** any `op` with `outcome="error"` and a `401`/`403`/permission denial
  in the log; or the provider fails at startup before serving with a TLS error.
- **Diagnose & fix:**
  - **401/403 on `CloneVM` or another op:** the API token's role is not granted on
    the resource pool, or the privilege set is short one entry. Match the denied
    action to the [Credentials](/providers/proxmox/credentials/) privilege table
    and `pveum acl modify /pool/<pool> --roles ... --tokens '...'`. A blanket
    denial on the very first call usually means the ACL was never bound to the
    token.
  - **`read --proxmox-ca-file` at startup:** the CA file path is wrong or
    unreadable in the pod. Confirm the Secret is mounted at the path the chart
    wires.
  - **`server certificate fingerprint does not match`:** the pinned
    `--proxmox-tls-fingerprint` no longer matches the API cert (it was reissued).
    Re-read it (`openssl x509 ... -fingerprint -sha256`) and re-pin, or switch to
    `--proxmox-ca-file`.
  - **chain/hostname verification failure:** the `--proxmox-ca-file` does not sign
    the cert the cluster presents, or you are dialing a hostname not on the cert.
    There is no skip-verify fallback by design — fix the trust material.

## Template / VMID problems

`CloneVM` locates the source template by its VMID across the cluster, then clones
it onto the target node.

- **Symptom:** `op="CloneVM",outcome="error"`; logs `locate template VMID <n>:
  VMID <n> not found in cluster`, or a clone task failure.
- **Diagnose:** confirm the template exists and the token can see it:
  ```sh
  qm list                      # the template VMID should appear
  pvesh get /cluster/resources --type vm   # what the API/token sees
  ```
- **Fix:** point `--template-vmid` (or the per-type `template_vmid` in
  `--instance-types`) at a VMID that exists on the cluster and is readable by the
  token. The template must be in (or readable from) the pool the token is scoped
  to.

## Offering placed on an unknown node

With the real backend every offering must place onto a node listed in `--nodes`,
validated at startup.

- **Symptom:** the process exits at startup with `offering <type> is placed on node
  "<x>", which is not in --nodes (...)`, or `the real Proxmox backend requires
  --nodes`.
- **Fix:** add the node to `--nodes`, or fix the offering's `zone`. The `zone` on
  every offering is a Proxmox node name and must be one of `--nodes`.

## Readiness never goes green

`/readyz` returns `503 not ready` until the server is fully wired and serving;
`/healthz` is liveness only (always `200 ok` once the HTTP server is up). The gRPC
`grpc.health.v1` status flips to `SERVING` at the same point.

- **Symptom:** `curl localhost:9090/readyz` ⇒ `not ready`; the pod never passes
  its readiness probe; no `serving CapacityProvider` log line.
- **Diagnose:** readiness is set only **after** the provider reaches the serving
  point. If the process exits during config load first, you'll see a startup error
  on stderr instead. Common blockers:
  - `--proxmox-backend=proxmox` (or `auto` with `--proxmox-api-url`) but missing
    `--proxmox-token-id` / a token secret / `--nodes`, or no TLS trust material
    (`TLS verification material is required: set --proxmox-ca-file ... or
    --proxmox-tls-fingerprint`).
  - gRPC TLS misconfig: `both --tls-cert and --tls-key are required`, or `--tls-ca
    set without --tls-cert/--tls-key`.
  - `--addr` already in use (`listen on :9000: …`).
  - `no offerings configured`, an offering with empty `instance_type`/`zone`, an
    instance_type not in the catalog, or a non-positive `count`.
- **Fix:** resolve the startup error in the logs. On shutdown (SIGINT/SIGTERM)
  readiness intentionally flips back to `not ready` and gRPC health to
  `NOT_SERVING` before graceful stop — a `not ready` during termination is
  expected, not a fault.

## Panics

`bigfleet_proxmox_panics_total` should stay flat. A recovered panic in a gRPC
handler is converted to `codes.Internal` (the RPC fails, the process survives) and
logged as `recovered panic in gRPC handler` with the method. Any non-zero value is
a bug — capture the log line and the request that triggered it.

## See also

- [Observability](/providers/proxmox/observability/) — the full metric/health/log surface
- [Credentials](/providers/proxmox/credentials/) — the API token privileges and TLS trust
- [Configuration](/providers/proxmox/configuration/) — every flag, backend modes, the clone/bootstrap model
- [Security](/providers/proxmox/security/) — the gRPC mTLS and guest-agent trust model
