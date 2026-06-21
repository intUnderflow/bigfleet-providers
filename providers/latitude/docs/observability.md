---
title: Observability
description: The Latitude.sh provider's metrics catalogue, health vs readiness, the logging model, and a sample Prometheus scrape.
sidebar:
  order: 6
  label: Observability
---

The Latitude.sh provider is built to be operated from its signals. Every
Latitude.sh API call, every gRPC request, and every background loop is
instrumented; liveness and readiness are separate probes; and requests are logged
through a panic-recovering interceptor chain.

Observability lives on a **separate HTTP port** from the gRPC server. The gRPC
contract (`CapacityProvider` + `grpc.health.v1` health + reflection) is served on
`--addr` (`:9000`); `/metrics`, `/healthz`, and `/readyz` are served on
`--metrics-addr` (`:9090`). Set `--metrics-addr ""` to disable the HTTP server
entirely (the gRPC health service stays up regardless).

## Metrics catalogue

Metrics are registered on an **isolated Prometheus registry** (not the global
default), exposed at `GET /metrics` on `--metrics-addr`. Every series is prefixed
`bigfleet_latitude_`. The Go runtime and process collectors are also registered,
so you get `go_*` and `process_*` for free.

### Latitude.sh API

The Latitude client is wrapped by a transparent metrics decorator, so every API
call the provider makes is counted and timed.

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_latitude_api_calls_total` | counter | `op`, `outcome` | Latitude API call volume, split by operation (`CreateServer`, `DeleteServer`, `DescribeManaged`, `GetServer`, `PowerOn`, `Configure`, `Drain`, `PlanPricing`, `Plans`) and `success`/`error`. The first place to look when deploys or drains are failing. |
| `bigfleet_latitude_api_duration_seconds` | histogram | `op` | Latitude API latency by operation. |

### gRPC

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_latitude_grpc_requests_total` | counter | `method`, `code` | CapacityProvider RPCs by method and gRPC status code. A spike of `FailedPrecondition` means a **zombie shard** is being fenced — alert on it. |
| `bigfleet_latitude_grpc_request_duration_seconds` | histogram | `method` | Per-RPC latency. The mutating RPCs are async, so these should stay sub-millisecond. |
| `bigfleet_latitude_panics_total` | counter | — | Recovered panics in gRPC handlers (should stay 0). |

### Background loops

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_latitude_reconcile_total` | counter | `outcome` | Background Latitude→inventory reconcile runs by outcome. |
| `bigfleet_latitude_price_refresh_total` | counter | `outcome` | Background price-refresh runs by outcome. |

## Health vs readiness

Two distinct probes, served on `--metrics-addr`:

- **`/healthz` (liveness)** — always `200 ok` once the HTTP server is up. Wire it
  to a liveness probe; a failure means the process is wedged and should be
  restarted.
- **`/readyz` (readiness)** — `200 ready` only after the gRPC server is serving;
  `503 not ready` during startup and shutdown. Wire it to a readiness probe so
  BigFleet only dials the Service once the provider can serve. On `SIGTERM` it
  flips to `not ready` first, so traffic drains before the gRPC server stops.

The standard gRPC health service (`grpc.health.v1`) is also registered on
`--addr`, reporting `SERVING`/`NOT_SERVING` for gRPC-native health checking.

## Logging

Structured `slog` text on stderr. Every RPC is logged through the interceptor
chain with its method, gRPC code, and duration; the mutating RPCs
(`Create`/`Configure`/`Drain`/`Delete`) log at `INFO`, reads at `DEBUG`, and any
error at `WARN`. **Neither the API token nor the opaque bootstrap blob is ever
logged.** A panic in a handler is recovered, counted
(`bigfleet_latitude_panics_total`), logged at `ERROR`, and turned into
`codes.Internal` — it never crashes the process.

## Sample Prometheus scrape

The chart's Service carries `prometheus.io/scrape` annotations on the metrics
port, so a standard annotation-based scrape config picks it up. A static scrape:

```yaml
scrape_configs:
  - job_name: bigfleet-latitude
    kubernetes_sd_configs:
      - role: pod
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_scrape]
        action: keep
        regex: "true"
```

Useful alerts: a non-zero rate of `bigfleet_latitude_grpc_requests_total{code="FailedPrecondition"}`
(zombie-shard fencing), a rising `bigfleet_latitude_api_calls_total{outcome="error"}`
(Latitude API trouble), or any `bigfleet_latitude_panics_total` increase.
