---
title: Security
description: mTLS on the gRPC port, the API sub-account credentials, and the SSH host-key-pinned bootstrap trust model for the BigFleet UpCloud provider.
sidebar:
  order: 5
  label: Security
---

The UpCloud provider sits on the trust boundary between BigFleet's control plane
and your UpCloud account: it accepts lifecycle RPCs over the network and turns
them into `CreateServer`, `DeleteServerAndStorages`, label mutations, and SSH
commands on running servers. This page covers the things an operator must get
right — the gRPC transport (mTLS), the API credentials the process holds, the SSH
bootstrap trust model, and how the process is exposed.

## Transport: mTLS on the gRPC port

The `CapacityProvider` gRPC service, the `grpc.health.v1` health service, and
(optionally) reflection all share `--addr` (default `:9000`). Secure it with the
TLS flags:

```sh
./bin/upcloud --provider upcloud-fi-hel1 --zone fi-hel1 \
              --template 0100...0200 \
              --tls-cert server.pem --tls-key server-key.pem \
              --tls-ca client-ca.pem
```

The flags compose into three modes (logged at startup as the `security` field):

| Mode | Flags | Behaviour |
|---|---|---|
| `insecure` | none of `--tls-cert`/`--tls-key` | Plaintext. Acceptable only for trusted in-cluster traffic or the fake backend. |
| `TLS` | `--tls-cert` + `--tls-key` | Server presents a cert; clients are not authenticated. |
| `mTLS` | `--tls-cert` + `--tls-key` + `--tls-ca` | Server presents a cert **and** requires a client cert chaining to `--tls-ca`. Use this in production for shard↔provider. |

Notes from the implementation, so you do not fight the validation:

- `--tls-cert` and `--tls-key` are required together — supplying only one is a
  startup error.
- `--tls-ca` without a cert/key is rejected; a CA only makes sense once the server
  itself has a cert.
- When `--tls-ca` is set, client auth is `RequireAndVerifyClientCert`: a missing
  or untrusted client certificate is refused at the TLS layer, before any RPC
  handler runs.
- The gRPC server pins **TLS 1.3** (`MinVersion`). Make sure BigFleet's client and
  any debugging tooling can negotiate 1.3.
- A bad keypair or an unparseable CA bundle fails the process at startup rather
  than degrading silently, so a misconfigured cert can never come up insecure.

The TLS keypair and CA are read once, at startup, so to roll a certificate,
restart the process after the new PEM is in place (a Deployment rollout). The
persisted `--state` file is the restart path, so a rolling restart is safe.

## The API credentials

Authorisation to UpCloud is a single set of **API sub-account credentials**
(username + password over HTTP Basic) — there is no IAM, role chain, or instance
profile, and no separate node identity. Consequences for security:

- The credentials can create and delete servers within the sub-account's scope.
  Use a **dedicated API sub-account** scoped to API access plus server/storage
  management, and nothing else — no billing or account administration — so its
  blast radius is only the servers this provider manages. See
  [Credentials](credentials.md).
- Store them as a Kubernetes Secret mounted as `UPCLOUD_USERNAME` /
  `UPCLOUD_PASSWORD`, never in args, an image, or values.
- They are **never logged**. Use a distinct, named sub-account per deployment so
  it can be rotated and audited independently.

## The SSH bootstrap trust model

UpCloud has no in-guest command API, and a server's cloud-init `user_data` is
immutable after first boot, so the per-cluster bootstrap blob — a **join
secret** — is delivered to the **already-running** server over **SSH**, with the
host key **verified against a pinned fingerprint**. This is the most important
design choice on this page.

The flow, end to end:

- **At Create, the provider mints a fresh ed25519 SSH host key** for the server
  and injects it via cloud-init, so the host boots presenting a key the provider
  already knows. The key's fingerprint (`base32(sha256(pubkey))`) is **pinned in a
  server label** (`bigfleet-host-key-fp`).
- **At every Configure / Drain, the SSH host key is verified against that pin.**
  The provider connects, and its host-key callback requires the presented key's
  fingerprint to **exactly match** the pinned one. A mismatch means a possible
  **MITM** and is a **hard fail** — the connection is aborted and the operation
  surfaces as `FAILED`. The provider **never** uses `ssh.InsecureIgnoreHostKey`,
  and the gRPC client side never uses `tls.InsecureSkipVerify`.
- **The blob travels only over that verified SSH channel.** It is base64-written
  next to `--bootstrap-hook` and the hook runs it to join the cluster. The provider
  waits for the hook to **succeed** before recording the binding label, so a failed
  apply is `FAILED`, never a false Idle. The blob is opaque — the provider never
  parses it.
- **The base user-data installs the on-host hook ONLY — never the secret.**
  `--base-user-data` is baked into `user_data` at create and is therefore readable
  from server metadata for the server's lifetime; it must contain only the
  cluster-agnostic hook, never the cluster-join secret. The secret is delivered
  later, over the verified SSH channel.

### TOFU fallback for orphans, and its residual risk

A server the provider did **not** create — an orphan adopted into inventory, or one
provisioned before host-key pinning — has **no pinned fingerprint**. For those, the
host-key callback **trust-on-first-uses (TOFU)**: it records the key presented on
the first connection, persists that fingerprint to the server's label, and verifies
every later connection against it.

The residual risk is **bounded to that single first connection**: a MITM would have
to be in position at exactly that moment, and every connection afterward is pinned
and verified. A server the provider created itself is never exposed to this window —
its key is pinned at Create, before any SSH connection happens. Prefer
provider-created servers; treat adopted orphans as the only TOFU case.

## gRPC mTLS for shard↔provider

The shard↔provider link is the gRPC port (`--addr`). Secure it with **mTLS**
(`--tls-cert` + `--tls-key` + `--tls-ca`) in production, so the provider only
accepts a verified client certificate from BigFleet — see [the transport
section](#transport-mtls-on-the-grpc-port) above. This is independent of the SSH
channel, which is provider→server.

## Exposure

Run the provider with `replicas: 1` per zone, reachable only by BigFleet:

- Keep the gRPC `--addr` on a `ClusterIP` Service inside the mesh/namespace, not a
  `LoadBalancer`. If you terminate TLS at a mesh sidecar instead of in the
  provider, leave the provider `insecure` but ensure the port is never reachable
  outside the mesh.
- The provider must be able to **reach the servers over SSH** (`--ssh-user`, port
  22). Scope outbound SSH to the zone's server range; consider a utility/private
  network the control plane and servers share.
- The metrics/health port (`--metrics-addr`) serves no secrets, but scope it to
  your Prometheus and kubelet probes all the same.
- The pod runs non-root (uid 65532) on a read-only root filesystem with all
  capabilities dropped (the chart's hardened defaults match the distroless image).
