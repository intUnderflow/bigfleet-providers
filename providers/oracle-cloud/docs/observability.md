---
title: Observability
description: Prometheus metrics, health/readiness probes, and structured logs exposed by the BigFleet OCI provider.
sidebar:
  order: 5
  label: Observability
---

The provider exposes Prometheus metrics and Kubernetes probes on the
`--metrics-addr` HTTP port (`:9090` by default), and structured logs on stderr.

## Endpoints

- `GET /metrics` — Prometheus exposition (isolated registry, not the global default).
- `GET /healthz` — liveness; always `200 ok` while the process is up.
- `GET /readyz` — readiness; `200 ready` once serving, `503` during shutdown.

The gRPC listener (`--addr`) also serves the standard `grpc.health.v1` health
service and (with `--reflection`) server reflection for `grpcurl`.

## Metrics

All metrics are namespaced `bigfleet_oci_*`:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `bigfleet_oci_api_calls_total` | counter | `op`, `outcome` | OCI Compute API calls by operation and success/error. |
| `bigfleet_oci_api_duration_seconds` | histogram | `op` | OCI Compute API call latency. |
| `bigfleet_oci_grpc_requests_total` | counter | `method`, `code` | CapacityProvider gRPC requests by method and gRPC status. |
| `bigfleet_oci_grpc_request_duration_seconds` | histogram | `method` | gRPC request latency by method. |
| `bigfleet_oci_panics_total` | counter | — | Recovered panics in gRPC handlers. |
| `bigfleet_oci_reconcile_total` | counter | `outcome` | Background OCI→inventory reconcile runs. |

Plus the standard Go runtime and process collectors.

The `op` label on the API metrics covers `LaunchInstance`, `TerminateInstance`,
`DescribeManaged`, `Configure` (Run Command bootstrap), and `Drain`.

## What to watch

- **`bigfleet_oci_grpc_requests_total{code!="OK",code!="FailedPrecondition"}`** —
  real RPC errors. `FailedPrecondition` is normal: it is the kit's fencing
  rejection of a stale shard, not a fault.
- **`bigfleet_oci_api_calls_total{outcome="error"}`** — OCI API failures (quota,
  permissions, throttling).
- **`bigfleet_oci_reconcile_total{outcome="error"}`** — the background reconcile
  can't read OCI truth; inventory may drift until it recovers.
- **gRPC latency histograms** — Create/Configure/Drain are asynchronous (they ack
  immediately), so their gRPC latency stays low; the real work shows up via `Get`
  reaching the target state and in the OCI API histograms.

The Helm Service carries `prometheus.io/scrape` annotations for the metrics port.
