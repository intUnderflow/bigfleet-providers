---
title: Security
description: mTLS, the project-scoped API token, and the SSH bootstrap trust model for the BigFleet Hetzner Cloud provider.
sidebar:
  order: 5
  label: Security
---

The Hetzner provider sits on the trust boundary between BigFleet's control plane
and your Hetzner Cloud project: it accepts lifecycle RPCs over the network and
turns them into `Server.Create`, `Server.Delete`, label mutations, and SSH
commands. This page covers the four things an operator must get right — the gRPC
transport (mTLS), the API token the process holds, the SSH-bootstrap trust model,
and how the process is exposed.

## Transport: mTLS on the gRPC port

The `CapacityProvider` gRPC service, the `grpc.health.v1` health service, and
(optionally) reflection all share `--addr` (default `:9000`). Secure it with the
TLS flags:

```sh
./bin/hetzner --provider hetzner-nbg1 --token "$HCLOUD_TOKEN" --image ubuntu-24.04 \
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
- `--tls-ca` without a cert/key is rejected; a CA only makes sense once the
  server itself has a cert.
- When `--tls-ca` is set, client auth is `RequireAndVerifyClientCert`: a missing
  or untrusted client certificate is refused at the TLS layer, before any RPC
  handler runs.
- The server pins **TLS 1.3** (`MinVersion`). Make sure BigFleet's client and any
  debugging tooling can negotiate 1.3.
- A bad keypair or an unparseable CA bundle fails the process at startup rather
  than degrading silently, so a misconfigured cert can never come up insecure.

The TLS keypair and CA are read once, at startup, so to roll a certificate,
restart the process after the new PEM is in place (a Deployment rollout). The
persisted `--state` file is the restart path, so a rolling restart is safe.

## The API token

Authorisation to Hetzner is a single **project-scoped, Read & Write API token** —
there is no IAM, role, or instance profile. Consequences for security:

- The token can create and delete **every server in the project**. Keep the
  project scoped to BigFleet-managed capacity so the token's blast radius is only
  what this provider owns.
- Store it as a Kubernetes Secret mounted as `HCLOUD_TOKEN`, never in args, an
  image, or values. The full minting / storage / rotation flow is on the
  [Credentials](/providers/hetzner/credentials/) page.
- The token is **never logged**. Use a distinct, named token per deployment so it
  can be rotated and audited independently.

## The SSH bootstrap trust model

Hetzner Cloud has no in-guest command API, so Configure and Drain reach the
server **over SSH**. The trust model:

- The provider connects as `--ssh-user` (default `root`) with the private key from
  `--ssh-key`. Use a **dedicated** key for the provider, stored as its own Secret,
  not an operator's personal key.
- The base image must authorise the matching public key. The
  cluster-join bootstrap blob is delivered to `<bootstrap-hook>.blob` and the hook
  is run as `<bootstrap-hook> <cluster-id>` — the blob is opaque and the provider
  never parses it.
### Host-key verification

The provider **verifies the server's SSH host key** before delivering the
bootstrap payload, so an on-path (MITM) attacker cannot impersonate a freshly
provisioned server and capture the cluster-join material in the Configure blob.
The model:

- **Injected, pinned key (default, for servers the provider creates).** At
  `Server.Create` the provider mints a fresh ed25519 **host** keypair, injects the
  private key into the server via cloud-init (`ssh_keys:` — merged with your
  `--base-user-data` as a MIME multipart archive), and pins the public key's
  SHA-256 fingerprint in the `bigfleet-host-key-fp` server label. Every later
  Configure/Drain connection checks the presented host key against that pin and
  **aborts on mismatch**. Because the key is known before the first connection,
  there is no trust-on-first-use window for servers the provider provisioned.
- **Trust-on-first-use (fallback, only for servers with no pin).** A server the
  provider did not create (an orphan it adopted) or one provisioned before
  host-key pinning has no pinned fingerprint. The first connection records the
  observed host key into the label and pins it; all later connections are verified
  against it. The residual risk is confined to that single first connection, and
  it is logged at `WARN`.

For defence in depth, run the SSH path over a **private/management network** the
control plane trusts. If your base image bakes its own host key, the injected key
takes precedence (the provider sets it via cloud-init); avoid setting a
conflicting `ssh_keys` block in your `--base-user-data`.

## Exposure

Run the provider with `replicas: 1` per location, reachable only by BigFleet:

- Keep the gRPC `--addr` on a `ClusterIP` Service inside the mesh/namespace, not
  a `LoadBalancer`. If you terminate TLS at a mesh sidecar instead of in the
  provider, leave the provider `insecure` but ensure the port is never reachable
  outside the mesh.
- The metrics/health port (`--metrics-addr`) serves no secrets, but scope it to
  your Prometheus and kubelet probes all the same.
- The pod runs non-root (uid 65532) on a read-only root filesystem with all
  capabilities dropped (the chart's hardened defaults match the distroless image).
