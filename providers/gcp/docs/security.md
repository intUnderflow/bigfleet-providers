---
title: Security
description: mTLS on the gRPC port, the two-identity service-account model (Workload Identity), the startup-script bootstrap trust model, and exposure for the BigFleet GCP provider.
sidebar:
  order: 5
  label: Security
---

The GCP provider sits on the trust boundary between BigFleet's control plane and
your GCP project: it accepts lifecycle RPCs over the network and turns them into
`instances.insert`, `instances.delete`, metadata/label mutations, and resets.
This page covers the four things an operator must get right — the gRPC transport
(mTLS), the identities the process and the nodes hold, the startup-script
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

## The startup-script bootstrap trust model

GCE delivers the cluster-join bootstrap by **instance metadata**: Configure
writes the opaque `bootstrap_blob` to the instance's `startup-script` metadata
and resets the instance so it runs on the next boot. Security implications:

- The blob is the kubelet join material. It is delivered over the GCE control
  plane (an authenticated `instances.setMetadata` call), not over a node-to-node
  channel, so there is no on-path delivery window to a freshly created host.
- The binding label (`bigfleet-cluster`) is written **only after** the blob
  applied, so a failed Configure never leaves an instance mislabelled as bound to
  a cluster it never joined.
- On Drain the `startup-script` is **removed**, so a future boot does not rejoin
  the cluster.
- The blob is opaque — the provider never parses, logs, or rewrites it (the same
  contract as `shard_metadata`). Treat the instance metadata as sensitive; scope
  who can read instance metadata in the project, and prefer the indirect model (a
  baked image that fetches a metadata key) if you want to keep the join material
  out of the plain `startup-script` value.

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
