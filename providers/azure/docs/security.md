---
title: Security
description: mTLS, the least-privilege managed identity, and the CustomScript bootstrap trust model for the BigFleet Azure provider.
sidebar:
  order: 5
  label: Security
---

The Azure provider sits on the trust boundary between BigFleet's control plane and
your Azure subscription: it accepts lifecycle RPCs over the network and turns them
into VM creates, CustomScript extensions, and tag mutations. This page covers the
four things an operator must get right — the gRPC transport (mTLS), the identity
the process holds, the bootstrap trust model, and how the process is exposed.

## Transport: mTLS on the gRPC port

The `CapacityProvider` gRPC service, the `grpc.health.v1` health service, and
(optionally) reflection all share `--addr` (default `:9000`). Secure it with the
TLS flags:

```sh
./bin/azure --location eastus --provider azure-eastus \
            --subscription-id ... --resource-group ... --subnet-id ... \
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
  startup error.
- `--tls-ca` without a cert/key is rejected; a CA only makes sense once the server
  has a cert.
- When `--tls-ca` is set, client auth is `RequireAndVerifyClientCert`: a missing
  or untrusted client certificate is refused at the TLS layer, before any RPC
  handler runs.
- The server pins **TLS 1.3** (`MinVersion`). Make sure BigFleet's client and any
  debugging tooling can negotiate 1.3.
- A bad keypair or unparseable CA bundle fails the process at startup rather than
  degrading silently, so a misconfigured cert can never come up insecure.

### Issuing and rotating certificates

The provider has no opinion on your PKI — give it PEM files and a client CA
bundle. On Kubernetes, [cert-manager](https://cert-manager.io/) is the path of
least resistance: issue a server `Certificate` for the provider's Service DNS
name, mount it via the chart's `tls.secretName`, and the chart wires the flags.

**Rotation.** The TLS keypair and CA are read once, at startup, so the process
does **not** hot-reload a rotated cert from disk. To roll a certificate, restart
the process after the new PEM is in place (a Deployment rollout when cert-manager
rewrites the Secret). Because the transitions run with minute-scale kit timeouts
and the persisted `--state` file is the restart path, a rolling restart is safe.
Rotate the client CA with the usual two-phase swap (issue the bundle with both
old and new roots, roll every provider, re-issue client certs).

If you terminate TLS at a mesh sidecar (Istio/Linkerd) instead, leave the provider
`insecure` and let the mesh enforce mTLS — but then the provider port must never
be reachable outside the mesh.

## Least-privilege identity

The provider holds **one** Azure identity: a user-assigned managed identity with a
role scoped to the target resource group. There is no service-principal secret in
config — it uses `azidentity.DefaultAzureCredential` (Workload Identity on AKS).
The full role and Terraform live in [Credentials](/providers/azure/credentials/);
this section is the security rationale.

- **Scope the role to the resource group**, not the subscription. The provider
  only ever touches VMs/NICs/disks/extensions in its one resource group, so the
  role assignment should be scoped there. A subscription-wide assignment is the
  most common over-grant.
- **Prefer the custom role over Contributor.** The custom role grants only the
  compute/network actions the code calls (`virtualMachines/*`,
  `networkInterfaces/*`, `disks/*`, `extensions/*`, `skus/read`, and the subnet
  `join/action`). `Contributor` works but is far broader.
- **The identity never reads secrets.** It does not touch Key Vault, storage
  accounts, or anything outside compute/network in its resource group. Keep it
  that way.

The launched VMs run as themselves (no managed identity is attached unless your
image needs one); the provider does not pass its own identity to the VMs.

## The CustomScript bootstrap trust model

Configure and Drain are delivered as **CustomScript VM extensions** — there is no
inbound SSH path required for the lifecycle (`--ssh-public-key` is optional, for
break-glass). The extension poller runs to completion; a failed extension becomes
a `FAILED` transition rather than a false `Configured`/`Idle`. The trust
properties that matter:

- **The bootstrap blob is opaque to the provider, and delivered encrypted.** On
  Configure the provider base64-encodes the `bootstrap_blob` and ships it in the
  extension's **`protectedSettings.commandToExecute`** — Azure encrypts
  `protectedSettings` at rest and never returns them on
  `virtualMachines/extensions/read`, so the join secret is not exposed to RG
  Readers or the activity log (it is **not** placed in the cleartext `settings`).
  The node writes it to a tmpfs file, invokes the image hook (`--bootstrap-hook`),
  and the script removes the file afterwards (preserving the hook's exit code).
  The provider never parses, logs, or interprets the blob's bytes — the hook is
  the only thing that consumes it. Treat the blob as a credential: it is what
  joins a node to a cluster.
- **The image hook is part of your TCB.** Whatever `--bootstrap-hook` points at
  runs as root on the node with the blob as input. Bake it into the image you
  control, pin the image (`--image`), and review the hook the way you would review
  an init system — a compromised hook is a compromised node. The pre-binding
  `--base-user-data` baked into customData at create is in the same trust
  position.
- **Drain is real.** Drain runs the hook's cordon/drain path and clears the
  `bigfleet-cluster` tag; the hook must exit non-zero on an incomplete drain so it
  surfaces as `FAILED`. Do not weaken that into a best-effort call — a node
  reported drained but still running pods is a correctness *and* safety problem.

## Network exposure

There is **one process per Azure region**, reached only by BigFleet's control
plane, in-cluster:

- **Bind scope.** `--addr` (gRPC) and `--metrics-addr` should both stay on the
  cluster network. Expose the gRPC port to BigFleet via a ClusterIP Service and a
  `NetworkPolicy` that admits only the control plane; never put it behind a public
  LoadBalancer or Ingress.
- **Reflection.** `--reflection` is on by default for `grpcurl` debugging. Under
  mTLS it is low-risk; if the port is reachable more broadly, set
  `--reflection=false`.
- **The metrics port carries no secrets** but exposes operational detail (RPC
  volumes, Azure API outcomes, panic and eviction counts). Scope it to your
  Prometheus scraper. `/healthz` and `/readyz` are unauthenticated probes and are
  fine to leave open to the kubelet.

## Do not commit credentials

The provider takes **no** Azure secret on the command line or in flags — it uses
`DefaultAzureCredential`. Use Workload Identity on AKS so there is no long-lived
secret to leak. Concretely:

- Never bake Azure secrets into the image, the `--base-user-data`, or a committed
  manifest. Prefer Workload Identity; fall back to env-var service principal only
  where Workload Identity is unavailable.
- Keep TLS private keys (`--tls-key`) and the bootstrap blob out of version
  control. Mount keys from a Secret (or cert-manager) and let BigFleet supply the
  blob at Configure time — it is never stored in this provider's config.
- The durable `--state` file holds inventory and bindings, not credentials, but
  treat it as sensitive operational data and keep it off shared/world-readable
  volumes.

See also [Install & deploy](/providers/azure/install/) for the AKS pod spec and
[Credentials](/providers/azure/credentials/) for the exact role.
