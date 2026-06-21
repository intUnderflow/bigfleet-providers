# Deploying the libvirt provider

Production deploy artifacts for the BigFleet libvirt (QEMU/KVM) capacity
provider: a container image, a Helm chart, the libvirt credential Secret wiring,
and the host-side least-privilege setup.

The provider follows a **one-process-per-deployment** model. A deployment is a
set of libvirt hosts — one BigFleet **zone** per host — that one process manages.
It holds deployment-scoped inventory/state and is the single `CapacityProvider`
for those hosts. To cover separate fleets, deploy the chart once each with a
distinct release name and `--connect` list — never scale a single release past
`replicas: 1`.

> **No IAM, no cloud token.** libvirt has no IAM/role system and no API token.
> The authorisation surface is the libvirt **connection** — how the pod reaches
> each host's libvirtd and the least-privilege identity it connects as. So this
> directory ships a credential **Secret** (`secret/`) and the **host-side setup**
> (`host-setup/`) instead of an `iam/` Terraform module.

## 1. Build the image

The `providers/libvirt` Go module uses a `replace ... => ../..` to resolve the
shared `providerkit` (repo-root) module from the local checkout, so the build
needs the **whole repository** in context. The provider uses the **pure-Go**
go-libvirt client, so the build is CGO-free and ships on `distroless/static`.
Build from the repo root:

```sh
# from the repository root
docker build -f providers/libvirt/deploy/Dockerfile -t ghcr.io/intunderflow/bigfleet-libvirt:0.1.0 .
docker push ghcr.io/intunderflow/bigfleet-libvirt:0.1.0
```

The multi-stage build compiles with `go -C providers/libvirt build -o /out/libvirt .`
and ships a `distroless/static:nonroot` final image (uid 65532, no shell). It
exposes the gRPC port (`9000`) and the metrics/health port (`9090`).

## 2. Prepare the hosts and credentials

On each libvirt host, create the least-privilege identity the provider connects
as and stage the storage pool / network / golden base image — see
[`host-setup/`](host-setup/). Then create the credential Secret matching your
transport (SSH key for `qemu+libssh://`, client TLS for `qemu+tls://`):

```sh
kubectl -n bigfleet create secret generic bigfleet-libvirt-ssh \
  --from-file=id_ed25519=./id_ed25519 \
  --from-file=known_hosts=./known_hosts
```

A ready-to-edit manifest covering both transports (and the gRPC mTLS Secret) is
in [`secret/libvirt-credentials.example.yaml`](secret/libvirt-credentials.example.yaml).
Full guidance — scoping, rotation, never-logged — is on the
[Credentials & auth](../docs/credentials.md) page.

## 3. Install the chart

Write an offerings file (see [Configuration](../docs/configuration.md) for the
schema), then install one release per deployment:

```sh
helm install libvirt-dc1 providers/libvirt/deploy/helm \
  --namespace bigfleet --create-namespace \
  --set image.repository=ghcr.io/intunderflow/bigfleet-libvirt \
  --set image.tag=0.1.0 \
  --set provider=libvirt-dc1 \
  --set connect='rack1=qemu+libssh://bigfleet@host-a/system?keyfile=/etc/bigfleet/libvirt-ssh/id_ed25519&known_hosts=/etc/bigfleet/libvirt-ssh/known_hosts&known_hosts_verify=normal,rack2=qemu+libssh://bigfleet@host-b/system?keyfile=/etc/bigfleet/libvirt-ssh/id_ed25519&known_hosts=/etc/bigfleet/libvirt-ssh/known_hosts&known_hosts_verify=normal' \
  --set libvirt.image=ubuntu-24.04.qcow2 \
  --set credentials.ssh.secretName=bigfleet-libvirt-ssh \
  --set-file offerings.content=offerings.dc1.json
```

The chart renders:

- a **Deployment** (`replicas: 1`, `Recreate`) with HTTP probes
  `livenessProbe: /healthz` and `readinessProbe: /readyz` on the metrics port,
  hardened to match the non-root, read-only-rootfs image, with the libvirt
  credentials mounted read-only;
- a **Service** exposing the `grpc` port (BigFleet dials this) and a `metrics`
  port carrying `prometheus.io/scrape` annotations;
- a **ServiceAccount**;
- **ConfigMaps** for the offerings, the instance-type catalog, and optional base
  user-data;
- an optional **PVC** for durable `--state`.

### Common extras

```sh
# Durable state on a PersistentVolume (recommended in production):
--set state.enabled=true \
--set state.persistence.enabled=true \
--set state.persistence.size=1Gi

# mTLS for the gRPC listener (Secret with tls.crt, tls.key, ca.crt):
--set tls.enabled=true --set tls.mtls=true --set tls.secretName=bigfleet-libvirt-tls-grpc

# A fixed bare-metal free pool instead of an on-demand churning pool:
--set capacityType=bare_metal
```

## Endpoints

- gRPC `CapacityProvider` + `grpc.health.v1` + reflection on `--addr` (`:9000`).
- HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) on
  `--metrics-addr` (`:9090`).

Metrics are namespaced `bigfleet_libvirt_*` on an isolated registry (libvirt API
calls, gRPC requests, reconcile runs), plus the standard Go/process collectors.

> The Helm chart and Dockerfile in this directory mirror the certified
> `providers/aws` and `providers/hetzner` deploy artifacts. CI lints the chart
> and builds the image on every change (the `deploy` job). Still run
> `helm template` against your own values before first install to confirm the
> rendered manifests match your cluster's conventions.
