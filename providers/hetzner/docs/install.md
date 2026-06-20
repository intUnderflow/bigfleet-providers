---
title: Install & deploy
description: Run the Hetzner Cloud provider — the container image, the Helm chart, flags, mTLS, and the HCLOUD_TOKEN Secret.
sidebar:
  order: 1
  label: Install & deploy
---

The Hetzner Cloud provider is **one process per location**. You run it next to
BigFleet, point it at a base image, give it a Hetzner Cloud API token and an SSH
key, and BigFleet dials its `--addr`. This page covers the container image, the
Helm chart, the flags you actually need, mTLS, and the Secret wiring.

If you just want to kick the tyres with no Hetzner account, the
[overview](/providers/hetzner/) shows the credential-free fake backend.
Everything below is for a real project.

## Container image

The binary is a single static Go binary; the image is built from
[`deploy/Dockerfile`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/hetzner/deploy/Dockerfile)
(distroless, non-root, no shell). Build and push it **from the repository root**
(the `providers/hetzner` module's `replace => ../..` needs the whole repo in
context to resolve the `providerkit` root module):

```sh
docker build -t ghcr.io/your-org/bigfleet-hetzner:latest \
  -f providers/hetzner/deploy/Dockerfile .
docker push ghcr.io/your-org/bigfleet-hetzner:latest
```

The entrypoint is the provider binary, so you pass [flags](#flags) as container
args. A bare smoke test (fake backend, no credentials) confirms the image runs:

```sh
docker run --rm -p 9000:9000 -p 9090:9090 \
  ghcr.io/your-org/bigfleet-hetzner:latest \
  --seed-count 32 --addr :9000 --metrics-addr :9090
# then: curl localhost:9090/healthz  -> ok
#       curl localhost:9090/readyz   -> ready
```

The container exposes two ports:

| Port | Flag | Serves |
|---|---|---|
| `9000` | `--addr` | gRPC `CapacityProvider` + `grpc.health.v1` + reflection |
| `9090` | `--metrics-addr` | HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) |

## Helm chart

The chart lives at
[`deploy/helm/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/hetzner/deploy/helm).
It renders a `Deployment` (single replica — one process per location, owns its
`--state`), a `Service` exposing the gRPC + metrics ports (with Prometheus
scrape annotations), a `ServiceAccount`, and — when enabled — a `ConfigMap` for
the offerings and a `PersistentVolumeClaim` for durable state. It consumes the
`HCLOUD_TOKEN` Secret you create in [Credentials](/providers/hetzner/credentials/).

Install it with a values file per location:

```sh
helm install bigfleet-hetzner-nbg1 providers/hetzner/deploy/helm \
  -n bigfleet --create-namespace \
  -f nbg1.values.yaml
```

A minimal `nbg1.values.yaml`:

```yaml
image:
  repository: ghcr.io/your-org/bigfleet-hetzner
  tag: latest

# One process per location. `location` sets the default offering location and
# the label stamped on every HostRef.
location: nbg1
provider: hetzner-nbg1

# The Hetzner Cloud server settings.
hetzner:
  image: ubuntu-24.04         # base image (must ship the bootstrap hook)
  eurToUSD: 1.08              # FX rate applied to Hetzner's EUR prices

# The Secret holding the project-scoped API token (key: token).
token:
  secretName: bigfleet-hetzner-token

# The Secret holding the SSH private key for Configure/Drain delivery.
ssh:
  secretName: bigfleet-hetzner-ssh
  user: root

# Durable state on a PersistentVolume: fence marks, the idempotency map, and
# bindings survive restarts. Without it the provider is in-memory only.
state:
  enabled: true
  persistence:
    enabled: true
    size: 1Gi
```

The offerings JSON is delivered through `offerings.content`: set it and the
chart renders the JSON into a ConfigMap, mounts it at
`/etc/bigfleet/offerings/offerings.json`, and passes `--offerings`. Use
`--set-file` so you keep the file out of your values:

```sh
helm install bigfleet-hetzner-nbg1 providers/hetzner/deploy/helm \
  -n bigfleet --create-namespace \
  -f nbg1.values.yaml \
  --set-file offerings.content=offerings.nbg1.json
```

The offerings shape is documented in
[Configuration](/providers/hetzner/configuration/). Always enable durable
`state` on a PersistentVolume in production — without it the provider is
in-memory and cannot recover bindings on restart.

## Flags

Every flag the binary accepts, grouped by what you touch first. The full
reference (defaults, semantics, the bootstrap model) is in
[Configuration](/providers/hetzner/configuration/).

**Core**

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address |
| `--provider` | `hetzner` | Label stamped on `HostRef.provider` (e.g. `hetzner-nbg1`) |
| `--hetzner-backend` | `auto` | `hetzner` \| `fake` \| `auto` (auto = `hetzner` when a token is set, else `fake`) |
| `--token` | _(empty)_ | Hetzner Cloud API token (or set `HCLOUD_TOKEN`) |
| `--state` | _(empty)_ | Durable state file; empty = in-memory only |

**Launch parameters (hetzner backend)**

| Flag | Default | Meaning |
|---|---|---|
| `--image` | _(empty)_ | Base image name/id for `Server.Create`. **Required** for the hetzner backend |
| `--ssh-key` | _(empty)_ | Path to the SSH private key used for Configure/Drain delivery |
| `--ssh-user` | `root` | SSH user for Configure/Drain delivery |
| `--bootstrap-hook` | `/opt/bigfleet/bootstrap` | Image path that applies the delivered bootstrap blob |
| `--base-user-data` | _(empty)_ | File with the generic pre-binding cloud-init baked in at create |
| `--eur-usd` | `1.08` | EUR→USD conversion rate applied to Hetzner prices |

**Offerings**

| Flag | Default | Meaning |
|---|---|---|
| `--offerings` | _(empty)_ | JSON offerings file (else a built-in mix sized by `--seed-count`) |
| `--seed-count` | `32` | Speculative slots for the default offerings |
| `--location-a` / `--location-b` | `nbg1` / `fsn1` | Locations for the default offerings |

**Background, observability & TLS**

| Flag | Default | Meaning |
|---|---|---|
| `--price-refresh` | `30m` | Price refresh interval (`0` = off) |
| `--reconcile-interval` | `2m` | Background Hetzner→inventory reconcile interval (`0` = off) |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz` (empty = disabled) |
| `--reflection` | `true` | Register gRPC server reflection (for grpcurl/debugging) |
| `--tls-cert` / `--tls-key` | _(empty)_ | Server certificate + key (PEM); enables TLS |
| `--tls-ca` | _(empty)_ | Client CA bundle (PEM); enables mTLS |

## mTLS

With no `--tls-cert`/`--tls-key` the provider serves **insecure** gRPC — fine
only for trusted in-cluster traffic. For production, terminate mTLS in the
provider itself:

- `--tls-cert` + `--tls-key` enable TLS (TLS 1.3 minimum).
- adding `--tls-ca` (a client CA bundle) enables **mTLS**: the provider then
  requires and verifies a client certificate on every connection.

`--tls-ca` without `--tls-cert`/`--tls-key` is rejected, and supplying only one
of cert/key is rejected — so a half-configured TLS setup fails fast at startup
rather than silently serving plaintext. The chart mounts a standard Kubernetes
TLS Secret at `/etc/bigfleet/tls` and wires the flags for you:

```yaml
tls:
  enabled: true
  mtls: true                       # mount ca.crt and require a verified client cert
  secretName: bigfleet-hetzner-tls # Secret keys: tls.crt, tls.key, ca.crt
```

The full trust model is in [Security](/providers/hetzner/security/).

## Bringing it up

```sh
helm install bigfleet-hetzner-nbg1 providers/hetzner/deploy/helm \
  -n bigfleet -f nbg1.values.yaml

kubectl -n bigfleet logs deploy/bigfleet-hetzner-nbg1 | grep 'serving CapacityProvider'
kubectl -n bigfleet port-forward deploy/bigfleet-hetzner-nbg1 9090:9090 &
curl localhost:9090/readyz   # -> ready once gRPC is serving
```

The pod reports `/readyz` green only after the gRPC server is serving, so wire
it to a readiness probe and let BigFleet dial the `Service` once the probe
passes.
