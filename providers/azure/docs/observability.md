---
title: Observability
description: The Azure provider's metrics catalogue, health vs readiness, the logging model, and sample Prometheus scrape and alerts.
sidebar:
  order: 6
  label: Observability
---

The Azure provider is built to be operated from its signals. Every Azure API
call, every gRPC request, every background loop, and every observed Spot eviction
is instrumented; liveness and readiness are separate probes; and requests are
logged through a panic-recovering interceptor chain. This page is the reference
for all of it.

Observability lives on a **separate HTTP port** from the gRPC server. The gRPC
contract (`CapacityProvider` + `grpc.health.v1` health + reflection) is served on
`--addr` (`:9000`); `/metrics`, `/healthz`, and `/readyz` are served on
`--metrics-addr` (`:9090`). Set `--metrics-addr ""` to disable the HTTP server
entirely (the gRPC health service stays up regardless).

## Metrics catalogue

Metrics are registered on an **isolated Prometheus registry** (not the global
default), exposed at `GET /metrics` on `--metrics-addr`. Every series is prefixed
`bigfleet_azure_`. The Go runtime and process collectors are also registered, so
you get `go_*` and `process_*` for free.

### Azure API

The Azure client is wrapped by a transparent metrics decorator, so every Azure API
call the provider makes is counted and timed.

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_azure_api_calls_total` | counter | `op`, `outcome` | Azure API call volume, split by operation and `success`/`error`. The first place to look when creates or drains are failing. |
| `bigfleet_azure_api_duration_seconds` | histogram | `op` | Per-operation Azure API latency (default buckets). Rising latency on `CreateVM` or `ListVMs` usually precedes throttling. |

`op` is one of: `CreateVM`, `DeleteVM`, `ListVMs`, `StartVM` (power on a
stopped host before Configure), `Configure` (the CustomScript extension that
delivers the bootstrap blob), `Drain` (the drain extension), `RetailPrices` (the
Spot price fetch), and `ResourceSkus` (the allocatable lookup). `outcome` is
`success` or `error`.

### gRPC RPCs

Recorded by the logging interceptor for every unary `CapacityProvider` call.

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_azure_grpc_requests_total` | counter | `method`, `code` | Request volume by short method name (`Create`, `Configure`, `Drain`, `Delete`, `List`, …) and gRPC status `code`. Non-`OK` rates are your primary error signal. |
| `bigfleet_azure_grpc_request_duration_seconds` | histogram | `method` | Per-method RPC latency. `Create`/`Configure`/`Drain` are intentionally slow (they block on Azure pollers); watch the tail, not the mean. |

### Lifecycle and background loops

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_azure_panics_total` | counter | — | Recovered panics in gRPC handlers. The recovery interceptor turns a panic into `codes.Internal` rather than crashing; **any** increase warrants investigation. |
| `bigfleet_azure_reconcile_total` | counter | `outcome` | Background Azure→inventory reconcile runs (`--reconcile-interval`, default 2m). A flatlining `success` rate means drift detection has stalled. |
| `bigfleet_azure_price_refresh_total` | counter | `outcome` | Background price refresh runs (on-demand + spot, `--price-refresh`, default 1h). `error` means at least one fetch failed and the cached price is going stale; price is still served from cache. |
| `bigfleet_azure_price_last_success_timestamp_seconds` | gauge | — | Unix time of the last **fully-successful** price refresh (on-demand + spot). Alert on `time() - <this>` exceeding a few refresh intervals to catch a silently-stale cache. |
| `bigfleet_azure_spot_evictions_total` | counter | — | Observed Spot eviction (Scheduled Events `Preempt`) notices acted on. |

### Runtime collectors

Standard `go_*` (goroutines, GC, heap) and `process_*` (CPU, RSS, open FDs) series
for the usual saturation/leak watches.

## Health vs readiness vs gRPC health

There are three distinct health surfaces. They answer different questions — wire
them to the right consumers.

| Surface | Where | Answers | Use for |
|---|---|---|---|
| `GET /healthz` | `--metrics-addr` (HTTP) | "Is the process alive?" Always `200 ok` once the HTTP server is up. | Kubernetes **liveness** probe. |
| `GET /readyz` | `--metrics-addr` (HTTP) | "Should this pod take traffic?" `200 ready` only after startup completes; `503 not ready` during boot and graceful shutdown. | Kubernetes **readiness** probe. |
| `grpc.health.v1.Health` | `--addr` (gRPC) | The standard gRPC health protocol. `SERVING` once ready, `NOT_SERVING` on shutdown. | gRPC clients / load balancers, `grpc_health_probe`. |

Readiness and the gRPC health status flip together: the provider marks itself
`SERVING` + ready only after the backend, store, offerings, and price-cache warm
complete. On `SIGINT`/`SIGTERM` it flips gRPC health to `NOT_SERVING` and
`/readyz` to `503` **before** draining connections.

```yaml
livenessProbe:
  httpGet: { path: /healthz, port: 9090 }
  initialDelaySeconds: 5
  periodSeconds: 10
readinessProbe:
  httpGet: { path: /readyz, port: 9090 }
  periodSeconds: 5
```

If you front the gRPC port directly, use the standard gRPC health check
(`grpc_health_probe -addr=:9000`, with `-tls` flags against an mTLS provider).

## Logging model

Logs are structured (`log/slog` text handler) to **stderr** at `INFO` and above.
Two unary interceptors, chained in order, sit in front of every RPC:

1. **`recoveryInterceptor`** — recovers a panicking handler, increments
   `bigfleet_azure_panics_total`, logs an `ERROR`, and returns `codes.Internal`.
2. **`loggingInterceptor`** — records the RPC metrics, then emits one structured
   line per RPC with `method`, `code`, and `dur_ms`.

Log level is chosen by outcome and method: the lifecycle RPCs (`Create`,
`Configure`, `Drain`, `Delete`) log at **`INFO`**; hot-path `List` logs at
**`DEBUG`** (suppressed by default); **any** RPC that returns an error is bumped
to **`WARN`**.

A typical steady-state stream:

```
level=INFO msg="serving CapacityProvider" addr=[::]:9000 provider=azure-eastus location=eastus azure_backend=azure security=mTLS offerings=32 metrics_addr=:9090
level=INFO msg=rpc method=Create code=OK dur_ms=63140
level=INFO msg=rpc method=Configure code=OK dur_ms=48201
level=WARN msg=rpc method=Drain code=DeadlineExceeded dur_ms=900012
```

## Scraping with Prometheus

Point Prometheus at `--metrics-addr`. The chart's Service carries
`prometheus.io/scrape` annotations; a minimal static scrape:

```yaml
scrape_configs:
  - job_name: bigfleet-azure-provider
    scrape_interval: 15s
    static_configs:
      - targets: ["bigfleet-azure.bigfleet.svc:9090"]
        labels:
          provider: azure-eastus
```

### Example alerts

```yaml
groups:
  - name: bigfleet-azure-provider
    rules:
      - alert: BigfleetAzurePanics
        expr: increase(bigfleet_azure_panics_total[5m]) > 0
        labels: { severity: critical }
        annotations: { summary: "Azure provider recovered a panic in a gRPC handler" }

      - alert: BigfleetAzureApiErrors
        expr: |
          sum(rate(bigfleet_azure_api_calls_total{outcome="error"}[5m])) by (op)
            / sum(rate(bigfleet_azure_api_calls_total[5m])) by (op) > 0.1
        for: 10m
        labels: { severity: warning }
        annotations: { summary: ">10% of {{ $labels.op }} Azure calls are failing" }

      - alert: BigfleetAzureGrpcErrors
        expr: |
          sum(rate(bigfleet_azure_grpc_requests_total{code!="OK"}[5m])) by (method)
            / sum(rate(bigfleet_azure_grpc_requests_total[5m])) by (method) > 0.05
        for: 10m
        labels: { severity: warning }
        annotations: { summary: ">5% of {{ $labels.method }} RPCs returning {{ $labels.code }}" }

      - alert: BigfleetAzurePriceRefreshFailing
        expr: increase(bigfleet_azure_price_refresh_total{outcome="error"}[2h]) > 0
        for: 3h
        labels: { severity: warning }
        annotations: { summary: "Price refresh failing; served on-demand/spot prices are going stale" }

      - alert: BigfleetAzurePriceCacheStale
        expr: time() - bigfleet_azure_price_last_success_timestamp_seconds > 4 * 3600
        for: 15m
        labels: { severity: warning }
        annotations: { summary: "No fully-successful price refresh in >4h; prices served from a stale cache" }

      - alert: BigfleetAzureReconcileStalled
        expr: increase(bigfleet_azure_reconcile_total{outcome="success"}[15m]) == 0
        for: 15m
        labels: { severity: warning }
        annotations: { summary: "Azure->inventory reconcile has made no successful run in 15m" }

      - alert: BigfleetAzureProviderDown
        expr: up{job="bigfleet-azure-provider"} == 0
        for: 5m
        labels: { severity: critical }
        annotations: { summary: "Azure provider target is down" }
```

A useful P99 RPC-latency expression for dashboards:

```text
histogram_quantile(0.99,
  sum(rate(bigfleet_azure_grpc_request_duration_seconds_bucket[5m])) by (le, method))
```

## Wiring the Scheduled Events eviction feed

SPOT machines always report a non-zero `interruption_probability` forecast from
the pinned eviction-rate bands (see
[Pricing & interruption](/providers/azure/pricing-and-interruption/)). To raise
that value toward `1.0` on a **real, observed** eviction, the provider exposes an
ingest endpoint that a node-side agent reports `Preempt` events to — Azure
Scheduled Events live on the per-VM IMDS endpoint, not a central queue, so the
agent is what observes them.

**Endpoint:** `POST /internal/eviction` on the metrics port (`--metrics-addr`),
registered only on the real `azure` backend. Body:

```json
{ "machine_id": "azure-eastus/Spot/Standard_F8s_v2/eastus-1/000", "event_type": "Preempt" }
```

It is **fail-closed**: the endpoint is only registered when a bearer token is
configured — supply it via the `BIGFLEET_EVICTION_TOKEN` env var (the Helm chart
sources it from a Secret via `evictionToken.secretName`) in preference to the
`--eviction-token` flag, which would sit in cleartext in the pod spec. Without a
token the endpoint does not exist (the agent's POSTs get 404). Pair it with a
NetworkPolicy restricting the metrics port to the node CIDR. On a `Preempt` it
raises the machine's observed probability to
`0.99`, increments `bigfleet_azure_spot_evictions_total`, logs
`observed spot eviction notice`, and kicks a reconcile so the value propagates.

**Agent:** install the reference
[`deploy/agent/scheduled-events-agent.sh`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/azure/deploy/agent/scheduled-events-agent.sh)
via `--base-user-data`. It polls
`http://169.254.169.254/metadata/scheduledevents`, reads its own
`bigfleet-machine-id` IMDS tag, and POSTs `Preempt` events to the endpoint:

```sh
export BIGFLEET_EVICTION_URL=http://bigfleet-azure-eastus.bigfleet.svc:9090/internal/eviction
export BIGFLEET_EVICTION_TOKEN=<matches the provider's eviction token>
/opt/bigfleet/scheduled-events-agent.sh
```

Confirm the feed is live: wait for (or simulate) a Spot eviction and watch
`bigfleet_azure_spot_evictions_total` increment alongside the
`observed spot eviction notice` log line. If the counter never moves, check the
agent can reach the endpoint (NetworkPolicy / token) and that the VM actually
carries the `bigfleet-machine-id` tag.
