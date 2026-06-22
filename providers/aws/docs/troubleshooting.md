---
title: Troubleshooting
description: A runbook for the AWS EC2 provider — diagnosing stuck/failed machines, spot capacity, IAM, SSM, pricing, and readiness from metrics, logs, and Get.
sidebar:
  order: 7
  label: Troubleshooting
---

This is a runbook: a symptom, then the three places you look — the
`bigfleet_aws_*` metrics on `--metrics-addr`, the structured logs on stderr,
and a `Get` against the machine — and the fix.

Keep these handy. The provider logs structured key/value lines (`method`,
`code`, `dur_ms`, `err`, …); grep them. The metrics live at `/metrics`, and the
EC2/SSM call counters are the fastest way to see *which* AWS API is unhappy:

```sh
# What's the provider doing right now?
curl -s localhost:9090/metrics | grep -E 'bigfleet_aws_(ec2_api_calls|grpc_requests|reconcile|spot_refresh|ondemand_refresh|spot_interruptions|panics)_total'

# gRPC error rate, by method and code:
curl -s localhost:9090/metrics | grep bigfleet_aws_grpc_requests_total

# EC2/SSM API errors, by operation:
curl -s localhost:9090/metrics | grep 'bigfleet_aws_ec2_api_calls_total' | grep 'outcome="error"'
```

`op` on the EC2 counters is the *logical* operation, not the SDK call:
`RunInstances`, `TerminateInstances`, `DescribeInstances`,
`DescribeSpotPriceHistory`, `Configure` (the SSM bootstrap), and `Drain` (the
SSM cordon/drain). A spike of `outcome="error"` on one `op` localizes almost
every failure below.

## Machines stuck or FAILED

`Configure`/`Drain`/`Create` run async under [providerkit](/providers/aws/configuration/)
transition timeouts (Create 5m, Configure 8m, Drain 15m, Delete 5m). A machine
that exceeds its timeout, or whose backend call returns an error, lands in
`FAILED` rather than a false Idle/Configured — that is by design. To find *why*,
correlate the failing RPC in the logs with the EC2/SSM `op` that errored.

```sh
# The last lifecycle RPCs and their gRPC codes:
journalctl -u bigfleet-aws | grep '"rpc"' | grep -E 'Create|Configure|Drain|Delete'
```

### Create times out (instance never reaches `running`)

`CreateInstance` calls `RunInstances` then **blocks on the instance-running
waiter** before returning Idle — so the machine sits in its Create transition
until the host is actually running. If that exceeds the kit's 5m Create timeout,
the machine goes `FAILED`.

- **Symptom:** `RunInstances` counter increments (often `outcome="success"` —
  the launch worked) but the machine never leaves Creating; a log line
  `waiting for instance i-… to run` then a Create RPC with a non-OK `code`.
- **Diagnose:** describe the instance directly — it is usually stuck `pending`
  on a capacity/ENI/subnet problem, or it came up but failed status checks.
  ```sh
  aws ec2 describe-instances --instance-ids i-0123 \
    --query 'Reservations[].Instances[].[State.Name,StateReason.Message]'
  ```
- **Fix:** resolve the underlying launch problem (subnet free IPs, correct AMI
  architecture for the instance family, security-group egress for the bootstrap
  to reach the cluster). For spot, see [InsufficientInstanceCapacity](#spot-insufficientinstancecapacity)
  below. If launches are *slow* but succeed, it is throttling, next.

### RunInstances throttled (`RequestLimitExceeded`)

The SDK uses **adaptive retry** (client-side rate limiting + backoff, up to 8
attempts), so a throttled `RunInstances`/`DescribeInstances` normally retries
rather than failing. Sustained throttling still shows up.

- **Symptom:** `bigfleet_aws_ec2_api_calls_total{op="RunInstances",outcome="error"}`
  climbing, logs with `RequestLimitExceeded` or `Throttling`, and rising
  `bigfleet_aws_ec2_api_duration_seconds{op="RunInstances"}` (backoff inflates
  latency). If a Create's whole 5m budget burns in backoff it still FAILs.
- **Diagnose:** check whether it is one busy region/account hitting the EC2
  mutating-call limit — concurrent shards scaling at once is the usual cause.
- **Fix:** spread launches (smaller per-shard offering `count`, or stagger
  scale-ups), request an EC2 API rate-limit increase, and confirm the Create
  timeout comfortably exceeds your worst-case retry backoff.

### SSM bootstrap failure (Configure → FAILED)

`Configure` tags `bigfleet:cluster`, then delivers the opaque bootstrap blob via
SSM `SendCommand` to the `--bootstrap-hook` path and **polls
`GetCommandInvocation` until Success**. A non-Success terminal status (Failed,
TimedOut, Cancelled) returns an error and the machine goes `FAILED`. A bootstrap
that merely *enqueues* is not success.

- **Symptom:** `bigfleet_aws_ec2_api_calls_total{op="Configure",outcome="error"}`
  increments; logs include `ssm command "bigfleet-configure" on i-… ended Failed`
  with the command's stderr tail.
- **Diagnose:** the most common root cause is **no SSM agent / wrong instance
  profile** — see [SSM agent missing](#ssm-agent-missing-instance-unreachable).
  Otherwise read the full invocation output:
  ```sh
  aws ssm list-command-invocations --instance-id i-0123 --details \
    --query 'CommandInvocations[?Comment==`bigfleet-configure`]'
  ```
- **Fix:** make the hook robust. The provider runs, on the node:
  `set -euo pipefail; … | base64 -d > <hook>.blob; <hook> <cluster-id>`. So the
  AMI must ship an executable at `--bootstrap-hook` (default
  `/opt/bigfleet/bootstrap`) that consumes `<hook>.blob` and joins the cluster,
  and exits non-zero on failure. A hook that is missing, non-executable, or
  joins the wrong cluster is the usual culprit.

### Drain times out (Drain → FAILED)

`Drain` removes the `bigfleet:cluster` tag and runs `kubectl cordon` (tolerant
of re-run) then `kubectl drain` via SSM, polling to Success. The drain must
**not** swallow failure — an incomplete drain has to surface as `FAILED`, never
a false Idle.

- **Symptom:** `op="Drain",outcome="error"`; logs `ssm command "bigfleet-drain"
  … ended Failed`/`TimedOut`. Strict PodDisruptionBudgets are the classic cause
  (hence the generous 15m Drain timeout).
- **Diagnose:** on the node / via SSM, `kubectl get pods --field-selector
  spec.nodeName=<node> -A` and check PDBs blocking eviction.
- **Fix:** relax the offending PDB or extend the grace period. Note the drain
  uses the node's FQDN (`hostname -f`, the EC2 private DNS name) as the
  Kubernetes node name — if your cluster names nodes differently, `kubectl
  drain` can't find the node and fails.

## Spot: InsufficientInstanceCapacity

Spot `RunInstances` is one-time (`SpotInstanceType=one-time`,
interruption-behavior terminate). When the pool is dry, AWS rejects the launch.

- **Symptom:** Create FAILs quickly; `op="RunInstances",outcome="error"` with
  `InsufficientInstanceCapacity` (or `MaxSpotInstanceCountExceeded`) in the log
  `RunInstances …: …` line.
- **Diagnose:** it is pool-specific (instance type × AZ). Cross-check the
  forecast you are already publishing — a high `interruption_probability` for
  that type predicts exactly this.
- **Fix:** offer **more (type, zone) pairs** so the engine has fallbacks; spread
  across both `--zone-a`/`--zone-b` (and more). The provider does not silently
  fall back to on-demand — capacity type is a property of the offering slot, so
  diversify offerings rather than expecting automatic substitution.

## IAM: AccessDenied / PassRole

- **Symptom:** any `op` with `outcome="error"` and `UnauthorizedOperation` /
  `AccessDenied` in the log; or `RunInstances` failing only when
  `--iam-instance-profile` is set, with `iam:PassRole`-related denial.
- **Diagnose:** match the denied action to [the IAM page](/providers/aws/iam/).
  The provider calls exactly: `ec2:RunInstances`, `ec2:TerminateInstances`,
  `ec2:DescribeInstances`, `ec2:DescribeSpotPriceHistory`, `ec2:CreateTags`,
  `ec2:DeleteTags`, `ssm:SendCommand`, `ssm:GetCommandInvocation`; plus
  `iam:PassRole` **only** when `--iam-instance-profile` is set, and
  `sqs:ReceiveMessage`/`sqs:DeleteMessage` **only** with
  `--spot-interruption-queue`.
- **Fix:**
  - `iam:PassRole` denied on `RunInstances`: the provider's role must be allowed
    to pass the **node** role, scoped to it, with condition
    `iam:PassedToService = ec2.amazonaws.com`. This permission is needed only
    because you set `--iam-instance-profile`.
  - `sqs:*` denied: grant `ReceiveMessage` + `DeleteMessage` on the
    `--spot-interruption-queue` (or drop the flag).
  - On EKS, use **IRSA**: annotate the ServiceAccount with
    `eks.amazonaws.com/role-arn` so the pod assumes the provider role. A blanket
    `AccessDenied` on the very first call usually means IRSA isn't wired and the
    SDK fell back to the node's own role.

## SSM agent missing / instance unreachable

`Configure` and `Drain` reach the node **through SSM**, so SSM must work end to
end or both fail.

- **Symptom:** `op="Configure"` (or `Drain`) errors; in the SSM console the
  instance is *not* a Managed Instance, or `SendCommand` targets it but the
  invocation never leaves `Pending`. The provider keeps polling
  `GetCommandInvocation` (treating not-yet-registered as transient) until the
  transition times out, then FAILs.
- **Diagnose:**
  ```sh
  aws ssm describe-instance-information \
    --query "InstanceInformationList[?InstanceId=='i-0123']"
  ```
  Empty result ⇒ the node never registered with SSM.
- **Fix:** three things must all hold: the AMI runs the SSM agent; the **node**
  instance profile (`--iam-instance-profile`) includes
  `AmazonSSMManagedInstanceCore`; and the node has network egress to the SSM
  endpoints (NAT or VPC endpoints for `ssm`, `ssmmessages`, `ec2messages`). This
  is the single most common cause of Configure/Drain failures.

## Cold spot price

On startup (and for a freshly-offered pair) the spot cache is empty. `price`
never blocks on the network on the List hot path, so a cold pair reports a
**conservative fallback of `0.3 × on-demand`** until a refresh fills the cache.

- **Symptom:** spot `price_per_hour` in `Get` looks like a round fraction of
  on-demand right after boot, and `bigfleet_aws_spot_refresh_total{outcome="error"}`
  or `op="DescribeSpotPriceHistory",outcome="error"` is non-zero.
- **Diagnose:** the startup warm-up is best-effort and bounded (20s). A failed
  refresh logs `pricing: spot price fetch failed; keeping fallback` per
  (type, zone). The background refresher then retries every `--spot-refresh`
  (default 5m).
- **Fix:** usually self-heals on the next refresh. If it persists, check
  `ec2:DescribeSpotPriceHistory` permission and that the (type, zone) actually
  has recent history — `no spot price history for <type> in <zone>` means AWS
  returned none in the 6h window (often a zone where that type isn't offered).

## On-demand price won't refresh / startup refuses to start

On-demand prices are **live-refreshed** from the public AWS Price List Bulk API
(`--ondemand-refresh`, default 60m); the pinned `onDemandByRegion` table is only
the seed/fallback. The refresh is best-effort, but an offering with **no** price
at all is rejected at startup.

- **Symptom A (won't start):** `on-demand pricing: instance types have no live or
  pinned price (would emit price_per_hour=0, winning the cost signal): <types>`.
  A named `on_demand`/`reserved` offering type has neither a live offer-file
  price nor a pinned seed.
- **Symptom B (stale):** `on-demand price refresh failed; serving last-known
  on-demand prices from cache/seed`, `bigfleet_aws_ondemand_refresh_total{outcome="error"}`
  rising, or `time() - bigfleet_aws_ondemand_price_last_success_timestamp_seconds`
  growing past a few intervals.
- **Diagnose:** the startup warm is best-effort and bounded (90s); a failed
  refresh logs `pricing: on-demand price fetch failed; keeping last-known
  prices`. The provider needs egress to `pricing.us-east-1.amazonaws.com` (no
  credentials) — check NAT/proxy. For Symptom A, the offer file simply doesn't
  price that type in this region.
- **Fix:** for Symptom A, add the types to `onDemandByRegion` (regenerate the
  seed with `cmd/genpricing`) or remove them from your offerings. For Symptom B,
  it self-heals on the next successful refresh; staleness only degrades the
  engine's *relative* cost ranking, never correctness (last-known prices keep
  serving). The advisor interruption buckets and instance-type resources are
  separately us-east-1 approximations — see
  [Pricing & interruption](/providers/aws/pricing-and-interruption/).

## Readiness never goes green

`/readyz` returns `503 not ready` until the server is fully wired and serving;
`/healthz` is liveness only (always `200 ok` once the HTTP server is up). The
gRPC `grpc.health.v1` status flips to `SERVING` at the same point.

- **Symptom:** `curl localhost:9090/readyz` ⇒ `not ready`; the pod never passes
  its readiness probe; no `serving CapacityProvider` log line.
- **Diagnose:** readiness is set only **after** `run()` reaches the serving
  point. If the process exits or hangs in `newEC2Real`/config load first, you'll
  see a startup error on stderr instead. Common blockers:
  - `--ec2-backend=aws` (or `auto` with `--region`) but missing **`--ami`**
    (`--ami is required for the aws backend`) or a bad `--subnets` value.
  - TLS misconfig: `both --tls-cert and --tls-key are required`, or `--tls-ca
    set without --tls-cert/--tls-key`.
  - `--addr` already in use (`listen on :9000: …`).
  - `no offerings configured` / an offering with empty `instance_type` or `zone`
    (the provider requires a zone on every offering).
- **Fix:** resolve the startup error in the logs. On shutdown (SIGINT/SIGTERM)
  readiness intentionally flips back to `not ready` and gRPC health to
  `NOT_SERVING` before graceful stop — a `not ready` during termination is
  expected, not a fault.

## Panics

`bigfleet_aws_panics_total` should stay flat. A recovered panic in a gRPC
handler is converted to `codes.Internal` (the RPC fails, the process survives)
and logged as `recovered panic in gRPC handler` with the method. Any non-zero
value is a bug — capture the log line and the request that triggered it.

## See also

- [Observability](/providers/aws/observability/) — the full metric/health/log surface
- [IAM](/providers/aws/iam/) — the exact policy and IRSA wiring
- [Pricing & interruption](/providers/aws/pricing-and-interruption/) — how price and probability are sourced
- [Configuration](/providers/aws/configuration/) — every flag, backend modes, the bootstrap model
