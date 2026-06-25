---
title: Install & deploy
description: Run the Scaleway provider — the container image, the Helm chart, flags, mTLS, and the SCW_* credentials Secret.
sidebar:
  order: 1
  label: Install & deploy
---

The Scaleway provider is **one process per zone**, **one substrate per process**.
You run it next to BigFleet, point it at a base image, give it a Scaleway API key,
and BigFleet dials its `--addr`. This page covers the container image, the Helm
chart, the flags you actually need, mTLS, and the Secret wiring.

Everything below is for a real project. (A credential-free in-memory **fake**
backend exists for the conformance/certification run only; it is testing-only and
must be requested with `--use-fake-backend`.)

## Container image

The binary is a single static Go binary; the image is built from
[`deploy/Dockerfile`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/scaleway/deploy/Dockerfile)
(distroless, non-root uid 65532, no shell). Build and push it **from the
repository root** (the `providers/scaleway` module's `replace => ../..` needs the
whole repo in context to resolve the `providerkit` root module):

```sh
docker build -f providers/scaleway/deploy/Dockerfile \
  -t ghcr.io/intunderflow/bigfleet-scaleway:0.1.0 .
docker push ghcr.io/intunderflow/bigfleet-scaleway:0.1.0
```

The entrypoint is the provider binary, so you pass [flags](#flags) as container
args. A bare smoke test (fake backend, no credentials) confirms the image runs:

```sh
docker run --rm -p 9000:9000 -p 9090:9090 \
  ghcr.io/intunderflow/bigfleet-scaleway:0.1.0 \
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
[`deploy/helm/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/scaleway/deploy/helm).
It renders a `Deployment` (single replica, `Recreate` — one process per zone, owns
its `--state`), a `Service` exposing the gRPC + metrics ports — plus the
`bootstrap` port (`9443`) when the bootstrap channel is enabled — with Prometheus
scrape annotations, a `ServiceAccount`, a `ConfigMap` for the offerings (and
optional base user-data), and — when enabled — a `PersistentVolumeClaim` for
durable state. It consumes the `SCW_*` credentials Secret, plus the bootstrap HMAC
and TLS Secrets, you create in
[Credentials](/providers/scaleway/credentials/).

Install it with a values file per zone:

```sh
helm install scaleway-fr-par-1 providers/scaleway/deploy/helm \
  -n bigfleet --create-namespace \
  -f fr-par-1.values.yaml
```

A minimal `fr-par-1.values.yaml`:

```yaml
image:
  repository: ghcr.io/intunderflow/bigfleet-scaleway
  tag: 0.1.0

# One process per zone, one substrate. `zone` sets the default offering zone and
# the region this process serves; `provider` is the label stamped on every HostRef.
zone: fr-par-1
provider: scaleway-fr-par
substrate: instances        # instances (ON_DEMAND) | elastic-metal (BARE_METAL)

# The Scaleway server settings.
scaleway:
  image: ubuntu_jammy       # base image (installs the on-host agent at first boot)
  eurToUSD: 1.08            # FX rate applied to Scaleway's EUR prices

# The Secret holding the API key (keys: SCW_ACCESS_KEY/SCW_SECRET_KEY/SCW_DEFAULT_PROJECT_ID).
credentials:
  secretName: bigfleet-scaleway-creds

# The Configure bootstrap channel: the on-host agent dials this endpoint, the
# provider authorises it with a per-machine HMAC token and serves the blob over TLS.
bootstrap:
  endpoint: https://scaleway-fr-par.bigfleet.svc:9443
  # kubernetes.io/tls Secret with tls.crt, tls.key, [ca.crt].
  tls:
    secretName: bigfleet-scaleway-bootstrap-tls
  # Secret with the HMAC secret (key: bootstrap-secret), exposed as
  # BIGFLEET_BOOTSTRAP_SECRET. Pin it so tokens survive a restart.
  secret:
    secretName: bigfleet-scaleway-bootstrap

# Durable state on a PersistentVolume: fence marks, the idempotency map, and
# bindings survive restarts. Without it the provider is in-memory only.
state:
  enabled: true
  persistence:
    enabled: true
    size: 1Gi
```

The offerings JSON is delivered through `offerings.content`: set it and the chart
renders the JSON into a ConfigMap, mounts it, and passes `--offerings`. Use
`--set-file` so you keep the file out of your values:

```sh
helm install scaleway-fr-par-1 providers/scaleway/deploy/helm \
  -n bigfleet --create-namespace \
  -f fr-par-1.values.yaml \
  --set-file offerings.content=offerings.fr-par-1.json
```

The offerings shape is documented in
[Configuration](/providers/scaleway/configuration/). Always enable durable
`state` on a PersistentVolume in production — without it the provider is in-memory
and cannot recover bindings on restart. To cover several zones or both substrates,
install the chart again with a distinct release name and offerings file; never
scale a single release past `replicas: 1`.

## Flags

Every flag the binary accepts, grouped by what you touch first. The full
reference (defaults, semantics, the bootstrap model) is in
[Configuration](/providers/scaleway/configuration/).

**Core**

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address |
| `--provider` | `scaleway` | Label stamped on `HostRef.provider` (e.g. `scaleway-fr-par`) |
| `--substrate` | `instances` | `instances` (ON_DEMAND) \| `elastic-metal` (BARE_METAL) |
| `--scaleway-backend` | `auto` | `scaleway` \| `fake` \| `auto` (auto = `scaleway` when credentials are set, else `fake`) |
| `--state` | _(empty)_ | Durable state file; empty = in-memory only |

**Credentials**

| Flag | Default | Meaning |
|---|---|---|
| `--access-key` | _(empty)_ | Scaleway access key (or set `SCW_ACCESS_KEY`) |
| `--secret-key` | _(empty)_ | Scaleway secret key (or set `SCW_SECRET_KEY`) |
| `--project-id` | _(empty)_ | Scaleway project id (or set `SCW_DEFAULT_PROJECT_ID`) |

**Launch parameters (scaleway backend)**

| Flag | Default | Meaning |
|---|---|---|
| `--image` | _(empty)_ | Base image label/id for `CreateServer`. **Required** for the scaleway backend |
| `--base-user-data` | _(empty)_ | File with the generic pre-binding cloud-init baked in at create (installs the on-host agent) |
| `--bootstrap-addr` | _(empty)_ | Address the provider serves the on-host agent bootstrap channel on (HTTPS, e.g. `:9443`). **Required** for the scaleway backend |
| `--bootstrap-endpoint` | _(empty)_ | Externally-reachable URL of the channel, injected into server `user_data` so the agent can dial back. **Required** for the scaleway backend |
| `--bootstrap-tls-cert` / `--bootstrap-tls-key` | _(empty)_ | Server cert/key (PEM) for the bootstrap channel. **Required** for the scaleway backend |
| `--bootstrap-ca` | _(server cert)_ | CA bundle (PEM) the agent pins to verify the provider (defaults to the server cert) |
| `--bootstrap-secret` | _(random)_ | HMAC secret minting per-machine agent tokens (or `BIGFLEET_BOOTSTRAP_SECRET`; random if unset — pin it in production) |
| `--eur-usd` | `1.08` | EUR→USD conversion rate applied to Scaleway prices |

**Offerings**

| Flag | Default | Meaning |
|---|---|---|
| `--offerings` | _(empty)_ | JSON offerings file (else a built-in mix sized by `--seed-count`) |
| `--seed-count` | `32` | Speculative slots for the default offerings |
| `--zone` | `fr-par-1` | The single Scaleway zone this process serves; all offerings must be in this zone |

**Background, observability & TLS**

| Flag | Default | Meaning |
|---|---|---|
| `--price-refresh` | `30m` | Price refresh interval (`0` = off) |
| `--reconcile-interval` | `2m` | Background Scaleway→inventory reconcile interval (`0` = off) |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz` (empty = disabled) |
| `--reflection` | `true` | Register gRPC server reflection (for grpcurl/debugging) |
| `--tls-cert` / `--tls-key` | _(empty)_ | Server certificate + key (PEM); enables TLS |
| `--tls-ca` | _(empty)_ | Client CA bundle (PEM); enables mTLS |

## mTLS

With no `--tls-cert`/`--tls-key` the provider serves **insecure** gRPC — fine only
for trusted in-cluster traffic. For production, terminate mTLS in the provider
itself:

- `--tls-cert` + `--tls-key` enable TLS (TLS 1.3 minimum).
- adding `--tls-ca` (a client CA bundle) enables **mTLS**: the provider then
  requires and verifies a client certificate on every connection.

`--tls-ca` without `--tls-cert`/`--tls-key` is rejected, and supplying only one of
cert/key is rejected — so a half-configured TLS setup fails fast at startup rather
than silently serving plaintext. The chart mounts a standard Kubernetes TLS Secret
and wires the flags for you:

```yaml
tls:
  enabled: true
  mtls: true                        # mount ca.crt and require a verified client cert
  secretName: bigfleet-scaleway-tls # Secret keys: tls.crt, tls.key, ca.crt
```

The full trust model is in [Security](/providers/scaleway/security/).

## Bringing it up

```sh
helm install scaleway-fr-par-1 providers/scaleway/deploy/helm \
  -n bigfleet -f fr-par-1.values.yaml

kubectl -n bigfleet logs deploy/scaleway-fr-par-1 | grep 'serving CapacityProvider'
kubectl -n bigfleet port-forward deploy/scaleway-fr-par-1 9090:9090 &
curl localhost:9090/readyz   # -> ready once gRPC is serving
```

The pod reports `/readyz` green only after the gRPC server is serving, so wire it
to a readiness probe and let BigFleet dial the `Service` once the probe passes.
