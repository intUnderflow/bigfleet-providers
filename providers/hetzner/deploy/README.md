# Deploying the Hetzner Cloud provider

Production deploy artifacts for the BigFleet Hetzner Cloud capacity provider: a
container image, a Helm chart, and the credential (token + SSH) Secret wiring.

The provider follows a **one-process-per-location** model. Each process owns a
single Hetzner location (`--location-a`, e.g. `nbg1`), holds location-scoped
inventory/state, and is the single `CapacityProvider` for that location. To cover
several locations, deploy the chart once per location with a distinct release
name, location, and offerings file — never scale a single release past
`replicas: 1`.

> **No IAM, no Terraform.** Unlike a hyperscaler provider, Hetzner Cloud has no
> IAM/role system. The entire authorisation surface is a single project-scoped
> API token, so this directory ships a **Secret** (`secret/`) instead of an `iam/`
> Terraform module. There is no role to provision.

## 1. Build the image

The `providers/hetzner` Go module uses a `replace ... => ../..` to resolve the
shared `providerkit` (repo-root) module from the local checkout, so the build
needs the **whole repository** in context. Build from the repo root:

```sh
# from the repository root
docker build -f providers/hetzner/deploy/Dockerfile -t ghcr.io/intunderflow/bigfleet-hetzner:0.1.0 .
docker push ghcr.io/intunderflow/bigfleet-hetzner:0.1.0
```

The multi-stage build compiles with `go -C providers/hetzner build -o /out/hetzner .`
and ships a `distroless/static:nonroot` final image (uid 65532, no shell). It
exposes the gRPC port (`9000`) and the metrics/health port (`9090`).

## 2. Create the credential Secrets

Mint a **project-scoped, Read & Write** API token in the Hetzner Cloud Console
(**Security → API Tokens → Generate API Token**), and create a dedicated SSH key
for bootstrap delivery. Store both as Secrets:

```sh
kubectl -n bigfleet create secret generic bigfleet-hetzner-token \
  --from-literal=token="$HCLOUD_TOKEN"

kubectl -n bigfleet create secret generic bigfleet-hetzner-ssh \
  --from-file=id_ed25519=./id_ed25519
```

A ready-to-edit manifest for both is in [`secret/hcloud-token.example.yaml`](secret/hcloud-token.example.yaml).
The chart mounts the token as the `HCLOUD_TOKEN` env var and the SSH key at
`/etc/bigfleet/ssh`. The base image must authorise the matching SSH **public**
key. Full guidance — scoping, rotation, never-logged — is on the
[Credentials & auth](../docs/credentials.md) page.

## 3. Install the chart

Write an offerings file (see the provider README / docs for the schema), then
install one release per location:

```sh
helm install hetzner-nbg1 providers/hetzner/deploy/helm \
  --namespace bigfleet --create-namespace \
  --set image.repository=ghcr.io/intunderflow/bigfleet-hetzner \
  --set image.tag=0.1.0 \
  --set location=nbg1 \
  --set locationB=fsn1 \
  --set provider=hetzner-nbg1 \
  --set hetzner.image=ubuntu-24.04 \
  --set hetzner.eurToUSD=1.08 \
  --set token.secretName=bigfleet-hetzner-token \
  --set ssh.secretName=bigfleet-hetzner-ssh \
  --set-file offerings.content=offerings.nbg1.json
```

The chart renders:

- a **Deployment** (`replicas: 1`, `Recreate`) with HTTP probes
  `livenessProbe: /healthz` and `readinessProbe: /readyz` on the metrics port,
  hardened to match the non-root, read-only-rootfs image, with `HCLOUD_TOKEN`
  sourced from the token Secret and the SSH key mounted read-only (mode 0400);
- a **Service** exposing the `grpc` port (BigFleet dials this) and a `metrics`
  port carrying `prometheus.io/scrape` annotations;
- a **ServiceAccount**;
- a **ConfigMap** for the offerings (and optional base user-data);
- an optional **PVC** for durable `--state`.

### Common extras

```sh
# Durable state on a PersistentVolume (recommended in production):
--set state.enabled=true \
--set state.persistence.enabled=true \
--set state.persistence.size=1Gi

# mTLS for the gRPC listener (Secret with tls.crt, tls.key, ca.crt):
--set tls.enabled=true --set tls.mtls=true --set tls.secretName=bigfleet-hetzner-tls
```

## Endpoints

- gRPC `CapacityProvider` + `grpc.health.v1` + reflection on `--addr` (`:9000`).
- HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) on
  `--metrics-addr` (`:9090`).

Metrics are namespaced `bigfleet_hetzner_*` on an isolated registry (Hetzner API
calls, gRPC requests, reconcile + price-refresh runs), plus the standard
Go/process collectors.
