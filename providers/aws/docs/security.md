---
title: Security
description: mTLS, least-privilege IAM, and the SSM bootstrap trust model for the BigFleet AWS EC2 provider.
sidebar:
  order: 5
  label: Security
---

The AWS provider sits on the trust boundary between BigFleet's control plane and
your AWS account: it accepts lifecycle RPCs over the network and turns them into
`RunInstances`, SSM commands, and tag mutations. This page covers the four
things an operator must get right — the gRPC transport (mTLS), the IAM the
process holds, the SSM-bootstrap trust model, and how the process is exposed.

## Transport: mTLS on the gRPC port

The `CapacityProvider` gRPC service, the `grpc.health.v1` health service, and
(optionally) reflection all share `--addr` (default `:9000`). Secure it with the
TLS flags:

```sh
./bin/aws --region us-east-1 --provider aws-us-east-1 \
          --ami ami-0123456789abcdef0 \
          --tls-cert server.pem --tls-key server-key.pem \
          --tls-ca client-ca.pem
```

The flags compose into three modes (logged at startup as the `security` field):

| Mode | Flags | Behaviour |
|---|---|---|
| `insecure` | none of `--tls-cert`/`--tls-key` | Plaintext. Acceptable only for trusted in-cluster traffic or the fake backend. |
| `TLS` | `--tls-cert` + `--tls-key` | Server presents a cert; clients are not authenticated. |
| `mTLS` | `--tls-cert` + `--tls-key` + `--tls-ca` | Server presents a cert **and** requires a client cert chaining to `--tls-ca`. Use this in production. |

Notes from the implementation, so you do not fight the validation:

- `--tls-cert` and `--tls-key` are required together — supplying only one is a
  startup error (`both --tls-cert and --tls-key are required to enable TLS`).
- `--tls-ca` without a cert/key is rejected (`--tls-ca set without
  --tls-cert/--tls-key`); a CA only makes sense once the server itself has a
  cert.
- When `--tls-ca` is set, client auth is `RequireAndVerifyClientCert`: a missing
  or untrusted client certificate is refused at the TLS layer, before any RPC
  handler runs.
- The server pins **TLS 1.3** (`MinVersion`). Make sure BigFleet's client and
  any debugging tooling (`grpcurl`) can negotiate 1.3.
- A bad keypair or an unparseable CA bundle fails the process at startup rather
  than degrading silently, so a misconfigured cert can never come up insecure.

### Issuing and rotating certificates

The provider has no opinion on your PKI — give it PEM files and a client CA
bundle. On Kubernetes, [cert-manager](https://cert-manager.io/) is the path of
least resistance: issue a server `Certificate` for the provider's Service DNS
name, mount it, and point the flags at the mounted paths.

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: bigfleet-aws-us-east-1
spec:
  secretName: bigfleet-aws-tls
  dnsNames:
    - aws-us-east-1.bigfleet.svc.cluster.local
  issuerRef:
    name: bigfleet-mesh-ca
    kind: ClusterIssuer
```

```sh
# In the pod spec, mount the secret and wire the flags:
#   --tls-cert /tls/tls.crt --tls-key /tls/tls.key --tls-ca /tls/ca.crt
```

**Rotation.** The TLS keypair and CA are read once, at startup
(`serverCredentials` runs during `run()`), so the process does **not** hot-
reload a rotated cert from disk. To roll a certificate, restart the process
after the new PEM is in place (a Deployment rollout when cert-manager rewrites
the Secret). Because Create/Configure/Drain run with minute-scale kit timeouts
and the persisted `--state` file is the restart path, a rolling restart is safe;
drain in-flight transitions first if you want zero `FAILED` churn. Rotate the
client CA by issuing the bundle with both the old and new roots, rolling every
provider, then re-issuing client certs — the usual two-phase CA swap.

If you terminate TLS at a mesh sidecar (Istio/Linkerd) instead, leave the
provider `insecure` and let the mesh enforce mTLS between pods — but then the
provider port must never be reachable outside the mesh (see
[network exposure](#network-exposure) below).

## Least-privilege IAM

The provider holds exactly two kinds of AWS identity, and they should be kept
separate and minimal. The full copy-pasteable policy and IRSA wiring live in
[IAM](/providers/aws/iam/); this section is the security rationale.

### The provider's own role

Give the **process** (via IRSA on EKS — `eks.amazonaws.com/role-arn` on its
ServiceAccount) only the actions the code actually calls:

- `ec2:RunInstances`, `ec2:TerminateInstances`, `ec2:DescribeInstances`
- `ec2:CreateTags`, `ec2:DeleteTags` (the binding/inventory tags)
- `ec2:DescribeSpotPriceHistory` (pricing)
- `ssm:SendCommand`, `ssm:GetCommandInvocation` (Configure/Drain delivery)

Two permissions are conditional — grant them **only** when the matching flag is
set, so the default posture is smaller:

- `iam:PassRole` — required **only** when `--iam-instance-profile` is set (the
  provider attaches that profile to launched instances). Scope it to the single
  node role and constrain it with `iam:PassedToService = ec2.amazonaws.com` so
  the role can be passed to EC2 and nothing else. This is the highest-leverage
  permission to lock down: an over-broad `PassRole` lets the process attach an
  arbitrary role to an instance it controls.
- `sqs:ReceiveMessage` + `sqs:DeleteMessage` — required **only** when
  `--spot-interruption-queue` is set. Scope them to that one queue ARN.

Do not grant anything else. The provider never calls IAM beyond `PassRole`,
never reads Secrets, and never writes to S3.

### The node instance profile

The `--iam-instance-profile` attached to launched instances is a **separate**
identity. It needs SSM so the agent can receive Configure/Drain commands —
attach the AWS-managed `AmazonSSMManagedInstanceCore`. Keep this profile scoped
to what your nodes need to run; the provider's role and the node role should not
be the same principal.

## The SSM bootstrap trust model

Configure and Drain are delivered over **SSM `SendCommand`** (document
`AWS-RunShellScript`), not SSH — there is no inbound path to the node and
`--key-name` is optional. The provider polls `GetCommandInvocation` until the
command reaches `Success`; a `Failed`/`TimedOut`/`Cancelled` invocation becomes
a `FAILED` transition rather than a false `Configured`/`Idle`. The trust
properties that matter:

- **The bootstrap blob is opaque to the provider.** On Configure the provider
  base64-encodes the `bootstrap_blob` it was handed, ships it to the node,
  writes it to `<--bootstrap-hook>.blob` under `umask 077`, and invokes the AMI
  hook (`--bootstrap-hook`, default `/opt/bigfleet/bootstrap`) with the cluster
  id. The provider never parses, logs, or interprets the blob's bytes — the AMI
  hook is the only thing that consumes it. Treat the blob as a credential: it is
  what joins a node to a cluster, so the AMI hook should write it with tight
  permissions and the SSM command output (which the provider only reads on
  failure, via `StandardErrorContent`) should not echo it.
- **Untrusted inputs are shell-quoted.** The blob and the cluster id are
  single-quoted (`shellQuote`) before interpolation into the `/bin/sh` command,
  and the script runs `set -euo pipefail`. The code's own comment is the rule to
  keep: *the blob and cluster id are opaque, so never trust their bytes.* If you
  extend the Configure/Drain scripts, every externally-supplied value must go
  through `shellQuote`.
- **The AMI hook is part of your TCB.** Whatever `--bootstrap-hook` points at
  runs as root on the node with the blob as input. Bake it into the AMI you
  control, pin the AMI (`--ami`), and review the hook the way you would review
  an init system — a compromised hook is a compromised node. The pre-binding
  `--base-user-data` baked in at launch is in the same trust position.
- **Drain is real.** Drain removes the `bigfleet:cluster` tag and runs
  `kubectl cordon`/`drain` over SSM; the drain command must exit non-zero on an
  incomplete drain so it surfaces as `FAILED`. Do not weaken that into a
  best-effort call — a node reported drained but still running pods is a
  correctness *and* safety problem.

## Network exposure

There is **one process per AWS region**, and it is meant to be reached only by
BigFleet's control plane, in-cluster:

- **Bind scope.** `--addr` (gRPC) and `--metrics-addr` (`/metrics`, `/healthz`,
  `/readyz`) should both stay on the cluster network. Expose the gRPC port to
  BigFleet via a ClusterIP Service and a `NetworkPolicy` that admits only the
  control plane; never put it behind a public LoadBalancer or Ingress.
- **Reflection.** `--reflection` is on by default for `grpcurl`-based
  debugging. It advertises the service schema to any client that can already
  reach the port, so under mTLS it is low-risk; if the port is reachable more
  broadly, set `--reflection=false`.
- **The metrics port carries no secrets** but does expose operational detail
  (RPC volumes, EC2 API outcomes, panic and interruption counts — see
  [Observability](/providers/aws/observability/)). Scope it to your Prometheus
  scraper. `/healthz` and `/readyz` are unauthenticated liveness/readiness
  probes and are fine to leave open to the kubelet.
- **No inbound to nodes.** Because Configure/Drain ride SSM, launched instances
  need no inbound SSH/management port from the provider. Keep `--key-name`
  unset unless you have a separate break-glass reason for SSH.

## Do not commit credentials

The provider takes **no** AWS access keys on the command line or in flags — it
uses the default AWS credential chain. Use IRSA on EKS (a role assumed via the
ServiceAccount's `eks.amazonaws.com/role-arn`) so there is no long-lived secret
to leak. Concretely:

- Never bake AWS keys into the image, the `--base-user-data`, the AMI, or a
  committed manifest. Prefer IRSA; fall back to an instance profile on the pod's
  node only if IRSA is unavailable.
- Keep TLS private keys (`--tls-key`) and the bootstrap blob out of version
  control. Mount keys from a Secret (or cert-manager) and let BigFleet supply
  the blob at Configure time — it is never stored in this provider's config.
- The durable `--state` file holds inventory and bindings, not credentials, but
  treat it as sensitive operational data and keep it off shared/world-readable
  volumes.

See also [Install & deploy](/providers/aws/install/) for the EKS/IRSA pod spec
and [IAM](/providers/aws/iam/) for the exact policy document.
