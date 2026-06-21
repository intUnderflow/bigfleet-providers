---
title: Security
description: The trust model of the BigFleet OVHcloud Public Cloud provider — mTLS on the gRPC listener, SSH host-key pinning for bootstrap delivery, least-privilege OpenStack user, and a non-root, read-only image.
sidebar:
  order: 6
  label: Security
---

This page is the provider's trust model: who can call it, how the secret-bearing
bootstrap reaches an instance safely, what credentials it holds, and how the
container is hardened.

## The gRPC listener — mTLS in production

BigFleet (the shard) dials the provider's `--addr`. By default the listener is
**plaintext**, which is acceptable only on a trusted in-cluster network. For any
exposed deployment, enable **mutual TLS** so only authorized shards connect:

- `--tls-cert` + `--tls-key` enable TLS.
- adding `--tls-ca` enables **mTLS** — the provider requires and verifies a client
  certificate against that CA (`tls.RequireAndVerifyClientCert`), and the minimum
  version is TLS 1.3.

The Helm chart exposes this via `tls.enabled` / `tls.mtls` / `tls.secretName`
(a Secret with `tls.crt`, `tls.key`, `ca.crt`). The certification harness dials
**insecure** (it is an in-process trust test), so a plaintext mode always exists
for it — but production should be mTLS.

## Bootstrap delivery — authenticated and confidential

`Configure` carries the opaque `bootstrap_blob`, which holds the cluster **join
secrets**. The provider never parses it, and it must reach **only the right
instance**, over a channel that is both authenticated and confidential. OVH
Public Cloud instances are reachable over SSH, so the provider uses SSH with two
guarantees — the OpenStack/SSH analogue of how `providers/hetzner` delivers its
blob:

- **No impersonation (the provider authenticates the host).** At create, the
  provider generates a fresh ed25519 **SSH host key**, injects it into the
  instance via cloud-init, and pins its fingerprint in the instance's OpenStack
  metadata (`bigfleet-host-key-fp`). Every later Configure/Drain SSH connection
  verifies the presented host key against that pin and **aborts on mismatch** as a
  possible MITM. The join secrets never go to an impostor host.
- **No unauthorized fetch (the host authenticates the provider).** The provider
  connects with its SSH key (`--ssh-key`); the matching public key is injected at
  create via the OpenStack keypair (`--key-name`), so only the provider can open
  the session and deliver the blob.

For an **orphan** instance with no pin (created out of band, or before pinning),
the provider trust-on-first-uses: it records the observed host key on the first
connection and verifies every connection after that. The residual exposure is that
single first connection; a created-by-us instance is always pinned from create, so
it never applies on the happy path.

The blob is delivered over the SSH session and applied by the image's hook; it is
written to disk on the host with `umask 077` and is **never logged** by the
provider.

## Credentials

- **OpenStack user.** A Keystone v3 user scoped to one Public Cloud project, with
  the project `member` role only — the least privilege OVH exposes. The provider
  filters every action to instances carrying its own `bigfleet-managed` metadata,
  so it never touches anything it did not create. See
  [Credentials](/providers/ovhcloud/credentials/) for scoping and rotation.
- **SSH key.** A dedicated private key (not an operator's personal key), held by
  the provider and rotated alongside the OpenStack user.
- **Never logged.** The OS_* password, the SSH key, and the bootstrap blob never
  appear in logs or metrics.

## Network exposure

The provider reaches instances over SSH for bootstrap delivery and drain, so the
instances must be reachable from the provider's pod. The default `--network`
is OVH's **`Ext-Net`** (the public network), which gives every instance a **public
IPv4** — convenient, but it means a misconfigured deploy exposes your nodes to the
internet. For a hardened deployment:

- attach a **private (vRack/internal) network** with `--network=<private-net>` that
  the provider's cluster can route to, so instances have no public IPv4;
- if instances must keep a public IPv4, lock inbound traffic to SSH (port 22) from
  the provider's source range with an OpenStack security group, and rely on the
  host-key pinning + key auth above for the bootstrap channel.

The provider does not open any ports on the instance itself; reachability is
entirely a function of the network and security groups you attach.

## Container hardening

The image is `distroless/static:nonroot` — no shell, no package manager. The Helm
chart runs it:

- as **non-root** (uid 65532, `runAsNonRoot: true`),
- with a **read-only root filesystem**,
- `allowPrivilegeEscalation: false` and **all capabilities dropped**,
- with the `RuntimeDefault` seccomp profile.

Durable `--state` (when enabled) is the only writable mount, backed by a
PersistentVolume.

## Fencing — defence against zombie shards

Every mutating RPC carries a `(shard_id, shard_epoch, sequence_number)` fencing
token. The provider (via `providerkit`) tracks the per-shard high-water mark,
rejects any not-strictly-newer token with `FAILED_PRECONDITION` **without applying
it**, and checks the fence **before** the idempotency short-circuit — so a zombie
shard can never replay a cached operation. `FAILED_PRECONDITION` is reserved
exclusively for fencing, which makes it a clean alerting signal (see
[Observability](/providers/ovhcloud/observability/)). The marks are persisted with
`--state`, so a restart does not re-open the zombie-admission window.
