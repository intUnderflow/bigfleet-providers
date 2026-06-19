---
title: Configuration
description: Every flag, the offerings JSON schema, the three EC2 backend modes, and the launch-then-bootstrap (SSM) model for the BigFleet AWS EC2 provider.
sidebar:
  order: 2
  label: Configuration
---

The provider is configured entirely by command-line flags. One process serves
one AWS region: you give it a quota of capacity it may provision (the
**offerings**), a base AMI + networking to launch into, and the addresses it
listens on. Everything else (fencing, idempotency, transition timeouts) is
handled by the shared `providerkit` and is not configurable here.

This page is the flag reference, the offerings schema, the backend modes, and
the launch-then-bootstrap contract your AMI must satisfy. For the IAM the flags
imply see [IAM](/providers/aws/iam/); for how price and interruption are
sourced see [Pricing & interruption](/providers/aws/pricing-and-interruption/).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address (CapacityProvider + health + reflection). |
| `--provider` | `aws` | Provider/region label stamped on every `HostRef` (e.g. `aws-us-east-1`). |
| `--region` | _(empty)_ | AWS region. Required for the `aws` backend; also what flips `auto` to `aws`. |
| `--ec2-backend` | `auto` | `aws` \| `fake` \| `auto`. `auto` = `aws` when `--region` is set, else `fake`. See [Backend modes](#backend-modes). |
| `--offerings` | _(built-in)_ | Path to a JSON offerings file. Omit to use a built-in mix sized by `--seed-count`. |
| `--seed-count` | `32` | Number of Speculative slots in the default offerings (ignored when `--offerings` is set). |
| `--zone-a` | `<region>a` | First AZ for the default offerings. |
| `--zone-b` | `<region>b` | Second AZ for the default offerings. |
| `--state` | _(empty)_ | Durable state file. Empty = in-memory only (state is lost on restart). |
| `--ami` | _(empty)_ | Base AMI id for `RunInstances`. **Required** for the `aws` backend. |
| `--subnets` | _(empty)_ | Comma list of `zone=subnet-id` (e.g. `us-east-1a=subnet-aaa,us-east-1b=subnet-bbb`). |
| `--security-groups` | _(empty)_ | Comma list of security group ids. |
| `--iam-instance-profile` | _(empty)_ | Instance profile name attached to launched instances. Needs SSM (see [bootstrap model](#launch-then-bootstrap)). Enables `iam:PassRole`. |
| `--key-name` | _(empty)_ | Optional EC2 SSH key name. |
| `--bootstrap-hook` | `/opt/bigfleet/bootstrap` | AMI path that consumes the delivered bootstrap blob and joins the cluster. See [the AMI contract](#the-ami-hook-contract). |
| `--base-user-data` | _(empty)_ | Path to the generic, pre-binding bootstrap baked into user-data at launch. |
| `--spot-refresh` | `5m` | Spot price refresh interval (never on the List hot path). |
| `--spot-interruption-queue` | _(empty)_ | SQS queue URL fed by an EventBridge spot-interruption/rebalance rule. Enables observed-interruption escalation + `sqs:ReceiveMessage`/`sqs:DeleteMessage`. |
| `--reconcile-interval` | `2m` | Background EC2→inventory reconcile interval (`0` = off). |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz`. Empty = disabled. |
| `--reflection` | `true` | Register gRPC server reflection (for `grpcurl`/debugging). |
| `--tls-cert` | _(empty)_ | Server certificate (PEM). With `--tls-key`, enables TLS. |
| `--tls-key` | _(empty)_ | Server private key (PEM). |
| `--tls-ca` | _(empty)_ | Client CA bundle (PEM). Enables mTLS (requires + verifies client certs). |

A minimal production invocation:

```sh
./bin/aws \
  --region us-east-1 --provider aws-us-east-1 \
  --ami ami-0123456789abcdef0 \
  --subnets us-east-1a=subnet-aaa,us-east-1b=subnet-bbb \
  --security-groups sg-0123456789abcdef0 \
  --iam-instance-profile bigfleet-node \
  --offerings /etc/bigfleet/offerings.json \
  --state /var/lib/bigfleet-aws/state.json \
  --spot-interruption-queue https://sqs.us-east-1.amazonaws.com/111122223333/bigfleet-spot \
  --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

:::note
The pinned pricing, interruption, and instance-type tables are `us-east-1`
approximations. Running another region logs a startup warning — verify those
tables for your region. See [Pricing & interruption](/providers/aws/pricing-and-interruption/).
:::

## Backend modes

`--ec2-backend` selects the substrate client:

- **`aws`** — the real EC2 client backed by `aws-sdk-go-v2`. Requires `--region`
  **and** `--ami`; startup fails without them. This is what creates real
  instances and runs real SSM commands.
- **`fake`** — an in-memory simulator. No AWS account, credentials, or network
  needed; no real instances are created. Used for dev and the credential-free
  conformance run. Selecting it logs a loud warning so it is never mistaken for
  production.
- **`auto`** (default) — resolves to `aws` when `--region` is set, otherwise
  `fake`.

So a bare `./bin/aws --seed-count 32` (no `--region`) comes up on the fake
backend — exactly how `make conformance-aws` runs credential-free — while
setting `--region` opts you into the real backend.

## Offerings

An **offering** is one shape of capacity the provider is allowed to provision:
an instance type, in a zone, at a capacity type, up to `count` slots. Each open
slot is a **Speculative** `Machine` the shard can actuate (the cloud analogue
of a free pool). The offerings are the provider's entire quota — it will never
launch a type/zone/capacity combination you did not list.

Pass a JSON file with `--offerings`. The file is a JSON array of objects:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `instance_type` | string | yes | EC2 instance type, e.g. `m6i.large`. |
| `zone` | string | yes | Availability zone, e.g. `us-east-1a`. Zoneless offerings are rejected at startup (the provider is multi-zone). |
| `capacity_type` | string | no | `on_demand` (default) \| `spot` \| `reserved` \| `bare_metal`. Empty = `on_demand`. |
| `count` | int | yes | Number of Speculative slots this offering provides. |
| `resources` | map[string]string | no | The per-replica request shape the offering serves (the `Machine.resources`). Distinct from `allocatable`, which is derived from the instance type. |
| `labels` | map[string]string | no | Extra labels carried on the slot. GPU families also get an automatic `bigfleet.io/accelerator` label. |

`capacity_type` accepts a few spellings (`on-demand`/`ondemand`,
`bare-metal`/`metal`); the canonical forms are above. An unknown value fails
startup.

Example `offerings.json`:

```json
[
  {
    "instance_type": "m6i.large",
    "zone": "us-east-1a",
    "capacity_type": "on_demand",
    "count": 8,
    "resources": { "cpu": "1", "memory": "2Gi" }
  },
  {
    "instance_type": "c7g.xlarge",
    "zone": "us-east-1a",
    "capacity_type": "spot",
    "count": 16,
    "resources": { "cpu": "1", "memory": "2Gi" }
  },
  {
    "instance_type": "g5.xlarge",
    "zone": "us-east-1b",
    "capacity_type": "on_demand",
    "count": 4,
    "resources": { "cpu": "3", "memory": "12Gi", "nvidia.com/gpu": "1" },
    "labels": { "team": "ml" }
  }
]
```

GPU families get an accelerator label automatically: `g5.*`/`g6.*` →
`bigfleet.io/accelerator=nvidia-a10g`, `p4*`/`p5*` → `nvidia-a100`. You do not
need to set it yourself; the `g5.xlarge` offering above will carry both `team`
and the accelerator label.

If you omit `--offerings`, the provider synthesizes a representative mix of
on-demand and spot `m6i.large`/`c7g.xlarge` slots across `--zone-a`/`--zone-b`,
distributing `--seed-count` slots evenly. That default is for dev and
conformance; **real deployments supply `--offerings`.**

Shrinking an offering (or removing it) does not terminate live instances: a
tagged, running instance keeps owning its slot, and any tagged instance with no
matching offering is surfaced as Idle under its machine id rather than being
lost.

## Launch then bootstrap

The provider deliberately splits **launch** from **cluster join**, because EC2
user-data is immutable after launch but a slot's target cluster is only known
when the shard binds it. The lifecycle:

1. **Create → `RunInstances`.** Launches the instance from `--ami` with
   `--base-user-data` as user-data, the chosen subnet/placement, security
   groups, instance profile, and the BigFleet tags (`bigfleet:managed`,
   `bigfleet:machine-id`, `bigfleet:capacity`). The operation id is the
   `ClientToken`, so a retried Create within EC2's idempotency window returns
   the same instance instead of launching a second one. Spot offerings launch
   with one-time spot market options. **Create blocks until the instance is
   actually `running`** before returning Idle, so the immediately following
   Configure never races a still-pending host.
2. **Configure → SSM.** Tags the instance `bigfleet:cluster=<id>`, then delivers
   the opaque `bootstrap_blob` to the node via SSM `SendCommand` and runs the
   AMI hook. It **polls `GetCommandInvocation` until the command succeeds** — a
   failed bootstrap becomes `FAILED`, never a false Configured.
3. **Drain → SSM.** Removes the `bigfleet:cluster` tag and cordons/drains the
   kubelet off the node via SSM, again waiting for success. A drain that does
   not complete surfaces as `FAILED`, not a false Idle. The instance is left
   running but unbound.
4. **Delete → `TerminateInstances`.** The slot returns to Speculative.

Because Configure and Drain ride on SSM, the **node instance profile must grant
SSM** (`AmazonSSMManagedInstanceCore`) so the commands can reach the instance.
Set it with `--iam-instance-profile`. See [IAM](/providers/aws/iam/) for the
exact policy and the `iam:PassRole` condition this implies.

### The AMI hook contract

Configure does not bake cluster join logic into the provider — it delivers an
opaque blob and runs a hook your AMI ships. The contract:

- The AMI must ship an executable at `--bootstrap-hook` (default
  `/opt/bigfleet/bootstrap`).
- Configure writes the decoded bootstrap blob next to the hook, at
  `<hook>.blob` (e.g. `/opt/bigfleet/bootstrap.blob`), with restrictive
  permissions (`umask 077`).
- It then invokes the hook with the cluster id as its single argument:
  `<hook> <cluster-id>`.
- The hook is responsible for reading `<hook>.blob`, joining the cluster, and
  **exiting non-zero on any failure** — a non-zero exit is what turns a botched
  bootstrap into a `FAILED` machine instead of a silently-broken node.

A minimal hook skeleton:

```sh
#!/usr/bin/env bash
# /opt/bigfleet/bootstrap — invoked as: bootstrap <cluster-id>
set -euo pipefail
cluster_id="$1"
blob="$(dirname "$0")/$(basename "$0").blob"   # /opt/bigfleet/bootstrap.blob

# The blob is opaque to the provider; interpret it however your join flow needs
# (kubeadm join args, a config bundle, secrets, etc.) and join the cluster.
join-the-cluster --cluster "$cluster_id" --config "$blob"
```

Drain assumes `kubectl` is available on the node and that the Kubernetes node
name matches the instance's private DNS name (`hostname -f`), which is the
default with the AWS cloud provider. Bake `kubectl` into the AMI so Drain can
cordon and drain.

Use `--base-user-data` for anything that must run at launch, before any cluster
is chosen — installing the hook's dependencies, pulling images, configuring the
kubelet's static bits. It is generic by design: the same blob runs on every
slot, regardless of which cluster eventually binds it.
