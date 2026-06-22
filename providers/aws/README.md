# AWS EC2 capacity provider

A BigFleet `CapacityProvider` for **AWS EC2**. It implements only the
substrate-specific [`providerkit.Backend`](../../providerkit) (+
`Deleter`); providerkit wraps it with the full
`bigfleet.v1alpha1.CapacityProvider` contract — fencing, idempotency, async
dispatch, transition timeouts, the `shard_metadata` lifecycle, the `Machine`
field-shape, and `since_revision`. This provider never re-implements any of
that; it only maps the kit's lifecycle calls onto EC2 and fills in the
substrate fields (`instance_type`, `zone`, `capacity_type`, `price_per_hour`,
`interruption_probability`, `resources`, `allocatable`, `host`).

> **📖 Operator documentation:** the full operator guide — install & deploy,
> configuration, IAM, pricing & interruption, observability, security,
> troubleshooting, and certification — lives at
> **<https://bigfleet-providers.lucy.sh/providers/aws/>**. The page sources are
> in [`docs/`](docs) and are published to the site automatically. This README is
> the quick repo-facing reference.

## Running it

```sh
make build-aws
./bin/aws --region us-east-1 --provider aws-us-east-1 \
          --ami ami-0123456789abcdef0 \
          --subnets us-east-1a=subnet-aaa,us-east-1b=subnet-bbb \
          --security-groups sg-0123 --iam-instance-profile bigfleet-node \
          --state /var/lib/bigfleet-aws/state.json \
          --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

### EC2 backend modes

`--ec2-backend` selects the substrate client:

- `aws` — the real EC2 client (requires `--region` and `--ami`).
- `fake` — an in-memory simulator (dev + the credential-free conformance run).
- `auto` (default) — `aws` when `--region` is set, otherwise `fake` (with a loud warning).

So a bare `./bin/aws --seed-count 32` (no `--region`) comes up on the fake
backend — which is exactly how `make conformance-aws` runs credential-free.

### Key flags

| flag | meaning |
|---|---|
| `--addr` | gRPC listen address (default `:9000`) |
| `--region` | AWS region (required for the `aws` backend; one process per region) |
| `--provider` | label stamped on `HostRef.provider` (e.g. `aws-us-east-1`) |
| `--ec2-backend` | `aws` \| `fake` \| `auto` (default `auto`; see modes above) |
| `--offerings` | JSON offerings file (else a built-in mix sized by `--seed-count`) |
| `--seed-count` | Speculative slots for the default offerings |
| `--zone-a` / `--zone-b` | AZs for the default offerings (default `<region>a` / `<region>b`) |
| `--base-user-data` | file with the generic pre-binding bootstrap baked in at launch |
| `--ami` | base AMI for `RunInstances` |
| `--subnets` | `zone=subnet-id` comma list |
| `--security-groups` / `--iam-instance-profile` / `--key-name` | launch params |
| `--bootstrap-hook` | AMI path that applies the delivered bootstrap (default `/opt/bigfleet/bootstrap`) |
| `--state` | durable state file (empty = in-memory) |
| `--tls-cert/--tls-key/--tls-ca` | TLS / mTLS |
| `--spot-refresh` | spot price refresh interval (default 5m) |
| `--ondemand-refresh` | on-demand price refresh interval from the AWS Price List Bulk API (default 60m; 0 = seed table only) |

### Offerings

The cloud analogue of a free pool: each offering is `(instance_type, zone,
capacity_type)` with a `count` of Speculative slots the shard may `Create`.
`--offerings` takes a JSON array:

```json
[
  { "instance_type": "m6i.4xlarge", "zone": "us-east-1a", "capacity_type": "on_demand",
    "count": 20, "resources": { "cpu": "1", "memory": "4Gi" } },
  { "instance_type": "g5.2xlarge",  "zone": "us-east-1b", "capacity_type": "spot",
    "count": 8,  "resources": { "nvidia.com/gpu": "1" } }
]
```

`resources` is the **per-replica request shape** the offering serves (what one
Pod needs); `allocatable` is filled from the instance type's real capacity (see
`instancetypes.go`). Do not confuse them.

## IAM policy

The provider's role/instance-profile needs:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": [
        "ec2:RunInstances",
        "ec2:TerminateInstances",
        "ec2:DescribeInstances",
        "ec2:DescribeSpotPriceHistory",
        "ec2:CreateTags",
        "ec2:DeleteTags"
      ], "Resource": "*" },
    { "Effect": "Allow", "Action": ["ssm:SendCommand"], "Resource": "*" },
    { "Effect": "Allow", "Action": ["iam:PassRole"],
      "Resource": "arn:aws:iam::<account-id>:role/<node-role>",
      "Condition": { "StringEquals": { "iam:PassedToService": "ec2.amazonaws.com" } } }
  ]
}
```

Use the AWS credential chain (IRSA on EKS, instance profile, or `AWS_*` env) —
nothing is hardcoded. **`iam:PassRole`** is required only when
`--iam-instance-profile` is set: `RunInstances` passes that profile's role to
EC2, and AWS rejects the call without `PassRole` on it (scope `Resource` to the
node role). The **node** instance profile (`--iam-instance-profile`) must allow
SSM so `Configure`/`Drain` can reach it; scope the other `Resource`s down with
tag conditions in production.

## Pricing

Both prices are live-refreshed on background timers into in-memory caches; the
`List`/seed path only ever reads the cache, never the network.

- **On-demand**: live from the **public AWS Price List Bulk API** (region offer
  JSON, no credentials), refreshed on `--ondemand-refresh` (default `60m`). The
  pinned `onDemandByRegion` table (`pricing.go`) is the **seed/fallback** —
  it floors a price before the first refresh and backstops a failed/missing one,
  so a successful refresh never zeroes a price. An `on_demand`/`reserved`
  offering whose type has no live **and** no seed price makes the provider
  **fail closed at startup** (a `0` would win the cost ranking). Regenerate the
  seed table with `go run ./cmd/genpricing`.
- **Spot**: the current price from `ec2:DescribeSpotPriceHistory`, cached and
  refreshed on `--spot-refresh`, never fetched per `List`. Cold-cache reads fall
  back to a conservative fraction of on-demand until the first refresh.

## Interruption probability (SPOT correctness)

`effective_cost = price + interruption_probability × penalty`, so a SPOT machine
that reports `0` corrupts the engine's decisions. This provider **never** ships
`0` for spot:

- **Forecast**: the AWS Spot Instance Advisor interruption-frequency bucket per
  instance type (`interruption.go`, `advisorBucket` — a pinned snapshot;
  refresh from the advisor JSON feed). An unknown spot type falls back to a
  non-zero middle bucket.
- **Observed**: `markWarning` raises a running instance's probability toward
  `1.0` once it receives a rebalance recommendation / the 2-minute
  spot-interruption notice. Wire it to an EventBridge spot-interruption rule (or
  the node's IMDS `spot/instance-action`) in production.

On-demand / reserved report `0`.

## Bootstrap delivery — and a divergence from the brief

The provider brief sketches "Option A: launch-at-Configure" so the kubelet join
data can be baked into immutable EC2 user-data. **That conflicts with
providerkit's state machine**, where `Create` must produce a *real, Idle host*
(the kit attaches the `HostRef` `CreateInstance` returns). So this provider uses
the brief's **Option B**:

- `CreateInstance` → `RunInstances` with a generic, cluster-agnostic base AMI +
  `--base-user-data`. The instance boots to Idle.
- `ConfigureInstance` → tags `bigfleet:cluster` and delivers the opaque
  `bootstrap_blob` via **SSM SendCommand**, which runs the AMI's
  `--bootstrap-hook` to join the cluster (the AMI must ship that hook).
- `DrainInstance` → removes the binding tag and runs cordon+drain via SSM.

This is the only material divergence from the brief; the proto and author guide
are otherwise followed exactly. Instances are tagged `bigfleet:managed=true`,
`bigfleet:machine-id`, and `bigfleet:cluster` so `DescribeInstances` recovers
inventory and never touches non-BigFleet instances.

## Conformance

```sh
make conformance-aws            # boots on the fake EC2 backend, runs the bigfleet suite
BIGFLEET_SRC=/path/to/bigfleet make conformance-aws   # reuse a checkout
```

The suite passes 24/24 against the in-memory EC2 simulator, proving the Backend
+ providerkit are contract-correct end-to-end without AWS credentials.

## Live EC2 demo (requires AWS credentials)

The end-to-end demo against real EC2 is **not run here** (no AWS credentials in
this environment). To run it yourself:

1. Build an AMI shipping `/opt/bigfleet/bootstrap` (consumes `<hook>.blob` +
   the cluster id and joins the kubelet) with the SSM agent enabled.
2. Boot the provider with `--region`, `--ami`, `--subnets`,
   `--iam-instance-profile`, and an `--offerings` file.
3. Drive one machine `Create → Get/List` until `IDLE` (a real `i-…` exists),
   `Configure` (node joins, `cluster` + `shard_metadata` echoed), `Drain`
   (cluster + metadata cleared), `Delete` (`TerminateInstances`, back to
   `SPECULATIVE`). Bring up one SPOT machine and confirm its
   `interruption_probability` is non-zero.

## Persistence

Pass `--state` so the providerkit store persists fence marks, the idempotency
map, and inventory + bindings + `shard_metadata` across restarts. Without it the
provider is in-memory and recovers running instances from EC2 tags on the next
seed (bindings are not recoverable from tags alone — use `--state` in
production).
