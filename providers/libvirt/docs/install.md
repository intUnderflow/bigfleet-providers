---
title: Install & deploy
description: Run the libvirt provider — the container image, the Helm chart, flags, mTLS, and the libvirt connection wiring.
sidebar:
  order: 1
  label: Install & deploy
---

The libvirt provider is **one process per host-set** (one BigFleet zone per
libvirt host). You run it next to BigFleet, point it at your libvirt hosts and a
golden base image, and BigFleet dials its `--addr`. This page covers the
container image, the Helm chart, the flags you actually need, mTLS, and the
credential wiring.

If you just want to kick the tyres with no libvirt host, the
[overview](/providers/libvirt/) shows the credential-free fake backend — a bare
`./bin/libvirt --seed-count 32` comes up simulating domains with no hypervisor.
Everything below is for real hosts.

## Container image

The binary is a single static Go binary (pure-Go libvirt client, CGO-free); the
image is built from
[`deploy/Dockerfile`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/libvirt/deploy/Dockerfile)
(distroless, non-root, no shell). Build and push it **from the repository root**
(the `providers/libvirt` module's `replace => ../..` needs the whole repo in
context to resolve the `providerkit` root module):

```sh
docker build -t ghcr.io/your-org/bigfleet-libvirt:latest \
  -f providers/libvirt/deploy/Dockerfile .
docker push ghcr.io/your-org/bigfleet-libvirt:latest
```

The entrypoint is the provider binary, so you pass [flags](#flags) as container
args. A bare smoke test (fake backend, no hypervisor) confirms the image runs:

```sh
docker run --rm -p 9000:9000 -p 9090:9090 \
  ghcr.io/your-org/bigfleet-libvirt:latest \
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
[`deploy/helm/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/libvirt/deploy/helm).
It renders a `Deployment` (single replica — one process per host-set, owns its
`--state`), a `Service` exposing the gRPC + metrics ports (with Prometheus
scrape annotations), a `ServiceAccount`, and — when enabled — `ConfigMap`s for
the offerings / instance-type catalog and a `PersistentVolumeClaim` for durable
state. It mounts the libvirt credential Secret you create in
[Credentials](/providers/libvirt/credentials/).

Install it with a values file per host-set:

```sh
helm install bigfleet-libvirt-dc1 providers/libvirt/deploy/helm \
  -n bigfleet --create-namespace \
  -f dc1.values.yaml
```

A minimal `dc1.values.yaml`:

```yaml
image:
  repository: ghcr.io/your-org/bigfleet-libvirt
  tag: latest

# HostRef label stamped on every machine.
provider: libvirt-dc1

# The libvirt hosts: one zone per host. Each zone maps Machine.zone to a host.
connect: "rack1=qemu+ssh://bigfleet@host-a/system?keyfile=/etc/bigfleet/libvirt-ssh/id_ed25519&known_hosts=/etc/bigfleet/libvirt-ssh/known_hosts,rack2=qemu+ssh://bigfleet@host-b/system?keyfile=/etc/bigfleet/libvirt-ssh/id_ed25519&known_hosts=/etc/bigfleet/libvirt-ssh/known_hosts"

# The libvirt domain settings.
libvirt:
  image: ubuntu-24.04.qcow2   # golden base image volume (must ship the bootstrap hook)
  storagePool: default
  network: default

# The Secret holding the SSH key for the qemu+ssh:// connection.
credentials:
  ssh:
    secretName: bigfleet-libvirt-ssh

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
helm install bigfleet-libvirt-dc1 providers/libvirt/deploy/helm \
  -n bigfleet --create-namespace \
  -f dc1.values.yaml \
  --set-file offerings.content=offerings.dc1.json
```

The offerings shape is documented in
[Configuration](/providers/libvirt/configuration/). Always enable durable
`state` on a PersistentVolume in production — without it the provider is
in-memory and cannot recover bindings on restart.

## Flags

Every flag the binary accepts, grouped by what you touch first. The full
reference (defaults, semantics, the bootstrap model) is in
[Configuration](/providers/libvirt/configuration/).

**Core**

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address |
| `--provider` | `libvirt` | Label stamped on `HostRef.provider` (e.g. `libvirt-dc1`) |
| `--libvirt-backend` | `auto` | `libvirt` \| `fake` \| `auto` (auto = `libvirt` when `--connect` is set, else `fake`) |
| `--connect` | _(empty)_ | A bare URI (`qemu:///system`) for the default zone, or a `zone=uri` list for multi-host |
| `--default-zone` | `local` | Zone label for a single bare `--connect` URI |
| `--state` | _(empty)_ | Durable state file; empty = in-memory only |

**Launch parameters (libvirt backend)**

| Flag | Default | Meaning |
|---|---|---|
| `--image` | _(empty)_ | Golden base-image volume the overlay disk backs onto. **Required** for the libvirt backend |
| `--storage-pool` | `default` | libvirt storage pool for overlay + cloud-init volumes |
| `--network` | `default` | libvirt network the domain NIC attaches to |
| `--base-user-data` | _(empty)_ | File with the generic pre-binding cloud-init baked in at define |
| `--capacity-type` | `on_demand` | `on_demand` (Delete implemented) or `bare_metal` (fixed free pool) for the default offerings |

**Instance types & pricing**

| Flag | Default | Meaning |
|---|---|---|
| `--instance-types` | _(built-in)_ | JSON catalog of `name -> {vcpu, memory_mib}` (else built-in `kvm.*` sizes) |
| `--prices` | _(synthetic)_ | Explicit `type=usd` per-hour prices (else synthetic per-vCPU/GiB) |
| `--price-per-vcpu-hour` | `0.0030` | Synthetic USD/hour per vCPU |
| `--price-per-gib-hour` | `0.0008` | Synthetic USD/hour per GiB RAM |

**Offerings**

| Flag | Default | Meaning |
|---|---|---|
| `--offerings` | _(empty)_ | JSON offerings file (else a built-in mix sized by `--seed-count`) |
| `--seed-count` | `32` | Speculative slots for the default offerings |

**Background, observability & TLS**

| Flag | Default | Meaning |
|---|---|---|
| `--reconcile-interval` | `2m` | Background libvirt→inventory reconcile interval (`0` = off) |
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
  mtls: true                            # mount ca.crt and require a verified client cert
  secretName: bigfleet-libvirt-tls-grpc # Secret keys: tls.crt, tls.key, ca.crt
```

The full trust model is in [Security](/providers/libvirt/security/). Note this
gRPC mTLS is **separate** from the libvirt-connection TLS (`qemu+tls://`) covered
in [Credentials](/providers/libvirt/credentials/).

## Bringing it up

```sh
helm install bigfleet-libvirt-dc1 providers/libvirt/deploy/helm \
  -n bigfleet -f dc1.values.yaml

kubectl -n bigfleet logs deploy/bigfleet-libvirt-dc1 | grep 'serving CapacityProvider'
kubectl -n bigfleet port-forward deploy/bigfleet-libvirt-dc1 9090:9090 &
curl localhost:9090/readyz   # -> ready once gRPC is serving
```

The pod reports `/readyz` green only after the gRPC server is serving, so wire
it to a readiness probe and let BigFleet dial the `Service` once the probe
passes.
