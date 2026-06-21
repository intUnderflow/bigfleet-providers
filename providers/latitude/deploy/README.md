# Deploying the Latitude.sh provider

Production deploy artifacts for the BigFleet Latitude.sh capacity provider: a
container image, a Helm chart, and the credential (token + SSH) Secret wiring.

The provider follows a **one-process-per-site** model. Each process owns a single
Latitude site (`--site-a`, e.g. `ASH`), holds site-scoped inventory/state, and is
the single `CapacityProvider` for that site. To cover several sites, deploy the
chart once per site with a distinct release name, site, and offerings file —
never scale a single release past `replicas: 1`.

> **No IAM, no Terraform.** Unlike a hyperscaler provider, Latitude.sh has no
> IAM/role system. The entire authorisation surface is a single project-scoped
> API token (plus the project id/slug), so this directory ships a **Secret**
> (`secret/`) instead of an `iam/` Terraform module. There is no role to
> provision.

## 1. Build the image

The `providers/latitude` Go module uses a `replace ... => ../..` to resolve the
shared `providerkit` (repo-root) module from the local checkout, so the build
needs the **whole repository** in context. Build from the repo root:

```sh
# from the repository root
docker build -f providers/latitude/deploy/Dockerfile -t ghcr.io/intunderflow/bigfleet-latitude:0.1.0 .
docker push ghcr.io/intunderflow/bigfleet-latitude:0.1.0
```

The multi-stage build compiles with `go -C providers/latitude build -o /out/latitude .`
and ships a `distroless/static:nonroot` final image (uid 65532, no shell). It
exposes the gRPC port (`9000`) and the metrics/health port (`9090`).

## 2. Create the credential Secrets

Mint a **project-scoped** API token in the Latitude.sh dashboard
(**Settings → API Keys**), and create a dedicated SSH key for bootstrap delivery.
Store both as Secrets:

```sh
kubectl -n bigfleet create secret generic bigfleet-latitude-token \
  --from-literal=token="$LATITUDESH_API_TOKEN"

kubectl -n bigfleet create secret generic bigfleet-latitude-ssh \
  --from-file=id_ed25519=./id_ed25519
```

A ready-to-edit manifest for both is in [`secret/latitude-token.example.yaml`](secret/latitude-token.example.yaml).
The chart mounts the token as the `LATITUDESH_API_TOKEN` env var and the SSH key
at `/etc/bigfleet/ssh`. The provider injects a generated SSH **host** key via
first-boot `user_data` and authorises the matching SSH key's **public** half on
every server it deploys, so it can reach the host to deliver the bootstrap over a
pinned-host-key channel. Full guidance — scoping, rotation, never-logged — is on
the [Credentials & auth](../docs/credentials.md) page.

## 3. Install the chart

Write an offerings file (see the provider README / docs for the schema), then
install one release per site:

```sh
helm install latitude-ash providers/latitude/deploy/helm \
  --namespace bigfleet --create-namespace \
  --set image.repository=ghcr.io/intunderflow/bigfleet-latitude \
  --set image.tag=0.1.0 \
  --set site=ASH \
  --set siteB=NYC \
  --set provider=latitude-ash \
  --set project.value=proj_yourprojectid \
  --set latitude.operatingSystem=ubuntu_22_04_x64_lts \
  --set token.secretName=bigfleet-latitude-token \
  --set ssh.secretName=bigfleet-latitude-ssh \
  --set-file offerings.content=offerings.ash.json
```

The chart renders:

- a **Deployment** (`replicas: 1`, `Recreate`) with HTTP probes
  `livenessProbe: /healthz` and `readinessProbe: /readyz` on the metrics port,
  hardened to match the non-root, read-only-rootfs image, with
  `LATITUDESH_API_TOKEN` sourced from the token Secret and the SSH key mounted
  read-only (mode 0400);
- a **Service** exposing the `grpc` port (BigFleet dials this) and a `metrics`
  port carrying `prometheus.io/scrape` annotations;
- a **ServiceAccount**;
- a **ConfigMap** for the offerings (and optional base user-data) + the default
  OS slug;
- an optional **PVC** for durable `--state`.

### Common extras

```sh
# Durable state on a PersistentVolume (recommended in production):
--set state.enabled=true \
--set state.persistence.enabled=true \
--set state.persistence.size=1Gi

# mTLS for the gRPC listener (Secret with tls.crt, tls.key, ca.crt):
--set tls.enabled=true --set tls.mtls=true --set tls.secretName=bigfleet-latitude-tls
```

## Endpoints

- gRPC `CapacityProvider` + `grpc.health.v1` + reflection on `--addr` (`:9000`).
- HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) on
  `--metrics-addr` (`:9090`).

Metrics are namespaced `bigfleet_latitude_*` on an isolated registry (Latitude
API calls, gRPC requests, reconcile + price-refresh runs), plus the standard
Go/process collectors.
