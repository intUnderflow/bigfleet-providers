---
title: Security
description: mTLS on the gRPC port, the scoped Personal Access Token, and the on-host agent TLS bootstrap trust model for the BigFleet DigitalOcean provider.
sidebar:
  order: 5
  label: Security
---

The DigitalOcean provider sits on the trust boundary between BigFleet's control
plane and your DigitalOcean account: it accepts lifecycle RPCs over the network
and turns them into `Droplets.Create`, `Droplets.Delete`, tag mutations, and
agent commands. This page covers the four things an operator must get right — the
gRPC transport (mTLS), the API token the process holds, the on-host agent TLS
bootstrap trust model, and how the process is exposed.

## Transport: mTLS on the gRPC port

The `CapacityProvider` gRPC service, the `grpc.health.v1` health service, and
(optionally) reflection all share `--addr` (default `:9000`). Secure it with the
TLS flags:

```sh
./bin/digitalocean --provider digitalocean-nyc3 --region nyc3 \
                   --token "$DIGITALOCEAN_TOKEN" --image ubuntu-24-04-x64 \
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
- The gRPC server pins **TLS 1.3** (`MinVersion`). Make sure BigFleet's client
  and any debugging tooling can negotiate 1.3.
- A bad keypair or an unparseable CA bundle fails the process at startup rather
  than degrading silently, so a misconfigured cert can never come up insecure.

The TLS keypair and CA are read once, at startup, so to roll a certificate,
restart the process after the new PEM is in place (a Deployment rollout). The
persisted `--state` file is the restart path, so a rolling restart is safe.

## The API token

Authorisation to DigitalOcean is a single **Personal Access Token (PAT)** — there
is no IAM, role chain, or instance profile, and no separate node identity.
Consequences for security:

- The token can create and delete Droplets within its scope. Scope it to **read +
  write on Droplets** (plus the Sizes/Tags catalogue) and nothing else — no
  account or billing scope — so its blast radius is only the Droplets this
  provider manages.
- Store it as a Kubernetes Secret mounted as `DIGITALOCEAN_TOKEN`, never in args,
  an image, or values. The full minting / storage / rotation flow is on the
  [Credentials](credentials.md) page.
- The token is **never logged**. Use a distinct, named token per deployment so it
  can be rotated and audited independently.

## The on-host agent TLS bootstrap trust model

DigitalOcean has no in-guest command API, and a Droplet's `user_data` is
immutable after first boot, so the per-cluster bootstrap blob — a **join
secret** — is delivered to the already-running Droplet over an **on-host agent**
channel using TLS with **mutual authentication**: the agent pins the provider's
CA to verify the server, and the provider authenticates each agent with a
per-machine bearer token. (This is mutual authentication, **not mTLS** — the
agent presents a token, not a client certificate.) This is the TLS analogue of
the Hetzner provider's SSH host-key-pinned delivery. The model:

- **The provider serves an HTTPS bootstrap channel** on `--bootstrap-addr`, with
  its own server certificate (`--bootstrap-tls-cert`/`--bootstrap-tls-key`). The
  channel always uses TLS — the provider refuses to start the real backend
  without it, because the blob is a secret and must not travel in plaintext.
- **The agent verifies the provider.** The generic Create-time `user_data` hands
  the agent the provider's pinned CA (`--bootstrap-ca`, default the server cert).
  The agent checks the channel's certificate against that pin, so an on-path
  (MITM) attacker cannot impersonate the provider and feed the Droplet a malicious
  cluster-join blob.
- **The provider authorises only that Droplet.** Each Droplet's agent presents a
  per-machine bearer token = `HMAC(--bootstrap-secret, machine_id)`, injected into
  its Create-time `user_data`. The provider re-derives and constant-time-compares
  it on every fetch, so one Droplet can never read another's blob. The token is
  re-derivable (never stored) and so restart-safe. **`--bootstrap-secret` is
  required** (the provider refuses to start without it) and must be a stable,
  pinned value — a random per-process secret would invalidate issued tokens on
  restart.
- **The blob is opaque.** The provider never parses it; the agent consumes it
  verbatim and acks. A failed apply surfaces as `FAILED`. Drain is delivered over
  the same channel.

For defence in depth, run the bootstrap channel on a **private/management
network** the control plane and the Droplets share, and keep the
`--bootstrap-secret` in a Secret (see [Credentials](credentials.md)).

## Exposure

Run the provider with `replicas: 1` per region, reachable only by BigFleet and
its own Droplets:

- Keep the gRPC `--addr` on a `ClusterIP` Service inside the mesh/namespace, not
  a `LoadBalancer`. If you terminate TLS at a mesh sidecar instead of in the
  provider, leave the provider `insecure` but ensure the port is never reachable
  outside the mesh.
- The bootstrap channel (`--bootstrap-addr`) must be reachable **by the
  Droplets** at `--bootstrap-endpoint`, but by nothing else — scope it tightly. It
  is already mutually authenticated (pinned CA + per-machine token), so an
  unauthorised caller gets `401`.
- The metrics/health port (`--metrics-addr`) serves no secrets, but scope it to
  your Prometheus and kubelet probes all the same.
- The pod runs non-root (uid 65532) on a read-only root filesystem with all
  capabilities dropped (the chart's hardened defaults match the distroless image).
