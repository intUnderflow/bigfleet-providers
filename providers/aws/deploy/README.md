# Deploying the AWS EC2 provider

Production deploy artifacts for the BigFleet AWS EC2 capacity provider: a
container image, a Helm chart, and the IAM (policy + Terraform).

The provider follows a **one-process-per-region** model. Each process owns a
single AWS region (`--region`), holds region-scoped inventory/state, and is the
single `CapacityProvider` for that region. To cover several regions, deploy the
chart once per region with a distinct release name, region, IRSA role, and
offerings file — never scale a single release past `replicas: 1`.

## 1. Build the image

The `providers/aws` Go module uses a `replace ... => ../..` to resolve the
shared `providerkit` (repo-root) module from the local checkout, so the build
needs the **whole repository** in context. Build from the repo root:

```sh
# from the repository root
docker build -f providers/aws/deploy/Dockerfile -t ghcr.io/intunderflow/bigfleet-aws:0.1.0 .
docker push ghcr.io/intunderflow/bigfleet-aws:0.1.0
```

The multi-stage build compiles with `go -C providers/aws build -o /out/aws .`
and ships a `distroless/static:nonroot` final image (uid 65532, no shell). It
exposes the gRPC port (`9000`) and the metrics/health port (`9090`).

## 2. Create the IAM role

The provider authenticates via the standard AWS credential chain — use **IRSA**
on EKS (preferred), an instance profile on plain EC2, or `AWS_*` env locally.
Nothing is hardcoded.

The least-privilege policy lives in [`iam/policy.json`](iam/policy.json); the
Terraform in [`iam/main.tf`](iam/main.tf) creates the role + policy + trust.

```sh
cd providers/aws/deploy/iam
terraform init
terraform apply \
  -var 'name=bigfleet-aws-us-east-1' \
  -var 'trust_mode=irsa' \
  -var 'oidc_provider_arn=arn:aws:iam::111122223333:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE' \
  -var 'oidc_provider_url=oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE' \
  -var 'service_account_namespace=bigfleet' \
  -var 'service_account_name=bigfleet-aws' \
  -var 'node_role_arn=arn:aws:iam::111122223333:role/bigfleet-node' \
  -var 'spot_interruption_queue_arn=arn:aws:sqs:us-east-1:111122223333:bigfleet-spot-interruptions'
# -> outputs role_arn
```

What the policy grants, and why (each line maps to a call the code makes):

| Statement | Actions | When |
|---|---|---|
| EC2 lifecycle + inventory | `ec2:RunInstances`, `ec2:TerminateInstances`, `ec2:DescribeInstances`, `ec2:DescribeSpotPriceHistory`, `ec2:CreateTags`, `ec2:DeleteTags` | always |
| SSM bootstrap + drain | `ssm:SendCommand`, `ssm:GetCommandInvocation` | always (Configure/Drain) |
| Pass node role to EC2 | `iam:PassRole` (scoped to the node role, `iam:PassedToService=ec2.amazonaws.com`) | only with `--iam-instance-profile` (omit `node_role_arn` otherwise) |
| Spot interruption queue | `sqs:ReceiveMessage`, `sqs:DeleteMessage` | only with `--spot-interruption-queue` (omit `spot_interruption_queue_arn` otherwise) |

The **node** instance profile (`--iam-instance-profile`, a separate role) must
carry `AmazonSSMManagedInstanceCore` so the SSM agent on each launched instance
can receive the Configure/Drain commands. Drop the PassRole and SQS variables to
omit those statements when the corresponding flags are unset.

## 3. Install the chart

Write an offerings file (see the provider README for the schema), then install
one release per region, pointing `serviceAccount.roleArn` at the IRSA role from
step 2:

```sh
helm install aws-us-east-1 providers/aws/deploy/helm \
  --namespace bigfleet --create-namespace \
  --set image.repository=ghcr.io/intunderflow/bigfleet-aws \
  --set image.tag=0.1.0 \
  --set region=us-east-1 \
  --set provider=aws-us-east-1 \
  --set serviceAccount.name=bigfleet-aws \
  --set serviceAccount.roleArn=arn:aws:iam::111122223333:role/bigfleet-aws-us-east-1-role \
  --set ec2.ami=ami-0123456789abcdef0 \
  --set ec2.subnets='us-east-1a=subnet-aaa,us-east-1b=subnet-bbb' \
  --set ec2.securityGroups=sg-0123 \
  --set ec2.iamInstanceProfile=bigfleet-node \
  --set-file offerings.content=offerings.us-east-1.json
```

The chart renders:

- a **Deployment** (`replicas: 1`, `Recreate`) with HTTP probes
  `livenessProbe: /healthz` and `readinessProbe: /readyz` on the metrics port,
  hardened to match the non-root, read-only-rootfs image;
- a **Service** exposing the `grpc` port (BigFleet dials this) and a `metrics`
  port carrying `prometheus.io/scrape` annotations;
- a **ServiceAccount** annotated `eks.amazonaws.com/role-arn` for IRSA;
- a **ConfigMap** for the offerings (and optional base user-data);
- an optional **PVC** for durable `--state`.

### Common extras

```sh
# Durable state on a PersistentVolume (recommended in production):
--set state.enabled=true \
--set state.persistence.enabled=true \
--set state.persistence.size=1Gi

# Observed spot interruptions via an EventBridge -> SQS rule:
--set spotInterruptionQueue=https://sqs.us-east-1.amazonaws.com/111122223333/bigfleet-spot-interruptions

# mTLS for the gRPC listener (Secret with tls.crt, tls.key, ca.crt):
--set tls.enabled=true --set tls.mtls=true --set tls.secretName=bigfleet-aws-tls
```

## Endpoints

- gRPC `CapacityProvider` + `grpc.health.v1` + reflection on `--addr` (`:9000`).
- HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) on
  `--metrics-addr` (`:9090`).

Metrics are namespaced `bigfleet_aws_*` on an isolated registry (EC2/SSM API
calls, gRPC requests, reconcile + spot-refresh runs, observed interruptions),
plus the standard Go/process collectors.
