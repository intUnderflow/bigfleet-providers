---
title: Observability
description: Prometheus metrics, health/readiness probes, and structured logs exposed by the BigFleet OVHcloud Public Cloud provider.
sidebar:
  order: 5
  label: Observability
---

The provider exposes everything you need to run it like any other production
service: Prometheus metrics, Kubernetes liveness/readiness probes, and structured
logs. The gRPC service is on `--addr` (`:9000`); metrics and probes are on a
separate HTTP port, `--metrics-addr` (`:9090`).

## Endpoints

| Endpoint | Port | Purpose |
|---|---|---|
| gRPC `CapacityProvider` | `:9000` | The contract BigFleet dials (Create/Configure/Drain/Delete/Get/List). |
| gRPC `grpc.health.v1.Health` | `:9000` | Standard gRPC health service (SERVING once ready). |
| gRPC reflection | `:9000` | For `grpcurl`/debugging (`--reflection`, on by default). |
| `GET /healthz` | `:9090` | Liveness — always `200 ok` while the process runs. |
| `GET /readyz` | `:9090` | Readiness — `200 ready` once serving, `503` during shutdown. |
| `GET /metrics` | `:9090` | Prometheus metrics (isolated registry). |

The Helm chart wires `livenessProbe: /healthz` and `readinessProbe: /readyz` on
the metrics port, and annotates the `Service` with `prometheus.io/scrape`.

## Metrics

All metrics are namespaced `bigfleet_ovh_*` on an isolated registry (not the
global default), plus the standard Go/process collectors.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `bigfleet_ovh_api_calls_total` | counter | `op`, `outcome` | OpenStack API calls by operation (`CreateServer`, `DeleteServer`, `StartServer`, `DescribeManaged`, `Configure`, `Drain`, `Flavors`) and `success`/`error`. |
| `bigfleet_ovh_api_duration_seconds` | histogram | `op` | OpenStack API call latency by operation. |
| `bigfleet_ovh_grpc_requests_total` | counter | `method`, `code` | CapacityProvider gRPC requests by method and gRPC status code. |
| `bigfleet_ovh_grpc_request_duration_seconds` | histogram | `method` | gRPC request latency by method. |
| `bigfleet_ovh_panics_total` | counter | — | Recovered panics in gRPC handlers (should stay 0). |
| `bigfleet_ovh_reconcile_total` | counter | `outcome` | Background OpenStack→inventory reconcile runs by outcome. |

### Useful queries

```
# OpenStack API error rate by operation
sum by (op) (rate(bigfleet_ovh_api_calls_total{outcome="error"}[5m]))

# Fencing rejections (zombie-shard incidents): FAILED_PRECONDITION is fencing-only
sum(rate(bigfleet_ovh_grpc_requests_total{code="FailedPrecondition"}[5m]))

# p99 Create latency (server create + wait-for-ACTIVE)
histogram_quantile(0.99, sum by (le) (rate(bigfleet_ovh_grpc_request_duration_seconds_bucket{method="Create"}[10m])))

# Reconcile failures
sum(rate(bigfleet_ovh_reconcile_total{outcome="error"}[15m]))
```

A `FailedPrecondition` on a mutating RPC is **always** a fencing rejection (the
provider reserves that code for fencing only), so it is a clean signal for a
zombie-shard incident — alert on a sustained non-zero rate.

## Logs

Structured `slog` text on stderr. Every RPC logs one line with `method`, `code`,
and `dur_ms`; lifecycle RPCs (Create/Configure/Drain/Delete) log at INFO,
read-only RPCs at DEBUG, and any error at WARN. The OpenStack password, the SSH
key, and the opaque bootstrap blob are **never** logged.

At startup the provider logs its mode, e.g.:

```
serving CapacityProvider addr=:9000 provider=ovh-public-GRA region=GRA ovh_backend=ovh security=mTLS offerings=24 metrics_addr=:9090
```

If you see `ovh_backend=fake` in production, the provider came up without
`--region`/credentials and is **not** creating real instances — see
[Configuration → Backend modes](/providers/ovhcloud/configuration/#backend-modes).
