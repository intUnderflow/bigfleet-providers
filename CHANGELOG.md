# Changelog

All notable changes to this repo. Versions tag every module together (see
[RELEASING.md](RELEASING.md)).

## v0.1.0

First release.

### providerkit (the shared correctness library)

Wraps a substrate-specific `Backend` (+ optional `Deleter`) with the full
`bigfleet.v1alpha1.CapacityProvider` contract so a provider only writes
substrate logic: fence-then-idempotency-then-validate ordering with
`FAILED_PRECONDITION` reserved for fencing; the `(machine_id, target_state)`
idempotency map; async dispatch with transition-timeout → `FAILED`;
restart-recovery of orphaned transitions; the `shard_metadata` store/echo/clear
lifecycle; `Machine` field-shape validation; `since_revision`; and durable
persistence via a `FileStore` (atomic write + fsync) or an in-memory store.

### AWS EC2 provider

The gold-standard reference provider: idempotent `RunInstances`
(`ClientToken`), a running-instance gate on `Create`, SSM-verified
`Configure`/`Drain`, authoritative `allocatable` from `DescribeInstanceTypes`,
region-keyed on-demand pricing with an offline generator, live spot pricing +
EventBridge/SQS interruption signals, Prometheus metrics, health + reflection,
mTLS, and deploy artifacts (distroless image, Helm chart, least-privilege IAM
Terraform). Certified against all 92 conformance behaviors.

### Conformance program

A Kubernetes-scale certification program: a frozen, append-only registry of
**92 behaviors** across 11 areas; a pure-wire extension suite; the
`bfconformance` runner (JUnit + JSON reports, profile-aware verdicts) with five
lanes — baseline, extension, fault (via a reference faultprovider), durability
(kill + restart against a `FileStore`), and scale/soak. Enforced in CI.
