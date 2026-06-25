---
title: Install & deploy
description: Run the DigitalOcean provider — the container image, the Helm chart, flags, mTLS, the bootstrap channel, and the DIGITALOCEAN_TOKEN Secret.
sidebar:
  order: 1
  label: Install & deploy
---

The DigitalOcean provider is **one process per region**. You run it next to
BigFleet, point it at a base image, give it a DigitalOcean Personal Access Token
and a bootstrap channel, and BigFleet dials its `--addr`. This page covers the
container image, the Helm chart, the flags you actually need, mTLS, and the
Secret wiring.

Everything below is
for a real account.

## Container image

The binary is a single static Go binary; the image is built from
[`deploy/Dockerfile`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/digitalocean/deploy/Dockerfile)
(distroless, non-root, no shell). Build and push it **from the repository root**
(the `providers/digitalocean` module's `replace => ../..` needs the whole repo
in context to resolve the `providerkit` root module):

```sh
docker build -t ghcr.io/intunderflow/bigfleet-digitalocean:0.1.0 \
  -f providers/digitalocean/deploy/Dockerfile .
docker push ghcr.io/intunderflow/bigfleet-digitalocean:0.1.0
```

The entrypoint is the provider binary, so you pass [flags](#flags) as container
args. A bare smoke test (fake backend, no credentials) confirms the image runs:

```sh
docker run --rm -p 9000:9000 -p 9090:9090 \
  ghcr.io/intunderflow/bigfleet-digitalocean:0.1.0 \
  --seed-count 32 --addr :9000 --metrics-addr :9090
# then: curl localhost:9090/healthz  -> ok
#       curl localhost:9090/readyz   -> ready
```

The container exposes two ports for BigFleet and Prometheus (the bootstrap
channel on `--bootstrap-addr` is a third, separate listener — see
[The bootstrap channel](#the-bootstrap-channel)):

| Port | Flag | Serves |
|---|---|---|
| `9000` | `--addr` | gRPC `CapacityProvider` + `grpc.health.v1` + reflection |
| `9090` | `--metrics-addr` | HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) |

## Helm chart

The chart lives at
[`deploy/helm/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/digitalocean/deploy/helm).
It renders a `Deployment` (single replica — one process per region, owns its
`--state`), a `Service` exposing the gRPC + metrics ports (with Prometheus
scrape annotations), a `ServiceAccount`, and — when enabled — a `ConfigMap` for
the offerings and a `PersistentVolumeClaim` for durable state. It consumes the
`DIGITALOCEAN_TOKEN` Secret you create in [Credentials](credentials.md).

Install it with a values file per region:

```sh
helm install bigfleet-digitalocean-nyc3 providers/digitalocean/deploy/helm \
  -n bigfleet --create-namespace \
  -f nyc3.values.yaml
```

A minimal `nyc3.values.yaml`:

```yaml
image:
  repository: ghcr.io/intunderflow/bigfleet-digitalocean
  tag: 0.1.0

# One process per region. `region` sets the region this process serves and
# `provider` is the label stamped on every HostRef.
region: nyc3
provider: digitalocean-nyc3

# The Droplet settings.
digitalocean:
  image: ubuntu-24-04-x64        # base image / snapshot (must ship the on-host agent)

# The Secret holding the PAT (key: token).
token:
  secretName: bigfleet-digitalocean-token

# The on-host agent bootstrap channel (HTTPS). The provider serves it; the
# Droplet's agent fetches its cluster-join blob from `endpoint`, pinning the CA.
# Two distinct Secrets: tlsSecretName holds the channel's server cert/key (a
# standard kubernetes.io/tls Secret: tls.crt, tls.key), and secretName holds the
# HMAC key (key: secret) that mints per-machine agent tokens.
bootstrap:
  enabled: true
  endpoint: https://do-provider.bigfleet.svc:9443
  tlsSecretName: bigfleet-digitalocean-bootstrap-tls   # keys: tls.crt, tls.key (+ ca.crt only if ca: true)
  secretName: bigfleet-digitalocean-bootstrap          # key: secret (BIGFLEET_BOOTSTRAP_SECRET)
  # ca: true   # set only if tlsSecretName also carries a ca.crt for the agent to pin

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
helm install bigfleet-digitalocean-nyc3 providers/digitalocean/deploy/helm \
  -n bigfleet --create-namespace \
  -f nyc3.values.yaml \
  --set-file offerings.content=offerings.nyc3.json
```

The offerings shape is documented in [Configuration](configuration.md). Always
enable durable `state` on a PersistentVolume in production — without it the
provider is in-memory and cannot recover bindings on restart.

## Flags

Every flag the binary accepts, grouped by what you touch first. The full
reference (defaults, semantics, the bootstrap model) is in
[Configuration](configuration.md).

**Core**

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address |
| `--provider` | `digitalocean` | Label stamped on `HostRef.provider` (e.g. `digitalocean-nyc3`) |
| `--do-backend` | `auto` | `digitalocean` \| `fake` \| `auto` (auto = `digitalocean` when a token **and** region are set, else `fake`) |
| `--token` | _(empty)_ | DigitalOcean Personal Access Token (or set `DIGITALOCEAN_TOKEN`) |
| `--region` | _(empty)_ | DigitalOcean region slug this process serves (e.g. `nyc3`). **Required** for the digitalocean backend |
| `--state` | _(empty)_ | Durable state file; empty = in-memory only |

**Launch parameters (digitalocean backend)**

| Flag | Default | Meaning |
|---|---|---|
| `--image` | _(empty)_ | Base image / snapshot slug or id for `Droplets.Create`. **Required** for the digitalocean backend |
| `--base-user-data` | _(empty)_ | File with the generic pre-binding cloud-init baked in at create (installs the on-host agent) |

**Bootstrap channel (digitalocean backend)**

| Flag | Default | Meaning |
|---|---|---|
| `--bootstrap-addr` | _(empty)_ | Address to serve the on-host agent bootstrap channel (HTTPS). **Required** for the digitalocean backend |
| `--bootstrap-endpoint` | _(empty)_ | Externally-reachable URL of the channel, injected into Droplet `user_data`. **Required** |
| `--bootstrap-tls-cert` / `--bootstrap-tls-key` | _(empty)_ | Server certificate + key (PEM) for the channel. **Required** |
| `--bootstrap-ca` | _(server cert)_ | CA bundle the agent pins to verify the provider (defaults to the server cert) |
| `--bootstrap-secret` | _(empty)_ | Stable HMAC secret minting per-machine agent tokens (or set `BIGFLEET_BOOTSTRAP_SECRET`). **Required** for the digitalocean backend — the provider refuses to start without it (a random per-process secret would invalidate issued tokens on restart) |

**Offerings**

| Flag | Default | Meaning |
|---|---|---|
| `--offerings` | _(empty)_ | JSON offerings file (else a built-in mix sized by `--seed-count`) |
| `--seed-count` | `32` | Speculative slots for the default offerings |
| `--region-a` / `--region-b` | `nyc3` / `sfo3` | Regions for the default offerings |

**Background, observability & TLS**

| Flag | Default | Meaning |
|---|---|---|
| `--price-refresh` | `30m` | Price refresh interval (`0` = off) |
| `--reconcile-interval` | `2m` | Background DigitalOcean→inventory reconcile interval (`0` = off) |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz` (empty = disabled) |
| `--reflection` | `true` | Register gRPC server reflection (for grpcurl/debugging) |
| `--tls-cert` / `--tls-key` | _(empty)_ | gRPC server certificate + key (PEM); enables TLS |
| `--tls-ca` | _(empty)_ | gRPC client CA bundle (PEM); enables mTLS |

## The bootstrap channel

The real backend will not start without `--bootstrap-addr`,
`--bootstrap-tls-cert`, and `--bootstrap-tls-key`: the per-cluster bootstrap blob
is a **join secret**, so the provider serves it only over TLS. The on-host agent
(installed by `--base-user-data` at create) fetches its cluster-join blob from
`--bootstrap-endpoint`, pinning `--bootstrap-ca` (or the server cert), and
authenticates with a per-machine token the provider mints. The real backend
**requires** `--bootstrap-secret` (or `BIGFLEET_BOOTSTRAP_SECRET`) — a stable
key so issued agent tokens survive a provider restart. The full model is in
[Configuration](configuration.md) and [Security](security.md).

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
  mtls: true                            # mount ca.crt and require a verified client cert
  secretName: bigfleet-digitalocean-tls # Secret keys: tls.crt, tls.key, ca.crt
```

The full trust model is in [Security](security.md).

## Bringing it up

```sh
helm install bigfleet-digitalocean-nyc3 providers/digitalocean/deploy/helm \
  -n bigfleet -f nyc3.values.yaml

kubectl -n bigfleet logs deploy/bigfleet-digitalocean-nyc3 | grep 'serving CapacityProvider'
kubectl -n bigfleet port-forward deploy/bigfleet-digitalocean-nyc3 9090:9090 &
curl localhost:9090/readyz   # -> ready once gRPC is serving
```

The pod reports `/readyz` green only after the gRPC server is serving, so wire
it to a readiness probe and let BigFleet dial the `Service` once the probe
passes.
