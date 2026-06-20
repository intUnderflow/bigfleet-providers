---
title: Install & deploy
description: Run the GCP (GCE) provider — the container image, the Helm chart, flags, mTLS, and Workload Identity wiring.
sidebar:
  order: 1
  label: Install & deploy
---

The GCP provider is **one process per region**. You run it next to BigFleet,
point it at a project, region, and boot image, give it a service account (via
Workload Identity on GKE), and BigFleet dials its `--addr`. This page covers the
container image, the Helm chart, the flags you actually need, mTLS, and the
credentials wiring.

If you just want to kick the tyres with no GCP account, the
[overview](/providers/gcp/) shows the credential-free fake backend. Everything
below is for a real project.

## Container image

The binary is a single static Go binary; the image is built from
[`deploy/Dockerfile`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/gcp/deploy/Dockerfile)
(distroless, non-root, no shell). Build and push it **from the repository root**
(the `providers/gcp` module's `replace => ../..` needs the whole repo in context
to resolve the `providerkit` root module):

```sh
docker build -t ghcr.io/your-org/bigfleet-gcp:latest \
  -f providers/gcp/deploy/Dockerfile .
docker push ghcr.io/your-org/bigfleet-gcp:latest
```

The entrypoint is the provider binary, so you pass [flags](#flags) as container
args. A bare smoke test (fake backend, no credentials) confirms the image runs:

```sh
docker run --rm -p 9000:9000 -p 9090:9090 \
  ghcr.io/your-org/bigfleet-gcp:latest \
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
[`deploy/helm/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/gcp/deploy/helm).
It renders a `Deployment` (single replica — one process per region, owns its
`--state`), a `Service` exposing the gRPC + metrics ports (with Prometheus
scrape annotations), a `ServiceAccount` (annotated for **Workload Identity**),
and — when enabled — a `ConfigMap` for the offerings and a
`PersistentVolumeClaim` for durable state.

Install it with a values file per region:

```sh
helm install bigfleet-gcp-us-central1 providers/gcp/deploy/helm \
  -n bigfleet --create-namespace \
  -f us-central1.values.yaml
```

A minimal `us-central1.values.yaml`:

```yaml
image:
  repository: ghcr.io/your-org/bigfleet-gcp
  tag: latest

# One process per region. `region` sets the GCE region; `provider` is the label
# stamped on every HostRef.
project: my-gcp-project
region: us-central1
provider: gcp-us-central1

gce:
  image: projects/debian-cloud/global/images/family/debian-12  # boot image (ships the bootstrap hook)
  network: global/networks/default
  diskSizeGb: 20
  instanceServiceAccount: bigfleet-node@my-gcp-project.iam.gserviceaccount.com

# Workload Identity: bind this Kubernetes SA to the Google SA from deploy/sa.
serviceAccount:
  create: true
  name: bigfleet-gcp
  gcpServiceAccount: bigfleet-gcp@my-gcp-project.iam.gserviceaccount.com

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
helm install bigfleet-gcp-us-central1 providers/gcp/deploy/helm \
  -n bigfleet --create-namespace \
  -f us-central1.values.yaml \
  --set-file offerings.content=offerings.us-central1.json
```

The offerings shape is documented in
[Configuration](/providers/gcp/configuration/). Always enable durable `state`
on a PersistentVolume in production — without it the provider is in-memory and
cannot recover bindings on restart.

## Flags

Every flag the binary accepts, grouped by what you touch first. The full
reference (defaults, semantics, the bootstrap model) is in
[Configuration](/providers/gcp/configuration/).

**Core**

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address |
| `--provider` | `gcp` | Label stamped on `HostRef.provider` (e.g. `gcp-us-central1`) |
| `--gcp-backend` | `auto` | `gcp` \| `fake` \| `auto` (auto = `gcp` when `--region` is set, else `fake`) |
| `--project` | _(empty)_ | GCP project id (**required** for the gcp backend) |
| `--region` | _(empty)_ | GCP region, e.g. `us-central1` (**required** for the gcp backend) |
| `--state` | _(empty)_ | Durable state file; empty = in-memory only |

**Launch parameters (gcp backend)**

| Flag | Default | Meaning |
|---|---|---|
| `--image` | `projects/debian-cloud/global/images/family/debian-12` | Boot disk source image for `Instances.Insert` |
| `--network` | `global/networks/default` | VPC network for the instance NIC |
| `--subnetwork` | _(empty)_ | Subnetwork for the NIC, e.g. `regions/<r>/subnetworks/<s>` |
| `--disk-size-gb` | `20` | Boot disk size in GiB |
| `--instance-service-account` | _(empty)_ | Service account the launched instances run as (default: project default) |
| `--base-startup-script` | _(empty)_ | File with the generic pre-binding startup script baked in at Insert |

**Offerings**

| Flag | Default | Meaning |
|---|---|---|
| `--offerings` | _(empty)_ | JSON offerings file (else a built-in mix sized by `--seed-count`) |
| `--seed-count` | `32` | Speculative slots for the default offerings |
| `--zone-a` / `--zone-b` | `<region>-a` / `<region>-b` | Zones for the default offerings |

**Background, observability & TLS**

| Flag | Default | Meaning |
|---|---|---|
| `--reconcile-interval` | `2m` | Background GCE→inventory reconcile interval (`0` = off) |
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
  mtls: true                  # mount ca.crt and require a verified client cert
  secretName: bigfleet-gcp-tls # Secret keys: tls.crt, tls.key, ca.crt
```

The full trust model is in [Security](/providers/gcp/security/).

## Bringing it up

```sh
helm install bigfleet-gcp-us-central1 providers/gcp/deploy/helm \
  -n bigfleet -f us-central1.values.yaml

kubectl -n bigfleet logs deploy/bigfleet-gcp-us-central1 | grep 'serving CapacityProvider'
kubectl -n bigfleet port-forward deploy/bigfleet-gcp-us-central1 9090:9090 &
curl localhost:9090/readyz   # -> ready once gRPC is serving
```

The pod reports `/readyz` green only after the gRPC server is serving, so wire
it to a readiness probe and let BigFleet dial the `Service` once the probe
passes.
