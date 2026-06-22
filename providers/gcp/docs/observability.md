---
title: Observability
description: The GCP provider's metrics catalogue, health vs readiness, the logging model, and a sample Prometheus scrape.
sidebar:
  order: 6
  label: Observability
---

The GCP provider is built to be operated from its signals. Every GCE API call,
every gRPC request, and every background loop is instrumented; liveness and
readiness are separate probes; and requests are logged through a panic-recovering
interceptor chain.

Observability lives on a **separate HTTP port** from the gRPC server. The gRPC
contract (`CapacityProvider` + `grpc.health.v1` health + reflection) is served on
`--addr` (`:9000`); `/metrics`, `/healthz`, and `/readyz` are served on
`--metrics-addr` (`:9090`). Set `--metrics-addr ""` to disable the HTTP server
entirely (the gRPC health service stays up regardless).

## Metrics catalogue

Metrics are registered on an **isolated Prometheus registry** (not the global
default), exposed at `GET /metrics` on `--metrics-addr`. Every series is prefixed
`bigfleet_gcp_`. The Go runtime and process collectors are also registered, so
you get `go_*` and `process_*` for free.

### GCE API

The GCE client is wrapped by a transparent metrics decorator, so every API call
the provider makes is counted and timed.

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_gcp_api_calls_total` | counter | `op`, `outcome` | GCE API call volume, split by operation (`Insert`, `Delete`, `List`, `Configure`, `Drain`, `MachineTypes`) and `success`/`error`. The first place to look when creates or drains are failing. |
| `bigfleet_gcp_api_duration_seconds` | histogram | `op` | GCE API latency by operation. |

### gRPC

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_gcp_grpc_requests_total` | counter | `method`, `code` | CapacityProvider RPCs by method and gRPC status code. A spike of `FailedPrecondition` means a **zombie shard** is being fenced — alert on it. |
| `bigfleet_gcp_grpc_request_duration_seconds` | histogram | `method` | Per-RPC latency. The mutating RPCs are async, so these should stay sub-millisecond. |
| `bigfleet_gcp_panics_total` | counter | — | Recovered panics in gRPC handlers (should stay 0). |

### Background loops

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_gcp_reconcile_total` | counter | `outcome` | Background GCE→inventory reconcile runs by outcome. |
| `bigfleet_gcp_spot_preemptions_total` | counter | — | Observed GCE Spot preemptions of managed instances (raises the affected slot's observed interruption probability). |
| `bigfleet_gcp_price_refresh_total` | counter | `outcome` | Background live price-refresh runs (Cloud Billing Catalog) by outcome. |
| `bigfleet_gcp_price_last_refresh_timestamp_seconds` | gauge | — | Unix time of the last fully-successful price refresh. Alert on `time() - this` exceeding a few refresh intervals: prices are stale and the provider is serving the pinned fallback. |

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
chain with its method, gRPC code, and duration; the mutating RPCs log at `INFO`,
reads at `DEBUG`, and any error at `WARN`. **Neither the credentials nor the
opaque bootstrap blob is ever logged.** A panic in a handler is recovered,
counted (`bigfleet_gcp_panics_total`), logged at `ERROR`, and turned into
`codes.Internal` — it never crashes the process.

## Sample Prometheus scrape

The chart's Service carries `prometheus.io/scrape` annotations on the metrics
port, so a standard annotation-based scrape config picks it up:

```yaml
scrape_configs:
  - job_name: bigfleet-gcp
    kubernetes_sd_configs:
      - role: pod
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_scrape]
        action: keep
        regex: "true"
```

Useful alerts: a non-zero rate of `bigfleet_gcp_grpc_requests_total{code="FailedPrecondition"}`
(zombie-shard fencing), a rising `bigfleet_gcp_api_calls_total{outcome="error"}`
(GCE API trouble), or any `bigfleet_gcp_panics_total` increase.
