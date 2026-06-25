---
title: Install & deploy
description: Run the Proxmox VE provider — the container image, the Helm chart, flags, mTLS on the gRPC listener, and one release per Proxmox cluster.
sidebar:
  order: 1
  label: Install & deploy
---

The Proxmox VE provider is **one process per Proxmox cluster**. You run it next
to BigFleet, point it at the cluster API + a template, give it an API token and
TLS trust material, and BigFleet dials its `--addr`. This page covers the
container image, the Helm chart, the flags you actually need, and the gRPC mTLS
posture.

Everything below is for a real cluster.

## Container image

The binary is a single static Go binary; the image is built from
[`deploy/Dockerfile`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/proxmox/deploy/Dockerfile)
(distroless/static, non-root uid 65532, no shell). It uses the pure-Go
go-proxmox client, so the build is CGO-free. Build and push it **from the
repository root** — the `providers/proxmox` module's `replace => ../..` needs the
whole repo in context to resolve the providerkit (root) module:

```sh
docker build -t ghcr.io/your-org/bigfleet-proxmox:latest \
  -f providers/proxmox/deploy/Dockerfile .
docker push ghcr.io/your-org/bigfleet-proxmox:latest
```

The entrypoint is the provider binary, so you pass [flags](#flags) as container
args. A bare smoke test (fake backend, no credentials) confirms the image runs:

```sh
docker run --rm -p 9000:9000 -p 9090:9090 \
  ghcr.io/your-org/bigfleet-proxmox:latest \
  --seed-count 32 --addr :9000 --metrics-addr :9090
# then: curl localhost:9090/healthz  -> ok
#       curl localhost:9090/readyz   -> ready
```

The container exposes two ports:

| Port | Flag | Serves |
|---|---|---|
| `9000` | `--addr` | gRPC `CapacityProvider` + `grpc.health.v1` + reflection |
| `9090` | `--metrics-addr` | HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) |

See [Observability](/providers/proxmox/observability/) for what `/metrics`
exposes and [Security](/providers/proxmox/security/) for the gRPC mTLS posture.

## Helm chart

The chart lives at
[`deploy/helm/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/proxmox/deploy/helm).
It renders a `Deployment` (single replica, `Recreate` — one process per cluster,
owns its `--state`), a `Service` exposing the gRPC + metrics ports (with
Prometheus scrape annotations), a `ServiceAccount`, and — when enabled —
`ConfigMap`s for the offerings and instance-type catalog and a
`PersistentVolumeClaim` for durable state. It mounts the API-token Secret and the
CA bundle read-only.

The values are **structured** — you set fields like `proxmox.apiURL` and
`proxmox.nodes` and the chart turns them into the right flags. Install one release
per cluster with a values file:

```sh
helm install bigfleet-proxmox-dc1 providers/proxmox/deploy/helm \
  -n bigfleet --create-namespace \
  -f dc1.values.yaml
```

A minimal `dc1.values.yaml`:

```yaml
image:
  repository: ghcr.io/your-org/bigfleet-proxmox
  tag: latest

# One process per cluster. `provider` is the label stamped on every HostRef.
provider: proxmox-dc1

# Connection to the Proxmox cluster API.
proxmox:
  apiURL: https://pve1.example.internal:8006/api2/json
  tokenID: bigfleet@pve!autoscaler         # the token secret comes from a Secret, below
  nodes: pve1,pve2,pve3                     # cluster node names = BigFleet zones
  pool: bigfleet                            # the resource pool clones land in
  templateVMID: 9000                        # the prepared template every clone copies

# TLS trust for the Proxmox API cert (the secret channel). Mount the cluster CA
# via a Secret (credentials.ca, below) OR pin the fingerprint here. Required.
# proxmox.tlsFingerprint: "AB:CD:..."      # alternative to a CA bundle

# The API token secret and the cluster CA, both from Kubernetes Secrets.
credentials:
  token:
    secretName: bigfleet-proxmox-token      # mounted; passed as --proxmox-token-file
    secretKey: token
  ca:
    secretName: bigfleet-proxmox-ca         # mounted; passed as --proxmox-ca-file
    secretKey: ca.pem

# Durable state on a PersistentVolume: the idempotency map, bindings, and
# inventory survive restarts. Without it the provider is in-memory only.
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
helm install bigfleet-proxmox-dc1 providers/proxmox/deploy/helm \
  -n bigfleet --create-namespace \
  -f dc1.values.yaml \
  --set-file offerings.content=offerings.dc1.json
```

An instance-type catalog is delivered the same way via `instanceTypes.content`
(rendered to `/etc/bigfleet/instance-types/instance-types.json`, passed as
`--instance-types`); omit it to use the built-in `pve.*` sizes. The offerings and
instance-type shapes are documented in
[Configuration](/providers/proxmox/configuration/). Always enable durable `state`
on a PersistentVolume in production — without it the provider is in-memory and
cannot recover bindings on restart.

## The credential Secrets

Create the two Secrets the chart mounts: the API token secret and the cluster CA
bundle. The token is read from a **file** (`--proxmox-token-file`) so it never
appears in a process arg list:

```sh
# The API token secret (USER@REALM!TOKENID is set as proxmox.tokenID above).
kubectl -n bigfleet create secret generic bigfleet-proxmox-token \
  --from-literal=token='xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx'

# The Proxmox cluster CA that verifies the API cert.
kubectl -n bigfleet create secret generic bigfleet-proxmox-ca \
  --from-file=ca.pem=/etc/pve/pve-root-ca.pem
```

The full least-privilege token setup (`pveum` user, role, pool, and ACL) and the
TLS-trust choices are on the [Credentials](/providers/proxmox/credentials/) page.

## Flags

Every flag the binary accepts, grouped by what you touch first. The full
reference (defaults, semantics, the template/bootstrap model) is in
[Configuration](/providers/proxmox/configuration/).

**Core**

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address |
| `--provider` | `proxmox` | Label stamped on `HostRef.provider` (e.g. `proxmox-dc1`) |
| `--proxmox-backend` | `auto` | `proxmox` \| `fake` \| `auto` (auto = `proxmox` when `--proxmox-api-url` is set, else `fake`) |
| `--state` | _(empty)_ | Durable state file; empty = in-memory only |

**Proxmox connection (proxmox backend)**

| Flag | Default | Meaning |
|---|---|---|
| `--proxmox-api-url` | _(empty)_ | Proxmox API URL, e.g. `https://host:8006/api2/json`; required for the `proxmox` backend |
| `--proxmox-token-id` | _(empty)_ | API token id `USER@REALM!TOKENID` |
| `--proxmox-token-secret` | _(empty)_ | API token secret (prefer `--proxmox-token-file`) |
| `--proxmox-token-file` | _(empty)_ | File holding the API token secret (wins over `--proxmox-token-secret`) |
| `--proxmox-ca-file` | _(empty)_ | PEM CA bundle verifying the API cert (e.g. `/etc/pve/pve-root-ca.pem`) |
| `--proxmox-tls-fingerprint` | _(empty)_ | Pinned SHA-256 fingerprint of the API cert (alternative to `--proxmox-ca-file`) |
| `--proxmox-pool` | _(empty)_ | Resource pool clones are placed in (least-privilege scope) |
| `--nodes` | _(empty)_ | Comma list of cluster node names, each a BigFleet zone; required for the `proxmox` backend |

**Catalog, offerings & pricing**

| Flag | Default | Meaning |
|---|---|---|
| `--offerings` | _(built-in)_ | JSON offerings file (else a built-in mix sized by `--seed-count`) |
| `--seed-count` | `32` | Speculative slots for the default offerings |
| `--default-zone` | `pve` | Zone seed for the fake backend's two synthetic zones |
| `--instance-types` | _(built-in)_ | JSON instance-type catalog (`name -> {vcpu, memory_mib, template_vmid}`) |
| `--template-vmid` | `9000` | Default source template VMID the default catalog clones from |
| `--prices` | _(empty)_ | Explicit USD/hour per type as `type=usd` pairs |
| `--price-per-vcpu-hour` | `0.0030` | Synthetic USD/hour per vCPU when no explicit price is set |
| `--price-per-gib-hour` | `0.0008` | Synthetic USD/hour per GiB RAM when no explicit price is set |

**Bootstrap & background loops**

| Flag | Default | Meaning |
|---|---|---|
| `--bootstrap-path` | `/run/bigfleet-bootstrap` | In-guest path the bootstrap blob is written to before it is run |
| `--bootstrap-exec` | `/bin/sh` | Comma-separated argv that runs the bootstrap (the path is appended as the final arg) |
| `--reconcile-interval` | `2m` | Background Proxmox→inventory reconcile interval (`0` = off) |

**Observability & TLS (gRPC listener)**

| Flag | Default | Meaning |
|---|---|---|
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz` (empty = disabled) |
| `--reflection` | `true` | Register gRPC server reflection (for grpcurl/debugging) |
| `--tls-cert` / `--tls-key` | _(empty)_ | Server certificate + key (PEM); enables TLS on the gRPC listener |
| `--tls-ca` | _(empty)_ | Client CA bundle (PEM); enables mTLS on the gRPC listener |

## gRPC mTLS

Two separate TLS surfaces exist and should not be confused: the **gRPC listener**
BigFleet dials (the `--tls-*` flags below) and the **Proxmox API connection**
(always verified — see [Credentials](/providers/proxmox/credentials/)). This
section is the gRPC listener.

With no `--tls-cert`/`--tls-key` the provider serves **insecure** gRPC — fine
only for trusted in-cluster traffic. For production, terminate mTLS in the
provider itself:

- `--tls-cert` + `--tls-key` enable TLS (TLS 1.3 minimum).
- adding `--tls-ca` (a client CA bundle) enables **mTLS**: the provider then
  requires and verifies a client certificate on every connection.

`--tls-ca` without `--tls-cert`/`--tls-key` is rejected, and supplying only one
of cert/key is rejected — so a half-configured TLS setup fails fast at startup
rather than silently serving plaintext.

The chart mounts a standard Kubernetes TLS Secret at `/etc/bigfleet/tls` and
wires `--tls-cert`/`--tls-key` (and `--tls-ca` when `tls.mtls` is set) for you —
you only point it at the Secret:

```yaml
tls:
  enabled: true
  mtls: true                       # mount ca.crt and require a verified client cert
  secretName: bigfleet-proxmox-tls # Secret keys: tls.crt, tls.key, ca.crt
```

Create the Secret with the standard TLS keys (`ca.crt` is only needed for mTLS):

```sh
kubectl -n bigfleet create secret generic bigfleet-proxmox-tls \
  --from-file=tls.crt=server.pem \
  --from-file=tls.key=server-key.pem \
  --from-file=ca.crt=client-ca.pem
```

BigFleet must then present a client certificate signed by `ca.crt` when it dials
the provider. The startup log line reports the negotiated mode
(`insecure` / `TLS` / `mTLS`) so you can confirm what is actually serving. The
full trust model is in [Security](/providers/proxmox/security/).

## Bringing it up

Install and watch it come up:

```sh
helm install bigfleet-proxmox-dc1 providers/proxmox/deploy/helm \
  -n bigfleet -f dc1.values.yaml --set-file offerings.content=offerings.dc1.json

kubectl -n bigfleet logs deploy/bigfleet-proxmox-dc1 | grep 'serving CapacityProvider'
kubectl -n bigfleet port-forward deploy/bigfleet-proxmox-dc1 9090:9090 &
curl localhost:9090/readyz   # -> ready once gRPC is serving
```

The pod reports `/readyz` green only after the gRPC server is serving, so wire it
to a readiness probe (the chart does) and let BigFleet dial the `Service` once the
probe passes. From here, see [Configuration](/providers/proxmox/configuration/)
for offerings, the instance-type catalog, and the template/bootstrap model, and
[Credentials](/providers/proxmox/credentials/) for the token and TLS trust setup.
