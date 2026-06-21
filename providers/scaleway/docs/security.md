---
title: Security
description: mTLS, the least-privilege Scaleway API key, and the on-host agent bootstrap channel trust model for the BigFleet Scaleway provider.
sidebar:
  order: 5
  label: Security
---

The Scaleway provider sits on the trust boundary between BigFleet's control plane
and your Scaleway project: it accepts lifecycle RPCs over the network and turns
them into `CreateServer`, `DeleteServer`, label mutations, and bootstrap delivery.
This page covers the four things an operator must get right — the gRPC transport
(mTLS), the API key the process holds, the agent-bootstrap trust model, and how
the process is exposed.

## Transport: mTLS on the gRPC port

The `CapacityProvider` gRPC service, the `grpc.health.v1` health service, and
(optionally) reflection all share `--addr` (default `:9000`). Secure it with the
TLS flags:

```sh
./bin/scaleway --provider scaleway-fr-par --substrate instances --image ubuntu_jammy \
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
  itself has a cert.
- When `--tls-ca` is set, client auth is `RequireAndVerifyClientCert`: a missing or
  untrusted client certificate is refused at the TLS layer, before any RPC handler
  runs.
- The server pins **TLS 1.3** (`MinVersion`). Make sure BigFleet's client and any
  debugging tooling can negotiate 1.3.
- A bad keypair or an unparseable CA bundle fails the process at startup rather
  than degrading silently, so a misconfigured cert can never come up insecure.

The TLS keypair and CA are read once, at startup, so to roll a certificate, restart
the process after the new PEM is in place (a Deployment rollout). The persisted
`--state` file is the restart path, so a rolling restart is safe.

## The API key

Authorisation to Scaleway is an **IAM-application access key + secret key**, scoped
by a least-privilege IAM policy to a single project — not a role or instance
profile. Consequences for security:

- The key holds only the permission sets the provider calls (`InstancesFullAccess`
  + `BlockStorageFullAccess` — the latter so Delete can remove the boot volume;
  plus `BareMetalFullAccess` when Elastic Metal is enabled), scoped to one project.
  Keep the project scoped to BigFleet-managed capacity so the key's blast radius is
  only what this provider owns.
- Store it as a Kubernetes Secret mounted as `SCW_ACCESS_KEY` / `SCW_SECRET_KEY` /
  `SCW_DEFAULT_PROJECT_ID`, never in args, an image, or values. The full minting /
  storage / rotation flow is on the [Credentials](/providers/scaleway/credentials/)
  page.
- The key is **never logged**. Use a distinct, named application/key per deployment
  so it can be rotated and audited independently.

## The agent bootstrap trust model

The per-cluster bootstrap blob carries the cluster-join material, so its delivery
must be authenticated both ways. Scaleway has no privileged in-guest command API
the provider can lean on, so delivery runs through a provider-served, mutually
authenticated **bootstrap channel** that the **on-host agent** (installed by the
generic base `user_data` at first boot) dials. This is the HTTP/agent analogue of
the Hetzner provider's SSH host-key-pinned delivery. The trust model:

- **Generic base `user_data` carries no secret.** The cloud-init baked in at
  `CreateServer` only installs and starts the agent. No cluster-specific material
  is present before a cluster is chosen, so a leaked image reveals nothing useful.
- **Per-machine HMAC token (the analogue of host-key pinning).** At Configure the
  agent **dials** the provider's HTTPS bootstrap channel (`--bootstrap-addr`) and
  long-polls for its own command; the provider authorises every request before any
  command or blob is released. The agent presents a **per-machine bearer token =
  `base64(HMAC-SHA256(--bootstrap-secret, machine_id))`**, which the provider
  re-derives and compares in constant time, so an attacker who has neither the
  HMAC secret nor that specific machine's identity cannot capture another
  machine's cluster-join material. The agent in turn pins the provider's CA
  (`--bootstrap-ca`, the server cert by default) and verifies it over TLS, so an
  on-path (MITM) attacker cannot impersonate the provider and feed a forged blob.
  The blob is released only to the authenticated, correct machine, and because the
  token is per machine, compromising one node's token unlocks no other node's blob.
  The token is re-derivable and never stored.
- **The blob is opaque.** It is delivered to the agent and consumed verbatim; the
  provider never parses it, and it is never logged.

For defence in depth, run the bootstrap channel over a **private/management
network** the control plane trusts, and use a dedicated, high-entropy
`--bootstrap-secret` stored as its own Secret (see
[Credentials](/providers/scaleway/credentials/)) — never an operator's personal
secret. Pin it (rather than the random default) so per-machine tokens survive a
provider restart.

## Exposure

Run the provider with `replicas: 1` per zone, reachable only by BigFleet:

- Keep the gRPC `--addr` on a `ClusterIP` Service inside the mesh/namespace, not a
  `LoadBalancer`. If you terminate TLS at a mesh sidecar instead of in the
  provider, leave the provider `insecure` but ensure the port is never reachable
  outside the mesh.
- The metrics/health port (`--metrics-addr`) serves no secrets, but scope it to
  your Prometheus and kubelet probes all the same.
- The pod runs non-root (uid 65532) on a read-only root filesystem with all
  capabilities dropped (the chart's hardened defaults match the distroless image).
