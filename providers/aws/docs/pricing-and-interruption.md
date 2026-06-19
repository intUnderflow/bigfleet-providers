---
title: Pricing & interruption
description: How the AWS provider sources price_per_hour and SPOT interruption_probability, why spot is never zero, and how to refresh and regenerate the pinned tables.
sidebar:
  order: 4
  label: Pricing & interruption
---

Spot capacity is how you make your fleet cheaper — but only if BigFleet can tell
which machines are safe to put work on. This provider gives BigFleet two honest
numbers per machine so it ranks capacity by real cost: the hourly price, and how
likely a spot machine is to be reclaimed. Crucially, this provider **never**
claims a spot machine has zero interruption risk — so BigFleet can't be fooled
into piling critical work onto capacity AWS may take back. This page covers
where both numbers come from, how they stay fresh, and how to keep the pinned
price tables accurate for your region.

Under the hood the engine combines the two into an effective cost — a spot
machine reporting zero interruption risk would look both cheap *and* safe and
get workloads it should never run, which is exactly the trap this provider
avoids:

```
effective_cost = price_per_hour + interruption_probability × penalty
```

Both values are read from in-memory caches on the `List`/seed hot path
(`speculativeSlots` and `Describe`) — neither ever blocks on an AWS API call
while the engine is waiting. The network work happens on background timers.

## `price_per_hour`

Price is sourced per capacity type (`pricing.go`):

| Capacity type | Source |
|---|---|
| `on_demand` | Pinned per-region on-demand table (`onDemandByRegion`). |
| `reserved` | Priced at on-demand unless you model a real reservation discount. |
| `spot` | Current price from `ec2:DescribeSpotPriceHistory`, cached + refreshed on a timer. |
| `bare_metal` | `0` — already paid for. |

### On-demand: a pinned, region-keyed table

On-demand prices come from `onDemandByRegion`, a pinned table keyed by region
then instance type. On-demand prices are stable, so a pinned table is the right
model: it is deterministic and has no runtime pricing-API dependency on the
`List` hot path. It is **not** load-bearing for correctness — it feeds the
engine's *relative* cost ranking — but keep it roughly accurate.

`us-east-1` is the authoritative baseline, and `us-west-2` is priced identically
for these families. A region with **no** pinned table of its own falls back to
the baseline and logs a warning (the prices are then approximate). The empty
region — the fake/dev backend, which does not price-rank — falls back silently.

#### Regenerating a region's table

Rather than hand-editing prices, regenerate a region's table with the
`genpricing` tool. It reads the **public** AWS Price List Bulk API — no AWS
credentials — and prints a Go map literal you paste into `onDemandByRegion`:

```sh
cd providers/aws
go run ./cmd/genpricing -region eu-west-1 \
  -types m6i.large,m6i.xlarge,c7g.large,c7g.xlarge,r6i.large,g5.xlarge
# ->  "eu-west-1": {
#         "c7g.large": 0.08075,
#         "m6i.large": 0.1056,
#         ...
#     },
```

It selects the plain on-demand SKU (Linux, Shared tenancy, no pre-installed
software), so Windows / Dedicated / Reserved SKUs for the same type are ignored.
The offer files are large (tens of MB), so this is an occasional offline
maintenance step, never a runtime dependency.

### Spot: refreshed from the price history

Spot prices come from `ec2:DescribeSpotPriceHistory`, one fetch per distinct
`(instance_type, zone)` SPOT offering pair, cached in memory. The cache is
warmed once at startup (a bounded, best-effort 20s warm before the first
`List`) and then refreshed on a timer set by `--spot-refresh` (default `5m`).
Refresh is best-effort: a failed fetch keeps the prior (or fallback) value and
logs a warning, rather than zeroing the price.

Before the first successful refresh, a SPOT read falls back to a conservative
`0.3 × on-demand` for that instance type, so a cold cache still ranks spot
below on-demand without ever reading `0`.

The background refresher records its outcome on
`bigfleet_aws_spot_refresh_total{outcome}`, and each underlying call shows up as
`bigfleet_aws_ec2_api_calls_total{op="DescribeSpotPriceHistory"}`. See
[Observability](/providers/aws/observability/).

## SPOT `interruption_probability`

For SPOT machines the provider publishes a real interruption probability built
from two signals (`interruption.go`): a **forecast** that always applies, raised
by an **observed** notice when one arrives.

### Forecast: Spot Instance Advisor buckets

The AWS Spot Instance Advisor publishes a per-`(region, instance-type)`
interruption-frequency bucket. `advisorBucket` is a pinned snapshot of those
buckets (`0 = <5%` … `4 = >20%`). Each bucket maps to a representative hourly
probability (`bucketProbability`), the bucket midpoint, with the open-ended top
bucket pinned to `0.30`:

| Bucket | Advisor frequency | Published probability |
|---|---|---|
| 0 | `<5%`   | `0.025` |
| 1 | `5–10%` | `0.075` |
| 2 | `10–15%` | `0.125` |
| 3 | `15–20%` | `0.175` |
| 4 | `>20%`  | `0.30` |

### Why SPOT is never `0`

A SPOT instance type that is **not** in `advisorBucket` falls back to the middle
`10–15%` bucket (`0.125`) — deliberately non-zero. Combined with the rule that
there is no `0` bucket, this guarantees every SPOT machine carries a real,
non-zero `interruption_probability`. On-demand and reserved machines report `0`
(the `forecast` function returns `0` for any non-spot capacity), which is
correct: they are not reclaimable.

### Observed: raised by a real notice

When a running spot instance receives an interruption or rebalance notice, its
observed probability is raised, and `probability` publishes the observed value
whenever it exceeds the forecast. The observed value comes from the EventBridge
detail type the [interruption poller](/providers/aws/observability/) reads off
the SQS queue:

| EventBridge detail type | Observed probability |
|---|---|
| `EC2 Spot Instance Interruption Warning` | `0.99` (the 2-minute kill notice) |
| `EC2 Instance Rebalance Recommendation` | `0.5` (elevated risk) |

The observed value is held per machine id and is clamped to `[0, 1]`. It is
cleared only once a `Delete` actuates (`TerminateInstances` succeeds), so a
machine about to be reclaimed keeps its raised probability until it is gone.

### The observed-interruption flow

The full path from an AWS notice to the number the engine scores against:

1. An EventBridge rule forwards `EC2 Spot Instance Interruption Warning` /
   `EC2 Instance Rebalance Recommendation` events to an SQS queue.
2. The provider's interruption poller (enabled with
   `--spot-interruption-queue <SQS URL>`) long-polls the queue, resolves the
   event's `instance-id` to a BigFleet machine id via the `bigfleet:machine-id`
   tag, and raises that machine's *observed* probability. The notice is counted
   on `bigfleet_aws_spot_interruptions_total`.
3. The background reconcile loop (`--reconcile-interval`, default `2m`)
   re-reads EC2 truth into kit inventory, propagating the raised value so the
   engine's victim scoring sees a real, rising probability for a machine about
   to be reclaimed.

The poller needs `sqs:ReceiveMessage` + `sqs:DeleteMessage` on the queue — see
[IAM](/providers/aws/iam/) — and is only started on the `aws` backend when
`--spot-interruption-queue` is set. Wiring the EventBridge rule and the queue is
covered in [Observability](/providers/aws/observability/).

## What is region-shaped, and what to verify

Three substrate facts could be region-shaped. Where they stand now:

| Fact | Source | Region handling |
|---|---|---|
| `allocatable` (vCPU/mem) | `ec2:DescribeInstanceTypes` | **Authoritative** — resolved live for any region; the pinned table is only an offline fallback. |
| Spot `price_per_hour` | `ec2:DescribeSpotPriceHistory` | **Authoritative** — fetched live per region, correct everywhere. |
| On-demand `price_per_hour` | `onDemandByRegion` table | Tabulated for `us-east-1`/`us-west-2`; other regions fall back to the baseline (approximate) until you regenerate. |
| Spot `interruption_probability` buckets | `advisorBucket` (`interruption.go`) | **Still pinned us-east-1 approximations** for every region. |

So the only genuinely us-east-1-shaped table left is the interruption advisor
buckets. When the `aws` backend serves any region other than `us-east-1`, it
logs a startup warning about them:

```
spot interruption-probability buckets are us-east-1 approximations; verify advisorBucket for this region
```

and `newPricing` logs its own warning if the region has no pinned on-demand
table. Both values drift over time even within us-east-1.

### How to regenerate

- **On-demand prices** — run the `genpricing` tool (see
  [above](#regenerating-a-regions-table)); it reads the public AWS Price List
  Bulk API and prints a Go map literal for `onDemandByRegion`.
- **Advisor buckets** (`advisorBucket` in `interruption.go`): refresh from the
  Spot Instance Advisor JSON feed for your region. Keep the bucket index in the
  `0`–`4` range; any type you drop falls back to the non-zero middle bucket
  rather than to `0`.

A type present in your offerings but absent from `onDemandByRegion` prices at the
zero value of the map; absent from `advisorBucket` it falls back to the middle
SPOT bucket — so keep both tables in sync with your offerings.
