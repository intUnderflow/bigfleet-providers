# Deploying the Scaleway provider

Production deploy artifacts for the BigFleet Scaleway capacity provider: a
container image, a Helm chart, and the credential (API key) setup.

The provider follows a **one-process-per-region/zone** model. Each process owns a
single Scaleway zone (`--zone-a`, e.g. `fr-par-1`) and one substrate
(`--substrate=instances` or `--substrate=elastic-metal`), holds zone-scoped
inventory/state, and is the single `CapacityProvider` for that pair. To cover
several zones or both substrates, deploy the chart once per (zone, substrate)
with a distinct release name and offerings file — never scale a single release
past `replicas: 1`.

## 1. Build the image

The `providers/scaleway` Go module uses a `replace ... => ../..` to resolve the
shared `providerkit` (repo-root) module from the local checkout, so the build
needs the **whole repository** in context. Build from the repo root:

```sh
# from the repository root
docker build -f providers/scaleway/deploy/Dockerfile -t ghcr.io/intunderflow/bigfleet-scaleway:0.1.0 .
docker push ghcr.io/intunderflow/bigfleet-scaleway:0.1.0
```

The multi-stage build compiles with `go -C providers/scaleway build -o /out/scaleway .`
and ships a `distroless/static:nonroot` final image (uid 65532, no shell). It
exposes the gRPC port (`9000`) and the metrics/health port (`9090`).

## 2. Create the credentials

The provider authenticates with a least-privilege Scaleway **IAM application API
key** (access key + secret key) scoped to one project. Create it with the
Terraform in [`iam/`](iam/):

```sh
cd providers/scaleway/deploy/iam
tofu init      # or: terraform init
tofu apply -var organization_id=<org> -var project_id=<proj> \
  -var name=bigfleet-scaleway-fr-par
# add -var enable_elastic_metal=true for an Elastic Metal deployment
```

Then store the outputs as a Secret (the chart mounts them as `SCW_ACCESS_KEY` /
`SCW_SECRET_KEY` / `SCW_DEFAULT_PROJECT_ID`):

```sh
kubectl -n bigfleet create secret generic bigfleet-scaleway-creds \
  --from-literal=SCW_ACCESS_KEY="$(tofu output -raw access_key)" \
  --from-literal=SCW_SECRET_KEY="$(tofu output -raw secret_key)" \
  --from-literal=SCW_DEFAULT_PROJECT_ID="$(tofu output -raw project_id)"

# Shared token the on-host bootstrap agent uses during Configure:
kubectl -n bigfleet create secret generic bigfleet-scaleway-agent \
  --from-literal=agent-token="$(openssl rand -hex 32)"
```

A ready-to-edit manifest for both Secrets is in
[`secret/scaleway-creds.example.yaml`](secret/scaleway-creds.example.yaml). Full
guidance — scoping, rotation, never-logged — is on the
[Credentials & auth](../docs/credentials.md) page.

## 3. Install the chart

Write an offerings file (see the provider README / docs for the schema), then
install one release per zone:

```sh
helm install scaleway-fr-par-1 providers/scaleway/deploy/helm \
  --namespace bigfleet --create-namespace \
  --set image.repository=ghcr.io/intunderflow/bigfleet-scaleway \
  --set image.tag=0.1.0 \
  --set zone=fr-par-1 \
  --set zoneB=nl-ams-1 \
  --set provider=scaleway-fr-par \
  --set substrate=instances \
  --set scaleway.image=ubuntu_jammy \
  --set scaleway.eurToUSD=1.08 \
  --set credentials.secretName=bigfleet-scaleway-creds \
  --set agentToken.secretName=bigfleet-scaleway-agent \
  --set-file offerings.content=offerings.fr-par-1.json
```

The chart renders:

- a **Deployment** (`replicas: 1`, `Recreate`) with HTTP probes
  `livenessProbe: /healthz` and `readinessProbe: /readyz` on the metrics port,
  hardened to match the non-root, read-only-rootfs image, with the `SCW_*`
  credentials sourced from the Secret;
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
--set tls.enabled=true --set tls.mtls=true --set tls.secretName=bigfleet-scaleway-tls

# Elastic Metal (BARE_METAL) substrate instead of Instances:
--set substrate=elastic-metal
```

## Endpoints

- gRPC `CapacityProvider` + `grpc.health.v1` + reflection on `--addr` (`:9000`).
- HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) on
  `--metrics-addr` (`:9090`).

Metrics are namespaced `bigfleet_scaleway_*` on an isolated registry (Scaleway
API calls, gRPC requests, reconcile + price-refresh runs), plus the standard
Go/process collectors.
