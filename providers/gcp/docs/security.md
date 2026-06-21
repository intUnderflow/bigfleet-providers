---
title: Security
description: mTLS on the gRPC port, the two-identity service-account model (Workload Identity), the in-band SSH bootstrap trust model, and exposure for the BigFleet GCP provider.
sidebar:
  order: 5
  label: Security
---

The GCP provider sits on the trust boundary between BigFleet's control plane and
your GCP project: it accepts lifecycle RPCs over the network and turns them into
`instances.insert`, `instances.delete`, metadata/label mutations, and in-band SSH
to the host. This page covers the four things an operator must get right — the
gRPC transport (mTLS), the identities the process and the nodes hold, the SSH
bootstrap trust model, and how the process is exposed.

## Transport: mTLS on the gRPC port

The `CapacityProvider` gRPC service, the `grpc.health.v1` health service, and
(optionally) reflection all share `--addr` (default `:9000`). Secure it with the
TLS flags:

```sh
./bin/gcp --provider gcp-us-central1 --project my-proj --region us-central1 \
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

## The two identities

Authorisation to GCE is **Application Default Credentials**, and there are two
identities you must keep apart (full detail on
[Credentials](/providers/gcp/credentials/)):

- **Provider service account** — what the process authenticates as. It holds
  `roles/compute.instanceAdmin.v1` on the project (and `serviceAccountUser` on the
  node SA). On GKE it is reached via **Workload Identity** — no key file. The
  least-privilege story is: that one role, scoped to the one project, mapped to
  the lifecycle calls it makes.
- **Instance service account** (`--instance-service-account`) — what the
  *launched nodes* run as. It must **not** be the provider's identity; a node
  should not be able to create or delete other nodes. Give it only what the
  workloads need.

Prefer Workload Identity over a key file. A key file is a long-lived credential
that can create and delete every instance in the project — store it as a Secret,
never in args/image/values, and rotate it. Credentials are **never logged**.

## The SSH bootstrap trust model

The cluster-join bootstrap is delivered **in-band over SSH** to the
already-running host — never persisted in instance metadata — mirroring the
certified AWS (SSM) and Hetzner (SSH) providers. Security implications:

- **The join secret is transient.** Configure SSHes to the host, writes the
  opaque `bootstrap_blob` to `<bootstrap-hook>.blob` with `umask 077`, and runs
  the hook. The blob is **never** written to instance metadata, so a healthy bound
  node does **not** carry the kubelet-join secret in durable, metadata-server-
  readable plaintext. (An earlier design wrote it to `startup-script` metadata;
  that persisted the secret for the node's lifetime and was replaced.)
- **The host is authenticated.** At Insert the provider injects a pinned SSH host
  key (cloud-init) and records its fingerprint in `bigfleet-host-key-fp` metadata;
  every Configure/Drain connection verifies the presented host key against that
  pin and aborts on mismatch (possible MITM). An instance with no pin (an orphan,
  or an image without cloud-init) is trust-on-first-used and the observed key
  pinned — the residual risk is confined to that first connection, logged at WARN.
- **Host-key trust boundary (know this).** The injected host **private** key is
  delivered via the instance's cloud-init `user-data` metadata, which is durable
  and readable by any principal with `compute.instances.get` on the project — and
  by workloads on the node via the metadata server. A reader of that metadata
  could impersonate the host's SSH key, weakening the MITM protection above. Treat
  it as a project-scoped trust boundary: restrict instance-metadata read (and
  block the metadata server from untrusted pods), and prefer **Shielded VM** images
  for the strongest host integrity. If you don't want any host private key at
  rest, omit the cloud-init injection and rely on trust-on-first-use pinning
  instead (one-connection window). This mirrors the certified Hetzner provider's
  model; the trade-off is called out here rather than hidden.
- **The client is authenticated.** The provider connects as `--ssh-user` with
  `--ssh-key`; only that key is authorised (via `ssh-keys` metadata, with
  `enable-oslogin=false`). Use a dedicated key for the provider, stored as its own
  Secret, not an operator's personal key.
- **The binding record** (`bigfleet-cluster` metadata) is written **only after**
  the on-host hook succeeds — a recovery record, not a join receipt, and not a
  secret. Drain cordons/drains the kubelet over SSH and then clears it. No reboot
  is involved at any point.
- For defence in depth, keep the SSH path on a private/management network
  (`--use-external-ip` is off by default; the provider reaches the host over its
  internal IP from the same VPC).

## Exposure

Run the provider with `replicas: 1` per region, reachable only by BigFleet:

- Keep the gRPC `--addr` on a `ClusterIP` Service inside the mesh/namespace, not
  a `LoadBalancer`. If you terminate TLS at a mesh sidecar instead of in the
  provider, leave the provider `insecure` but ensure the port is never reachable
  outside the mesh.
- The metrics/health port (`--metrics-addr`) serves no secrets, but scope it to
  your Prometheus and kubelet probes all the same.
- The pod runs non-root (uid 65532) on a read-only root filesystem with all
  capabilities dropped (the chart's hardened defaults match the distroless image).
