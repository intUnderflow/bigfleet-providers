---
title: Install & deploy
description: Run the AWS EC2 provider — the container image, the Helm chart, flags, mTLS, and running it on EKS with IRSA.
sidebar:
  order: 1
  label: Install & deploy
---

The AWS EC2 provider is **one process per region**. You run it next to BigFleet,
point it at a base AMI + subnets, give it AWS credentials, and BigFleet dials its
`--addr`. This page covers the container image, the Helm chart, the flags you
actually need, mTLS, and the EKS + IRSA path.

If you just want to kick the tyres with no AWS account, the
[overview](/providers/aws/) shows the credential-free fake backend. Everything
below is for a real region.

## Container image

The binary is a single static Go binary; the image is built from
[`deploy/Dockerfile`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/aws/deploy/Dockerfile)
(distroless, non-root, no shell). Build and push it:

```sh
# Build from the repository root: the providers/aws module's `replace => ../..`
# needs the whole repo in context to resolve the providerkit (root) module.
docker build -t ghcr.io/your-org/bigfleet-aws:latest \
  -f providers/aws/deploy/Dockerfile .
docker push ghcr.io/your-org/bigfleet-aws:latest
```

The entrypoint is the provider binary, so you pass [flags](#flags) as container
args. A bare smoke test (fake backend, no credentials) confirms the image runs:

```sh
docker run --rm -p 9000:9000 -p 9090:9090 \
  ghcr.io/your-org/bigfleet-aws:latest \
  --seed-count 32 --addr :9000 --metrics-addr :9090
# then: curl localhost:9090/healthz  -> ok
#       curl localhost:9090/readyz   -> ready
```

The container exposes two ports:

| Port | Flag | Serves |
|---|---|---|
| `9000` | `--addr` | gRPC `CapacityProvider` + `grpc.health.v1` + reflection |
| `9090` | `--metrics-addr` | HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) |

See [Observability](/providers/aws/observability/) for what `/metrics` exposes
and [Security](/providers/aws/security/) for the mTLS posture.

## Helm chart

The chart lives at
[`deploy/helm/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/aws/deploy/helm).
It renders a `Deployment` (single replica — one process per region, owns its
`--state`), a `Service` exposing the gRPC + metrics ports (with Prometheus
scrape annotations), a `ServiceAccount` (for IRSA), and — when enabled — a
`ConfigMap` for the offerings and a `PersistentVolumeClaim` for durable state.

Install it with a values file per region:

```sh
helm install bigfleet-aws-use1 providers/aws/deploy/helm \
  -n bigfleet --create-namespace \
  -f us-east-1.values.yaml
```

A minimal `us-east-1.values.yaml`:

The values are **structured** — you set fields like `region` and `ec2.ami` and
the chart turns them into the right flags. A minimal `us-east-1.values.yaml`:

```yaml
image:
  repository: ghcr.io/your-org/bigfleet-aws
  tag: latest

# One process per region. `region` sets --region; `provider` is the label
# stamped on every HostRef.
region: us-east-1
provider: aws-us-east-1

# EC2 launch parameters.
ec2:
  ami: ami-0123456789abcdef0
  subnets: us-east-1a=subnet-aaa,us-east-1b=subnet-bbb
  securityGroups: sg-0123456789abcdef0
  iamInstanceProfile: bigfleet-node

# Durable state on a PersistentVolume: fence marks, the idempotency map, and
# bindings survive restarts. Without it the provider is in-memory only.
state:
  enabled: true
  persistence:
    enabled: true
    size: 1Gi

# IRSA: the chart annotates the ServiceAccount with this role (see below).
serviceAccount:
  roleArn: arn:aws:iam::111122223333:role/bigfleet-aws-provider
```

The offerings JSON is delivered through `offerings.content`: set it and the
chart renders the JSON into a ConfigMap, mounts it at
`/etc/bigfleet/offerings/offerings.json`, and passes `--offerings`. Use
`--set-file` so you keep the file out of your values:

```sh
helm install bigfleet-aws-use1 providers/aws/deploy/helm \
  -n bigfleet --create-namespace \
  -f us-east-1.values.yaml \
  --set-file offerings.content=offerings.us-east-1.json
```

The offerings shape is documented in
[Configuration](/providers/aws/configuration/). Always enable durable `state` on
a PersistentVolume in production — without it the provider is in-memory and
cannot recover bindings on restart.

## Flags

Every flag the binary accepts, grouped by what you touch first. The full
reference (defaults, semantics, the bootstrap model) is in
[Configuration](/providers/aws/configuration/).

**Core**

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address |
| `--provider` | `aws` | Label stamped on `HostRef.provider` (e.g. `aws-us-east-1`) |
| `--region` | _(empty)_ | AWS region; **required** for the `aws` backend (one process per region) |
| `--ec2-backend` | `auto` | `aws` \| `fake` \| `auto` (auto = `aws` when `--region` is set, else `fake`) |
| `--state` | _(empty)_ | Durable state file; empty = in-memory only |

**Launch parameters (aws backend)**

| Flag | Default | Meaning |
|---|---|---|
| `--ami` | _(empty)_ | Base AMI for `RunInstances` |
| `--subnets` | _(empty)_ | Comma list of `zone=subnet-id` |
| `--security-groups` | _(empty)_ | Comma list of security group ids |
| `--iam-instance-profile` | _(empty)_ | Instance profile granting the node SSM (triggers `iam:PassRole`) |
| `--key-name` | _(empty)_ | Optional EC2 SSH key name |
| `--bootstrap-hook` | `/opt/bigfleet/bootstrap` | AMI path that applies the delivered bootstrap blob |
| `--base-user-data` | _(empty)_ | File with the generic pre-binding bootstrap baked in at launch |

**Offerings**

| Flag | Default | Meaning |
|---|---|---|
| `--offerings` | _(empty)_ | JSON offerings file (else a built-in mix sized by `--seed-count`) |
| `--seed-count` | `32` | Speculative slots for the default offerings |
| `--zone-a` / `--zone-b` | `<region>a` / `<region>b` | AZs for the default offerings |

**Pricing & interruption**

| Flag | Default | Meaning |
|---|---|---|
| `--spot-refresh` | `5m` | Spot price refresh interval |
| `--spot-interruption-queue` | _(empty)_ | SQS URL fed by an EventBridge spot-interruption/rebalance rule (raises observed interruption probability) |
| `--reconcile-interval` | `2m` | Background EC2→inventory reconcile interval (`0` = off) |

**Observability & TLS**

| Flag | Default | Meaning |
|---|---|---|
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz` (empty = disabled) |
| `--reflection` | `true` | Register gRPC server reflection (for grpcurl/debugging) |
| `--tls-cert` / `--tls-key` | _(empty)_ | Server certificate + key (PEM); enables TLS |
| `--tls-ca` | _(empty)_ | Client CA bundle (PEM); enables mTLS |

## mTLS

With no `--tls-cert`/`--tls-key` the provider serves **insecure** gRPC — fine
only for trusted in-cluster traffic. For production, terminate mTLS in the
provider itself:

- `--tls-cert` + `--tls-key` enable TLS (TLS 1.3 minimum).
- adding `--tls-ca` (a client CA bundle) enables **mTLS**: the provider then
  requires and verifies a client certificate on every connection.

`--tls-ca` without `--tls-cert`/`--tls-key` is rejected, and supplying only one
of cert/key is rejected — so a half-configured TLS setup fails fast at startup
rather than silently serving plaintext.

The chart mounts a standard Kubernetes TLS Secret at `/etc/bigfleet/tls` and
wires `--tls-cert`/`--tls-key` (and `--tls-ca` when `mtls` is set) for you — you
only point it at the Secret:

```yaml
tls:
  enabled: true
  mtls: true                     # mount ca.crt and require a verified client cert
  secretName: bigfleet-aws-tls   # Secret keys: tls.crt, tls.key, ca.crt
```

Create the Secret with the standard TLS keys (`ca.crt` is only needed for mTLS):

```sh
kubectl -n bigfleet create secret generic bigfleet-aws-tls \
  --from-file=tls.crt=server.pem \
  --from-file=tls.key=server-key.pem \
  --from-file=ca.crt=client-ca.pem
```

BigFleet must then present a client certificate signed by `ca.crt` when it dials
the provider. The startup log line reports the negotiated mode
(`insecure` / `TLS` / `mTLS`) so you can confirm what is actually serving. The
full trust model is in [Security](/providers/aws/security/).

## Running on EKS with IRSA

On EKS, give the provider AWS credentials with **IRSA** (IAM Roles for Service
Accounts) rather than node instance profiles or static keys — the SDK picks up
the projected token automatically, nothing is hardcoded.

**1. Create the provider role** with the trust policy bound to the cluster OIDC
provider and the provider's ServiceAccount. The exact permissions policy lives
at
[`deploy/iam/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/aws/deploy/iam)
and is documented field-by-field in [IAM](/providers/aws/iam/). In short the
role needs `ec2:RunInstances`, `ec2:TerminateInstances`, `ec2:DescribeInstances`,
`ec2:DescribeSpotPriceHistory`, `ec2:CreateTags`, `ec2:DeleteTags`,
`ssm:SendCommand`, and `ssm:GetCommandInvocation`. Two permissions are
conditional:

- `iam:PassRole` — **only** when `--iam-instance-profile` is set. Scope it to the
  node role and add `"iam:PassedToService": "ec2.amazonaws.com"`.
- `sqs:ReceiveMessage` + `sqs:DeleteMessage` — **only** when
  `--spot-interruption-queue` is set.

The Terraform at `deploy/iam` provisions exactly this — the role, the
least-privilege policy, and (by default, `trust_mode = irsa`) the web-identity
trust to your ServiceAccount. Apply it with your cluster's OIDC details:

```sh
terraform -chdir=providers/aws/deploy/iam apply \
  -var name=bigfleet-aws-us-east-1 \
  -var oidc_provider_arn=arn:aws:iam::111122223333:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE \
  -var oidc_provider_url=oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE \
  -var service_account_namespace=bigfleet \
  -var service_account_name=bigfleet-aws-use1 \
  -var node_role_arn=arn:aws:iam::111122223333:role/bigfleet-node
# outputs role_arn
```

**2. Bind the role to the chart's ServiceAccount.** The chart creates the
ServiceAccount; point `serviceAccount.roleArn` at the Terraform `role_arn`
output and it stamps the `eks.amazonaws.com/role-arn` annotation EKS reads:

```yaml
serviceAccount:
  name: bigfleet-aws-use1
  roleArn: arn:aws:iam::111122223333:role/bigfleet-aws-us-east-1-role
```

**3. Give the node role SSM.** `Configure` and `Drain` reach instances over SSM
`SendCommand` + `GetCommandInvocation`, so the **node** instance profile you
pass to `--iam-instance-profile` must allow SSM — attach
`AmazonSSMManagedInstanceCore`. Without it `Configure` cannot deliver the
bootstrap blob and the machine ends up `FAILED`.

**4. Install** and watch it come up:

```sh
helm install bigfleet-aws-use1 providers/aws/deploy/helm \
  -n bigfleet -f us-east-1.values.yaml

kubectl -n bigfleet logs deploy/bigfleet-aws-use1 | grep 'serving CapacityProvider'
kubectl -n bigfleet port-forward deploy/bigfleet-aws-use1 9090:9090 &
curl localhost:9090/readyz   # -> ready once the spot cache is warm and gRPC is serving
```

The pod reports `/readyz` green only after the gRPC server is serving, so wire
it to a readiness probe and let BigFleet dial the `Service` once the probe
passes. From here, see [Configuration](/providers/aws/configuration/) for
offerings and the bootstrap model, and
[Pricing & interruption](/providers/aws/pricing-and-interruption/) for the spot
feed.
