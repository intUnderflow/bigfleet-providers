---
title: Install & deploy
description: Run the UpCloud provider — the container image, the Helm chart (one release per zone), flags, mTLS, and the API sub-account Secret.
sidebar:
  order: 1
  label: Install & deploy
---

The UpCloud provider is **one process per zone**. You run it next to BigFleet,
point it at an OS template, give it an UpCloud API sub-account and an SSH key
pair, and BigFleet dials its `--addr`. This page covers the container image, the
Helm chart, the flags you actually need, mTLS, and the Secret wiring.

If you just want to kick the tyres with no UpCloud account, the
[overview](index.md) shows the credential-free fake backend. Everything below is
for a real account.

## Container image

The binary is a single static Go binary; the image is built from
[`deploy/Dockerfile`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/upcloud/deploy/Dockerfile)
(distroless, non-root, no shell). Build and push it **from the repository root**
(the `providers/upcloud` module's `replace => ../..` needs the whole repo in
context to resolve the `providerkit` root module):

```sh
docker build -t ghcr.io/intunderflow/bigfleet-upcloud:0.1.0 \
  -f providers/upcloud/deploy/Dockerfile .
docker push ghcr.io/intunderflow/bigfleet-upcloud:0.1.0
```

The entrypoint is the provider binary, so you pass [flags](#flags) as container
args. A bare smoke test (fake backend, no credentials) confirms the image runs:

```sh
docker run --rm -p 9000:9000 -p 9090:9090 \
  ghcr.io/intunderflow/bigfleet-upcloud:0.1.0 \
  --seed-count 32 --addr :9000 --metrics-addr :9090
# then: curl localhost:9090/healthz  -> ok
#       curl localhost:9090/readyz   -> ready
```

The container exposes two ports, for BigFleet and Prometheus:

| Port | Flag | Serves |
|---|---|---|
| `9000` | `--addr` | gRPC `CapacityProvider` + `grpc.health.v1` + reflection |
| `9090` | `--metrics-addr` | HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) |

## Helm chart

The chart lives at
[`deploy/helm/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/upcloud/deploy/helm).
It renders a `Deployment` (single replica — one process per zone, owns its
`--state`), a `Service` exposing the gRPC + metrics ports (with Prometheus scrape
annotations), a `ServiceAccount`, and — when enabled — a `ConfigMap` for the
offerings and a `PersistentVolumeClaim` for durable state. It consumes the
`UPCLOUD_USERNAME` / `UPCLOUD_PASSWORD` Secret you create in
[Credentials](credentials.md).

**Install one release per zone**, with a values file per zone:

```sh
helm install bigfleet-upcloud-fi-hel1 providers/upcloud/deploy/helm \
  -n bigfleet --create-namespace \
  -f fi-hel1.values.yaml
```

A minimal `fi-hel1.values.yaml`:

```yaml
image:
  repository: ghcr.io/intunderflow/bigfleet-upcloud
  tag: 0.1.0

# One process per zone. `zone` sets the zone this process serves and `provider`
# is the label stamped on every HostRef.
zone: fi-hel1
provider: upcloud-fi-hel1

# The server settings.
upcloud:
  template: 01000000-0000-4000-8000-000030240200   # OS template storage UUID to clone
  eurUSD: 1.08                                      # EUR->USD conversion for the pinned price table

# The API sub-account Secret (keys: username, password) -> UPCLOUD_USERNAME / UPCLOUD_PASSWORD.
credentials:
  secretName: bigfleet-upcloud-credentials

# SSH delivery of the per-cluster bootstrap blob. The private key authenticates
# the provider; the public key is injected into each server at create.
ssh:
  user: root
  privateKeySecretName: bigfleet-upcloud-ssh    # key: id (PEM private key)
  publicKey: "ssh-ed25519 AAAA... bigfleet-upcloud"

# Durable state on a PersistentVolume: fence marks, the idempotency map, and
# bindings survive restarts. Without it the provider is in-memory only.
state:
  enabled: true
  persistence:
    enabled: true
    size: 1Gi
```

The offerings JSON is delivered through `offerings.content`: set it and the chart
renders the JSON into a ConfigMap, mounts it at
`/etc/bigfleet/offerings/offerings.json`, and passes `--offerings`. Use
`--set-file` so you keep the file out of your values:

```sh
helm install bigfleet-upcloud-fi-hel1 providers/upcloud/deploy/helm \
  -n bigfleet --create-namespace \
  -f fi-hel1.values.yaml \
  --set-file offerings.content=offerings.fi-hel1.json
```

The offerings shape is documented in [Configuration](configuration.md). Always
enable durable `state` on a PersistentVolume in production — without it the
provider is in-memory and cannot recover bindings on restart.

## Flags

Every flag the binary accepts, grouped by what you touch first. The full
reference (defaults, semantics, the create-then-bootstrap model) is in
[Configuration](configuration.md).

**Core**

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address |
| `--provider` | `upcloud` | Label stamped on `HostRef.provider` (e.g. `upcloud-fi-hel1`) |
| `--upcloud-backend` | `auto` | `upcloud` \| `fake` \| `auto` (auto = `upcloud` when credentials **and** `--zone` are set, else `fake`) |
| `--username` / `--password` | _(empty)_ | UpCloud API sub-account credentials (or set `UPCLOUD_USERNAME` / `UPCLOUD_PASSWORD`) |
| `--zone` | _(empty)_ | UpCloud zone id this process serves (e.g. `fi-hel1`). **Required** for the upcloud backend |
| `--state` | _(empty)_ | Durable state file; empty = in-memory only |

**Launch parameters (upcloud backend)**

| Flag | Default | Meaning |
|---|---|---|
| `--template` | _(empty)_ | OS template storage UUID to clone at create. **Required** for the upcloud backend |
| `--base-user-data` | _(empty)_ | File with the generic pre-binding cloud-init baked in at create (installs the on-host hook **only** — never the secret) |
| `--bootstrap-hook` | `/opt/bigfleet/bootstrap` | Image path that applies the delivered bootstrap blob |

**SSH delivery (upcloud backend)**

| Flag | Default | Meaning |
|---|---|---|
| `--ssh-key` | _(empty)_ | SSH private key (PEM) used for Configure/Drain delivery |
| `--ssh-pubkey` | _(empty)_ | Authorized public key injected into servers at create (so `--ssh-key` can authenticate) |
| `--ssh-user` | `root` | SSH user for Configure/Drain delivery |

**Offerings & pricing**

| Flag | Default | Meaning |
|---|---|---|
| `--offerings` | _(empty)_ | JSON offerings file (else a built-in mix sized by `--seed-count`) |
| `--seed-count` | `32` | Speculative slots for the default offerings |
| `--zone-a` / `--zone-b` | `fi-hel1` / `de-fra1` | Zones for the default offerings |
| `--eur-usd` | `1.08` | EUR→USD rate applied to the pinned price table |

**Background, observability & TLS**

| Flag | Default | Meaning |
|---|---|---|
| `--reconcile-interval` | `2m` | Background UpCloud→inventory reconcile interval (`0` = off) |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz` (empty = disabled) |
| `--reflection` | `true` | Register gRPC server reflection (for grpcurl/debugging) |
| `--tls-cert` / `--tls-key` | _(empty)_ | gRPC server certificate + key (PEM); enables TLS |
| `--tls-ca` | _(empty)_ | gRPC client CA bundle (PEM); enables mTLS |

## mTLS

With no `--tls-cert`/`--tls-key` the provider serves **insecure** gRPC — fine only
for trusted in-cluster traffic. For production, terminate mTLS in the provider
itself:

- `--tls-cert` + `--tls-key` enable TLS (TLS 1.3 minimum).
- adding `--tls-ca` (a client CA bundle) enables **mTLS**: the provider then
  requires and verifies a client certificate on every connection.

`--tls-ca` without `--tls-cert`/`--tls-key` is rejected, and supplying only one of
cert/key is rejected — so a half-configured TLS setup fails fast at startup rather
than silently serving plaintext. The chart mounts a standard Kubernetes TLS
Secret at `/etc/bigfleet/tls` and wires the flags for you:

```yaml
tls:
  enabled: true
  mtls: true                       # mount ca.crt and require a verified client cert
  secretName: bigfleet-upcloud-tls # Secret keys: tls.crt, tls.key, ca.crt
```

The full trust model is in [Security](security.md).

## Bringing it up

```sh
helm install bigfleet-upcloud-fi-hel1 providers/upcloud/deploy/helm \
  -n bigfleet -f fi-hel1.values.yaml

kubectl -n bigfleet logs deploy/bigfleet-upcloud-fi-hel1 | grep 'serving CapacityProvider'
kubectl -n bigfleet port-forward deploy/bigfleet-upcloud-fi-hel1 9090:9090 &
curl localhost:9090/readyz   # -> ready once gRPC is serving
```

The pod reports `/readyz` green only after the gRPC server is serving, so wire it
to a readiness probe and let BigFleet dial the `Service` once the probe passes.
