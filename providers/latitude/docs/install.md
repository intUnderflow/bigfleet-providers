---
title: Install & deploy
description: Run the Latitude.sh provider — the container image, the Helm chart, flags, mTLS, and the LATITUDESH_API_TOKEN + project Secret.
sidebar:
  order: 1
  label: Install & deploy
---

The Latitude.sh provider is **one process per site**. You run it next to
BigFleet, point it at an OS slug, give it a Latitude.sh API token plus a project
id/slug and an SSH key, and BigFleet dials its `--addr`. This page covers the
container image, the Helm chart, the flags you actually need, mTLS, and the
Secret wiring.

If you just want to kick the tyres with no Latitude account, the
[overview](/providers/latitude/) shows the credential-free fake backend.
Everything below is for a real project.

## Container image

The binary is a single static Go binary; the image is built from
[`deploy/Dockerfile`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/latitude/deploy/Dockerfile)
(distroless, non-root, no shell). Build and push it **from the repository root**
(the `providers/latitude` module's `replace => ../..` needs the whole repo in
context to resolve the `providerkit` root module):

```sh
docker build -t ghcr.io/intunderflow/bigfleet-latitude:0.1.0 \
  -f providers/latitude/deploy/Dockerfile .
docker push ghcr.io/intunderflow/bigfleet-latitude:0.1.0
```

The entrypoint is the provider binary, so you pass [flags](#flags) as container
args. A bare smoke test (fake backend, no credentials) confirms the image runs:

```sh
docker run --rm -p 9000:9000 -p 9090:9090 \
  ghcr.io/intunderflow/bigfleet-latitude:0.1.0 \
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
[`deploy/helm/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/latitude/deploy/helm),
and the full walkthrough is in
[`deploy/README.md`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/latitude/deploy/README.md).
It renders a `Deployment` (single replica — one process per site, owns its
`--state`), a `Service` exposing the gRPC + metrics ports (with Prometheus
scrape annotations), a `ServiceAccount`, a `ConfigMap` for the offerings and OS
slug, and — when enabled — a `PersistentVolumeClaim` for durable state. It
consumes the token Secret (mounted as `LATITUDESH_API_TOKEN`) and the SSH key
Secret you create in [Credentials](/providers/latitude/credentials/).

Install it with a values file per site:

```sh
helm install latitude-ash providers/latitude/deploy/helm \
  -n bigfleet --create-namespace \
  -f ash.values.yaml
```

A minimal `ash.values.yaml`:

```yaml
image:
  repository: ghcr.io/intunderflow/bigfleet-latitude
  tag: "0.1.0"

# One process per site. `site` sets the default offering site and the label
# stamped on every HostRef.
site: ASH
siteB: NYC
provider: latitude-ash

# Every Latitude server operation is scoped to a project (id or slug).
project:
  value: proj_yourprojectid

# The Latitude.sh server settings.
latitude:
  operatingSystem: ubuntu_22_04_x64_lts   # OS slug deployed at create
  bootstrapHook: /opt/bigfleet/bootstrap  # path the deployed OS ships

# The Secret holding the project-scoped API token (key: token).
token:
  secretName: bigfleet-latitude-token

# The Secret holding the SSH private key for Configure/Drain delivery.
ssh:
  secretName: bigfleet-latitude-ssh
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
chart renders the JSON into a ConfigMap, mounts it, and passes `--offerings`.
Use `--set-file` so you keep the file out of your values:

```sh
helm install latitude-ash providers/latitude/deploy/helm \
  -n bigfleet --create-namespace \
  -f ash.values.yaml \
  --set-file offerings.content=offerings.ash.json
```

The offerings shape is documented in
[Configuration](/providers/latitude/configuration/). Always enable durable
`state` on a PersistentVolume in production — without it the provider is
in-memory and cannot recover bindings on restart.

## Flags

Every flag the binary accepts, grouped by what you touch first. The full
reference (defaults, semantics, the bootstrap model) is in
[Configuration](/providers/latitude/configuration/).

**Core**

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address |
| `--provider` | `latitude` | Label stamped on `HostRef.provider` (e.g. `latitude-ash`) |
| `--latitude-backend` | `auto` | `latitude` \| `fake` \| `auto` (auto = `latitude` when **both** a token and a project are set, else `fake`) |
| `--token` | _(empty)_ | Latitude.sh API token (or set `LATITUDESH_API_TOKEN`) |
| `--project` | _(empty)_ | Latitude.sh project id or slug (or set `LATITUDESH_PROJECT`); **required** for the latitude backend |
| `--state` | _(empty)_ | Durable state file; empty = in-memory only |

**Launch parameters (latitude backend)**

| Flag | Default | Meaning |
|---|---|---|
| `--operating-system` | `ubuntu_22_04_x64_lts` | OS slug deployed at `Server` create |
| `--ssh-key` | _(empty)_ | Path to the SSH private key used for Configure/Drain delivery |
| `--ssh-user` | `root` | SSH user for Configure/Drain delivery |
| `--bootstrap-hook` | `/opt/bigfleet/bootstrap` | OS path that applies the delivered bootstrap blob |
| `--base-user-data` | _(empty)_ | File with the generic pre-binding cloud-init baked in at create |

**Offerings**

| Flag | Default | Meaning |
|---|---|---|
| `--offerings` | _(empty)_ | JSON offerings file (else a built-in mix sized by `--seed-count`) |
| `--seed-count` | `32` | Speculative slots for the default offerings |
| `--site-a` / `--site-b` | `ASH` / `NYC` | Sites for the default offerings |

**Background, observability & TLS**

| Flag | Default | Meaning |
|---|---|---|
| `--price-refresh` | `30m` | Price refresh interval (`0` = off) |
| `--reconcile-interval` | `2m` | Background Latitude→inventory reconcile interval (`0` = off) |
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
TLS Secret and wires the flags for you:

```yaml
tls:
  enabled: true
  mtls: true                        # mount ca.crt and require a verified client cert
  secretName: bigfleet-latitude-tls # Secret keys: tls.crt, tls.key, ca.crt
```

The full trust model is in [Security](/providers/latitude/security/).

## Bringing it up

```sh
helm install latitude-ash providers/latitude/deploy/helm \
  -n bigfleet -f ash.values.yaml

kubectl -n bigfleet logs deploy/latitude-ash | grep 'serving CapacityProvider'
kubectl -n bigfleet port-forward deploy/latitude-ash 9090:9090 &
curl localhost:9090/readyz   # -> ready once gRPC is serving
```

The pod reports `/readyz` green only after the gRPC server is serving, so wire
it to a readiness probe and let BigFleet dial the `Service` once the probe
passes.
