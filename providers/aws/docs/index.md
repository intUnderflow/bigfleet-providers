---
title: AWS EC2 provider
description: The BigFleet capacity provider for AWS EC2 — create, configure, drain, and delete EC2 instances on demand.
sidebar:
  order: 0
  label: AWS overview
---

The **AWS EC2 provider** is a BigFleet `CapacityProvider` that creates,
configures, drains, and deletes EC2 instances on demand. It supports on-demand,
spot, and reserved capacity, with idempotent launches, a running-instance gate
on `Create`, SSM-verified `Configure`/`Drain`, live pricing and interruption
signals, and full health + metrics instrumentation.

It implements only the substrate-specific `providerkit.Backend`; the shared
[`providerkit`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providerkit)
library handles fencing, idempotency, async dispatch, transition timeouts, the
`shard_metadata` lifecycle, `since_revision`, and the `Machine` field shape. The
provider's job is the EC2 mapping and the substrate facts (`instance_type`,
`zone`, `capacity_type`, `price_per_hour`, `interruption_probability`,
`resources`, `allocatable`, `host`).

## At a glance

- **One process per region.** Configure offerings (the quota of slots it may
  provision), point it at a base AMI + subnets, and BigFleet dials its `--addr`.
- **Capacity types:** on-demand, spot, and reserved. Spot machines always carry
  a real, non-zero `interruption_probability` (forecast from the Spot Instance
  Advisor, raised on an observed interruption notice).
- **Correct by construction.** `RunInstances` is idempotent (`ClientToken`),
  `Create` blocks until the instance is actually running, and `Configure`/`Drain`
  confirm their SSM commands *succeeded* — a failed bootstrap or drain becomes
  `FAILED`, never a false Configured/Idle.
- **Production-ready.** gRPC health + reflection, Prometheus metrics, structured
  logging + panic recovery, `/healthz` + `/readyz`, a background reconcile loop,
  and adaptive SDK retries.

## Operator guide

| Page | What it covers |
|---|---|
| [Install & deploy](/providers/aws/install/) | Docker image, Helm chart, flags, mTLS, running it on EKS (IRSA) |
| [Configuration](/providers/aws/configuration/) | Offerings, the backend modes, every flag, the bootstrap model |
| [IAM](/providers/aws/iam/) | The exact IAM policy (incl. `iam:PassRole`), IRSA, the node profile |
| [Pricing & interruption](/providers/aws/pricing-and-interruption/) | How price and SPOT interruption probability are sourced and refreshed |
| [Observability](/providers/aws/observability/) | Metrics, health/readiness, logging, the EventBridge → SQS interruption feed |
| [Security](/providers/aws/security/) | mTLS, least-privilege IAM, the SSM bootstrap trust model |
| [Troubleshooting](/providers/aws/troubleshooting/) | Common failure modes and how to diagnose them from metrics/logs |
| [Certification](/providers/aws/certification/) | Running the conformance + extension suites against this provider |

## Quick start (dev / fake backend)

```sh
# No AWS account needed: the in-memory backend seeds Speculative slots.
make build-aws
./bin/aws --seed-count 32 --addr :9000 --metrics-addr :9090
```

For a real deployment, see [Install & deploy](/providers/aws/install/).
