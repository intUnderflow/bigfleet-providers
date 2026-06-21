---
title: Observability
description: The Proxmox VE provider's metrics catalogue, health vs readiness, the logging model, and a sample Prometheus scrape and alerts.
sidebar:
  order: 6
  label: Observability
---

The Proxmox provider is built to be operated from its signals. Every Proxmox API
call, every gRPC request, and the background reconcile loop is instrumented;
liveness and readiness are separate probes; and requests are logged through a
panic-recovering interceptor chain. This page is the reference for all of it.

Observability lives on a **separate HTTP port** from the gRPC server. The gRPC
contract (`CapacityProvider` + `grpc.health.v1` health + reflection) is served on
`--addr` (`:9000`); `/metrics`, `/healthz`, and `/readyz` are served on
`--metrics-addr` (`:9090`). Set `--metrics-addr ""` to disable the HTTP server
entirely (the gRPC health service stays up regardless).

```sh
./bin/proxmox --proxmox-api-url https://pve1:8006/api2/json \
  --addr :9000 --metrics-addr :9090
# gRPC:    :9000  (CapacityProvider, health, reflection)
# HTTP:    :9090  (/metrics, /healthz, /readyz)
```

## Metrics catalogue

Metrics are registered on an **isolated Prometheus registry** (not the global
default), exposed at `GET /metrics` on `--metrics-addr`. Every series is prefixed
`bigfleet_proxmox_`. The Go runtime and process collectors are also registered, so
you get `go_*` and `process_*` for free.

### Proxmox API

The Proxmox client is wrapped by a transparent metrics decorator, so every API
operation the provider makes is counted and timed.

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_proxmox_api_calls_total` | counter | `op`, `outcome` | Proxmox API call volume, split by operation and `success`/`error`. The first place to look when clones or drains are failing. |
| `bigfleet_proxmox_api_duration_seconds` | histogram | `op` | Per-operation API latency (default buckets). Rising latency on `CloneVM` usually means slow storage or a busy cluster. |

`op` is one of: `CloneVM` (the Create clone + start + wait-for-agent),
`DeleteVM` (stop + destroy + purge), `DescribeManaged` (the inventory read),
`EnsureRunning` (power a stopped VM back on before Configure/Drain), `Configure`
(the guest-agent bootstrap delivery), and `Drain` (the guest-agent kubelet
drain). `outcome` is `success` or `error`.

### gRPC RPCs

Recorded by the logging interceptor for every unary `CapacityProvider` call.

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_proxmox_grpc_requests_total` | counter | `method`, `code` | Request volume by short method name (`Create`, `Configure`, `Drain`, `Delete`, `List`, …) and gRPC status `code` (`OK`, `Internal`, `DeadlineExceeded`, …). Non-`OK` rates are your primary error signal. |
| `bigfleet_proxmox_grpc_request_duration_seconds` | histogram | `method` | Per-method RPC latency. `Create`/`Configure`/`Drain` are intentionally slow (they block on the clone + guest agent); watch the tail, not the mean. |

### Lifecycle and background loops

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_proxmox_panics_total` | counter | — | Recovered panics in gRPC handlers. The recovery interceptor turns a panic into `codes.Internal` rather than crashing the process, so this is the only place a panic surfaces. **Any** increase warrants investigation. |
| `bigfleet_proxmox_reconcile_total` | counter | `outcome` | Background Proxmox→inventory reconcile runs (`--reconcile-interval`, default 2m), `success`/`error`. A flatlining `success` rate means drift detection has stalled. |

### Runtime collectors

Standard `go_*` (goroutines, GC, heap) and `process_*` (CPU, RSS, open FDs) series
from the Go and process collectors. Useful for the usual saturation/leak watches.

## Health vs readiness vs gRPC health

There are three distinct health surfaces. They answer different questions — wire
them to the right consumers.

| Surface | Where | Answers | Use for |
|---|---|---|---|
| `GET /healthz` | `--metrics-addr` (HTTP) | "Is the process alive?" Always `200 ok` once the HTTP server is up. | Kubernetes **liveness** probe. |
| `GET /readyz` | `--metrics-addr` (HTTP) | "Should this pod take traffic?" `200 ready` only after startup completes; `503 not ready` during boot and during graceful shutdown. | Kubernetes **readiness** probe. |
| `grpc.health.v1.Health` | `--addr` (gRPC) | The standard gRPC health protocol. Set `SERVING` once ready, flipped to `NOT_SERVING` on shutdown. | gRPC clients / load balancers, `grpc_health_probe`. |

Readiness and the gRPC health status flip together: the provider marks itself
`SERVING` + ready only after the backend, store, and offerings are wired and it is
serving. On `SIGINT`/`SIGTERM` it flips gRPC health to `NOT_SERVING` and `/readyz`
to `503` **before** draining connections, so load balancers stop sending new work
while in-flight RPCs finish.

Example probe wiring for a Kubernetes `Deployment` (the chart sets this):

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: 9090
  initialDelaySeconds: 5
  periodSeconds: 10
readinessProbe:
  httpGet:
    path: /readyz
    port: 9090
  periodSeconds: 10
```

If you front the gRPC port directly, you can instead use the standard gRPC health
check:

```sh
grpc_health_probe -addr=:9000
# or, against an mTLS provider:
grpc_health_probe -addr=:9000 -tls \
  -tls-client-cert client.crt -tls-client-key client.key -tls-ca-cert ca.crt
```

See [Install & deploy](/providers/proxmox/install/) for the full manifest and
[Security](/providers/proxmox/security/) for the gRPC mTLS flags.

## Logging model

Logs are structured (`log/slog` text handler) to **stderr** at `INFO` and above.
Two unary interceptors, chained in order, sit in front of every RPC:

1. **`loggingInterceptor`** (outer) — records `bigfleet_proxmox_grpc_requests_total`
   and `bigfleet_proxmox_grpc_request_duration_seconds`, then emits one structured
   line per RPC with `method`, `code`, and `dur_ms`.
2. **`recoveryInterceptor`** (inner) — recovers a panicking handler, increments
   `bigfleet_proxmox_panics_total`, logs an `ERROR` with the method, and returns
   `codes.Internal` instead of crashing the process. Because logging is the outer
   interceptor, a recovered panic is still recorded as `code=Internal`.

Log level for the per-RPC line is chosen by outcome and method:

- The lifecycle RPCs — `Create`, `Configure`, `Drain`, `Delete` — log at **`INFO`**
  (they are infrequent and operationally interesting).
- Hot-path RPCs like `List` log at **`DEBUG`** (suppressed by default).
- **Any** RPC that returns an error is bumped to **`WARN`**, regardless of method.

The background reconcile loop logs `WARN` on failure (`reconcile failed`).

A typical steady-state stream:

```
level=INFO msg="serving CapacityProvider" addr=[::]:9000 provider=proxmox-dc1 proxmox_backend=proxmox security=mTLS offerings=12 zones=pve1,pve2 metrics_addr=:9090
level=INFO msg=rpc method=Create code=OK dur_ms=64213
level=INFO msg=rpc method=Configure code=OK dur_ms=38880
level=WARN msg=rpc method=Drain code=DeadlineExceeded dur_ms=900014
```

## Scraping with Prometheus

Point Prometheus at `--metrics-addr`. A minimal static scrape:

```yaml
scrape_configs:
  - job_name: bigfleet-proxmox-provider
    scrape_interval: 15s
    static_configs:
      - targets: ["bigfleet-proxmox-dc1.bigfleet.svc:9090"]
        labels:
          provider: proxmox-dc1
```

With the Prometheus Operator, a `PodMonitor` keyed on the provider's labels does
the same in-cluster (scrape the `metrics` port at `/metrics`). The chart adds
`prometheus.io/scrape` annotations to the Service when `metrics.prometheusScrape`
is set.

### Example alerts

These PromQL rules cover the failure modes the metrics are designed to catch.
Tune thresholds to your fleet.

```yaml
groups:
  - name: bigfleet-proxmox-provider
    rules:
      # The recovery interceptor caught a panic. Never expected in steady state.
      - alert: BigfleetProxmoxPanics
        expr: increase(bigfleet_proxmox_panics_total[5m]) > 0
        labels: { severity: critical }
        annotations:
          summary: "Proxmox provider recovered a panic in a gRPC handler"

      # Proxmox API errors: clones, deletes, guest-agent calls failing.
      - alert: BigfleetProxmoxApiErrors
        expr: |
          sum(rate(bigfleet_proxmox_api_calls_total{outcome="error"}[5m])) by (op)
            / sum(rate(bigfleet_proxmox_api_calls_total[5m])) by (op) > 0.1
        for: 10m
        labels: { severity: warning }
        annotations:
          summary: ">10% of {{ $labels.op }} Proxmox API calls are failing"

      # Lifecycle RPCs returning non-OK to the engine.
      - alert: BigfleetProxmoxGrpcErrors
        expr: |
          sum(rate(bigfleet_proxmox_grpc_requests_total{code!="OK"}[5m])) by (method)
            / sum(rate(bigfleet_proxmox_grpc_requests_total[5m])) by (method) > 0.05
        for: 10m
        labels: { severity: warning }
        annotations:
          summary: ">5% of {{ $labels.method }} RPCs are returning {{ $labels.code }}"

      # Reconcile loop has stopped making successful progress.
      - alert: BigfleetProxmoxReconcileStalled
        expr: increase(bigfleet_proxmox_reconcile_total{outcome="success"}[15m]) == 0
        for: 15m
        labels: { severity: warning }
        annotations:
          summary: "Proxmox->inventory reconcile has made no successful run in 15m"

      # Provider down / not scrapeable.
      - alert: BigfleetProxmoxProviderDown
        expr: up{job="bigfleet-proxmox-provider"} == 0
        for: 5m
        labels: { severity: critical }
        annotations:
          summary: "Proxmox provider target is down"
```

A useful P99 RPC-latency expression for dashboards:

```text
histogram_quantile(0.99,
  sum(rate(bigfleet_proxmox_grpc_request_duration_seconds_bucket[5m])) by (le, method))
```

## See also

- [Troubleshooting](/providers/proxmox/troubleshooting/) — the runbook keyed on these metrics.
- [Configuration](/providers/proxmox/configuration/) — every flag, backend modes, the clone/bootstrap model.
- [Credentials](/providers/proxmox/credentials/) — the API token and TLS trust.
