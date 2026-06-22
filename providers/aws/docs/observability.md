---
title: Observability
description: The AWS provider's metrics catalogue, health vs readiness, the logging model, sample Prometheus scrape and alerts, and the EventBridge → SQS spot-interruption feed.
sidebar:
  order: 6
  label: Observability
---

The AWS provider is built to be operated from its signals. Every EC2/SSM API
call, every gRPC request, every background loop, and every observed spot
interruption is instrumented; liveness and readiness are separate probes; and
requests are logged through a panic-recovering interceptor chain. This page is
the reference for all of it.

Observability lives on a **separate HTTP port** from the gRPC server. The gRPC
contract (`CapacityProvider` + `grpc.health.v1` health + reflection) is served on
`--addr` (`:9000`); `/metrics`, `/healthz`, and `/readyz` are served on
`--metrics-addr` (`:9090`). Set `--metrics-addr ""` to disable the HTTP server
entirely (the gRPC health service stays up regardless).

```sh
./bin/aws --region us-east-1 --addr :9000 --metrics-addr :9090
# gRPC:    :9000  (CapacityProvider, health, reflection)
# HTTP:    :9090  (/metrics, /healthz, /readyz)
```

## Metrics catalogue

Metrics are registered on an **isolated Prometheus registry** (not the global
default), exposed at `GET /metrics` on `--metrics-addr`. Every series is
prefixed `bigfleet_aws_`. The Go runtime and process collectors are also
registered, so you get `go_*` and `process_*` for free.

### EC2 / SSM API

The EC2 client is wrapped by a transparent metrics decorator, so every AWS API
call the provider makes is counted and timed.

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_aws_ec2_api_calls_total` | counter | `op`, `outcome` | AWS API call volume, split by operation and `success`/`error`. The first place to look when launches or drains are failing. |
| `bigfleet_aws_ec2_api_duration_seconds` | histogram | `op` | Per-operation AWS API latency (default buckets). Rising latency on `RunInstances` or `DescribeInstances` usually precedes throttling. |

`op` is one of: `RunInstances`, `TerminateInstances`, `DescribeInstances`,
`DescribeSpotPriceHistory`, `Configure` (the SSM `SendCommand` +
`GetCommandInvocation` poll that delivers the bootstrap blob), and `Drain` (the
SSM cordon/drain). `outcome` is `success` or `error`.

### gRPC RPCs

Recorded by the logging interceptor for every unary `CapacityProvider` call.

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_aws_grpc_requests_total` | counter | `method`, `code` | Request volume by short method name (`Create`, `Configure`, `Drain`, `Delete`, `List`, …) and gRPC status `code` (`OK`, `Internal`, `DeadlineExceeded`, …). Non-`OK` rates are your primary error signal. |
| `bigfleet_aws_grpc_request_duration_seconds` | histogram | `method` | Per-method RPC latency. `Create`/`Configure`/`Drain` are intentionally slow (they block on EC2 + SSM); watch the tail, not the mean. |

### Lifecycle and background loops

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `bigfleet_aws_panics_total` | counter | — | Recovered panics in gRPC handlers. The recovery interceptor turns a panic into `codes.Internal` rather than crashing the process, so this is the only place a panic surfaces. **Any** increase warrants investigation. |
| `bigfleet_aws_reconcile_total` | counter | `outcome` | Background EC2→inventory reconcile runs (`--reconcile-interval`, default 2m), `success`/`error`. A flatlining `success` rate means drift detection has stalled. |
| `bigfleet_aws_spot_refresh_total` | counter | `outcome` | Background spot-price refresh runs (`--spot-refresh`, default 5m), `success`/`error`. `error` means the cached spot price is going stale; price is still served from cache. |
| `bigfleet_aws_ondemand_refresh_total` | counter | `outcome` | Background on-demand price refresh runs from the AWS Price List Bulk API (`--ondemand-refresh`, default 60m), `success`/`error`. `error` means live on-demand prices are going stale; the seed/last-known prices are still served. |
| `bigfleet_aws_ondemand_price_last_success_timestamp_seconds` | gauge | — | Unix time of the last successful on-demand price refresh. Staleness = `time() - this`; alert if it grows past a few refresh intervals. |
| `bigfleet_aws_spot_interruptions_total` | counter | — | Observed spot interruption / rebalance notices consumed from the SQS queue. Only increments when `--spot-interruption-queue` is wired (see below). |

### Runtime collectors

Standard `go_*` (goroutines, GC, heap) and `process_*` (CPU, RSS, open FDs)
series from the Go and process collectors. Useful for the usual
saturation/leak watches.

## Health vs readiness vs gRPC health

There are three distinct health surfaces. They answer different questions — wire
them to the right consumers.

| Surface | Where | Answers | Use for |
|---|---|---|---|
| `GET /healthz` | `--metrics-addr` (HTTP) | "Is the process alive?" Always `200 ok` once the HTTP server is up. | Kubernetes **liveness** probe. |
| `GET /readyz` | `--metrics-addr` (HTTP) | "Should this pod take traffic?" `200 ready` only after startup completes; `503 not ready` during boot and during graceful shutdown. | Kubernetes **readiness** probe. |
| `grpc.health.v1.Health` | `--addr` (gRPC) | The standard gRPC health protocol. Set `SERVING` once ready, flipped to `NOT_SERVING` on shutdown. | gRPC clients / load balancers, `grpc_health_probe`. |

Readiness and the gRPC health status flip together: the provider marks itself
`SERVING` + ready only after the backend, store, offerings, and price-cache warm
have all completed. On `SIGINT`/`SIGTERM` it flips gRPC health to `NOT_SERVING`
and `/readyz` to `503` **before** draining connections, so load balancers stop
sending new work while in-flight RPCs finish.

Example probe wiring for a Kubernetes `Deployment`:

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
  periodSeconds: 5
```

If you front the gRPC port directly, you can instead use the standard gRPC
health check:

```sh
grpc_health_probe -addr=:9000
# or, against an mTLS provider:
grpc_health_probe -addr=:9000 -tls \
  -tls-client-cert client.crt -tls-client-key client.key -tls-ca-cert ca.crt
```

See [Install & deploy](/providers/aws/install/) for the full manifest and
[Security](/providers/aws/security/) for the mTLS flags.

## Logging model

Logs are structured (`log/slog` text handler) to **stderr** at `INFO` and above.
Two unary interceptors, chained in order, sit in front of every RPC:

1. **`recoveryInterceptor`** — recovers a panicking handler, increments
   `bigfleet_aws_panics_total`, logs an `ERROR` with the method and panic value,
   and returns `codes.Internal` instead of crashing the process.
2. **`loggingInterceptor`** — records `bigfleet_aws_grpc_requests_total` and
   `bigfleet_aws_grpc_request_duration_seconds`, then emits one structured line
   per RPC with `method`, `code`, and `dur_ms`.

Log level for the per-RPC line is chosen by outcome and method:

- The lifecycle RPCs — `Create`, `Configure`, `Drain`, `Delete` — log at
  **`INFO`** (they are infrequent and operationally interesting).
- Hot-path RPCs like `List` log at **`DEBUG`** (suppressed by default).
- **Any** RPC that returns an error is bumped to **`WARN`**, regardless of
  method.

The background loops log on their own: reconcile and the interruption poller log
`WARN` on failure, and the poller logs an `INFO` line (`observed spot
interruption`) with the `instance`, `machine`, `probability`, and `event` for
every notice it acts on.

A typical steady-state stream:

```
level=INFO msg="serving CapacityProvider" addr=[::]:9000 provider=aws region=us-east-1 ec2_backend=aws security=mTLS offerings=32 metrics_addr=:9090
level=INFO msg=rpc method=Create code=OK dur_ms=41213
level=INFO msg=rpc method=Configure code=OK dur_ms=92880
level=WARN msg=rpc method=Drain code=DeadlineExceeded dur_ms=900014
level=INFO msg="observed spot interruption" instance=i-0abc... machine=m-7f3... probability=0.99 event="EC2 Spot Instance Interruption Warning"
```

## Scraping with Prometheus

Point Prometheus at `--metrics-addr`. A minimal static scrape:

```yaml
scrape_configs:
  - job_name: bigfleet-aws-provider
    scrape_interval: 15s
    static_configs:
      - targets: ["bigfleet-aws.bigfleet.svc:9090"]
        labels:
          provider: aws-us-east-1
```

With the Prometheus Operator, a `PodMonitor` keyed on the provider's labels does
the same in-cluster (scrape the `metrics` port at `/metrics`).

### Example alerts

These PromQL rules cover the failure modes the metrics are designed to catch.
Tune thresholds to your fleet.

```yaml
groups:
  - name: bigfleet-aws-provider
    rules:
      # The recovery interceptor caught a panic. Never expected in steady state.
      - alert: BigfleetAwsPanics
        expr: increase(bigfleet_aws_panics_total[5m]) > 0
        labels: { severity: critical }
        annotations:
          summary: "AWS provider recovered a panic in a gRPC handler"

      # EC2/SSM API errors: launches, terminations, drains failing against AWS.
      - alert: BigfleetAwsEc2ApiErrors
        expr: |
          sum(rate(bigfleet_aws_ec2_api_calls_total{outcome="error"}[5m])) by (op)
            / sum(rate(bigfleet_aws_ec2_api_calls_total[5m])) by (op) > 0.1
        for: 10m
        labels: { severity: warning }
        annotations:
          summary: ">10% of {{ $labels.op }} EC2 calls are failing"

      # Lifecycle RPCs returning non-OK to the engine.
      - alert: BigfleetAwsGrpcErrors
        expr: |
          sum(rate(bigfleet_aws_grpc_requests_total{code!="OK"}[5m])) by (method)
            / sum(rate(bigfleet_aws_grpc_requests_total[5m])) by (method) > 0.05
        for: 10m
        labels: { severity: warning }
        annotations:
          summary: ">5% of {{ $labels.method }} RPCs are returning {{ $labels.code }}"

      # Spot price cache going stale: refresh loop is erroring.
      - alert: BigfleetAwsSpotRefreshFailing
        expr: increase(bigfleet_aws_spot_refresh_total{outcome="error"}[15m]) > 0
        for: 30m
        labels: { severity: warning }
        annotations:
          summary: "Spot-price refresh failing; served spot prices are going stale"

      # On-demand prices going stale: no successful refresh in 3 intervals.
      - alert: BigfleetAwsOnDemandPriceStale
        expr: time() - bigfleet_aws_ondemand_price_last_success_timestamp_seconds > 3 * 60 * 60
        for: 15m
        labels: { severity: warning }
        annotations:
          summary: "On-demand price refresh has not succeeded recently; seed/last-known prices are being served"

      # Reconcile loop has stopped making successful progress.
      - alert: BigfleetAwsReconcileStalled
        expr: increase(bigfleet_aws_reconcile_total{outcome="success"}[15m]) == 0
        for: 15m
        labels: { severity: warning }
        annotations:
          summary: "EC2->inventory reconcile has made no successful run in 15m"

      # Provider down / not scrapeable.
      - alert: BigfleetAwsProviderDown
        expr: up{job="bigfleet-aws-provider"} == 0
        for: 5m
        labels: { severity: critical }
        annotations:
          summary: "AWS provider target is down"
```

A useful P99 RPC-latency expression for dashboards:

```text
histogram_quantile(0.99,
  sum(rate(bigfleet_aws_grpc_request_duration_seconds_bucket[5m])) by (le, method))
```

## Wiring the EventBridge → SQS spot-interruption feed

SPOT machines always report a non-zero `interruption_probability` forecast from
the pinned Spot Instance Advisor buckets (see
[Pricing & interruption](/providers/aws/pricing-and-interruption/)). To raise
that value toward `1.0` on a **real, observed** notice, feed the provider an SQS
queue of EC2 spot interruption / rebalance events and pass it as
`--spot-interruption-queue <url>`.

When wired, the provider long-polls the queue, and for each event it raises the
affected machine's observed probability:

- `EC2 Spot Instance Interruption Warning` (the 2-minute kill notice) → **0.99**
- `EC2 Instance Rebalance Recommendation` (elevated risk) → **0.5**

It increments `bigfleet_aws_spot_interruptions_total`, logs the notice, and the
background reconcile loop (`--reconcile-interval`) propagates the raised value
into inventory, so the engine's victim scoring sees a rising probability for a
machine about to be reclaimed. Events for instances the provider does not manage
(or that are already gone) are silently ignored, and messages are deleted from
the queue after handling.

### Create the queue and rule

```sh
REGION=us-east-1

# 1. SQS queue.
QURL=$(aws sqs create-queue --region $REGION \
  --queue-name bigfleet-spot-interruptions \
  --query QueueUrl --output text)
QARN=$(aws sqs get-queue-attributes --region $REGION \
  --queue-url "$QURL" --attribute-names QueueArn \
  --query Attributes.QueueArn --output text)

# 2. EventBridge rule for both spot signals.
aws events put-rule --region $REGION \
  --name bigfleet-spot-interruptions \
  --event-pattern '{
    "source": ["aws.ec2"],
    "detail-type": [
      "EC2 Spot Instance Interruption Warning",
      "EC2 Instance Rebalance Recommendation"
    ]
  }'

# 3. Point the rule at the queue.
aws events put-targets --region $REGION \
  --rule bigfleet-spot-interruptions \
  --targets "Id=sqs,Arn=$QARN"
```

### Let EventBridge write to the queue

Attach a queue policy granting `events.amazonaws.com` `sqs:SendMessage`,
scoped to the rule ARN:

```sh
aws sqs set-queue-attributes --region $REGION --queue-url "$QURL" \
  --attributes Policy='{
    "Version": "2012-10-17",
    "Statement": [{
      "Effect": "Allow",
      "Principal": { "Service": "events.amazonaws.com" },
      "Action": "sqs:SendMessage",
      "Resource": "'"$QARN"'",
      "Condition": {
        "ArnEquals": { "aws:SourceArn": "arn:aws:events:'"$REGION"':ACCOUNT_ID:rule/bigfleet-spot-interruptions" }
      }
    }]
  }'
```

### Run the provider against it

```sh
./bin/aws --region $REGION --addr :9000 --metrics-addr :9090 \
  --spot-interruption-queue "$QURL"
```

The provider's own IAM role needs `sqs:ReceiveMessage` and `sqs:DeleteMessage`
on this queue — these permissions are **only** required when
`--spot-interruption-queue` is set. See [IAM](/providers/aws/iam/) for the exact
policy statement.

Confirm the feed is live: trigger or wait for a spot notice and watch
`bigfleet_aws_spot_interruptions_total` increment alongside the
`observed spot interruption` log line. If the counter never moves, check the
queue's `ApproximateNumberOfMessagesVisible` (messages arriving but not
consumed → an IAM problem on the provider; no messages → the rule or queue
policy).