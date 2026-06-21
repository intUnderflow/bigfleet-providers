# Deploying the OVHcloud Public Cloud provider

Production deploy artifacts for the BigFleet OVHcloud Public Cloud capacity
provider: a container image, a Helm chart, the OpenStack-user credential Secret
wiring, and a scoped-user creation script.

The provider follows a **one-process-per-region** model. Each process owns a
single OVH region (`--region`, e.g. `GRA`), holds region-scoped inventory/state,
and is the single `CapacityProvider` for that region. To cover several regions,
deploy the chart once per region with a distinct release name, region, and
offerings file — never scale a single release past `replicas: 1`.

> **OpenStack user, not IAM.** OVH Public Cloud is OpenStack. There is no AWS-style
> IAM role graph; the authorisation surface is a **project-scoped OpenStack user**
> (Keystone v3) with the project `member` role. So this directory ships a
> credential **Secret** (`secret/`) and a least-privilege **user creation script**
> (`openstack/create-scoped-user.sh`) instead of an `iam/` Terraform module.

## 1. Build the image

The `providers/ovhcloud` Go module uses a `replace ... => ../..` to resolve the
shared `providerkit` (repo-root) module from the local checkout, so the build
needs the **whole repository** in context. Build from the repo root:

```sh
# from the repository root
docker build -f providers/ovhcloud/deploy/Dockerfile -t ghcr.io/intunderflow/bigfleet-ovhcloud:0.1.0 .
docker push ghcr.io/intunderflow/bigfleet-ovhcloud:0.1.0
```

The multi-stage build compiles with `go -C providers/ovhcloud build -o /out/ovhcloud .`
and ships a `distroless/static:nonroot` final image (uid 65532, no shell). It
exposes the gRPC port (`9000`) and the metrics/health port (`9090`).

## 2. Create the credentials

Create a dedicated OpenStack user scoped to the Public Cloud project, plus the
SSH keypair used for bootstrap delivery. The helper script does both:

```sh
# authenticated as a project admin (openrc sourced):
providers/ovhcloud/deploy/openstack/create-scoped-user.sh bigfleet-gra <PROJECT_ID> GRA
```

It creates the user, grants only the project `member` role (Compute + Network),
generates an ed25519 keypair, registers the public half in OpenStack, and prints
the `kubectl create secret` commands. A ready-to-edit manifest for both Secrets is
in [`secret/openstack-credentials.example.yaml`](secret/openstack-credentials.example.yaml).

The chart injects the OS_* keys as env vars (`envFrom`) and mounts the SSH key at
`/etc/bigfleet/ssh`. Full guidance — scoping, rotation, never-logged — is on the
[Credentials & auth](../docs/credentials.md) page.

## 3. Install the chart

Write an offerings file (see the provider README / docs for the schema), then
install one release per region:

```sh
helm install ovh-gra providers/ovhcloud/deploy/helm \
  --namespace bigfleet --create-namespace \
  --set image.repository=ghcr.io/intunderflow/bigfleet-ovhcloud \
  --set image.tag=0.1.0 \
  --set region=GRA \
  --set regionB=SBG \
  --set provider=ovh-public-GRA \
  --set ovh.image=<BASE_IMAGE_UUID> \
  --set ovh.keyName=bigfleet-ovh \
  --set ovh.eurToUSD=1.08 \
  --set openstack.secretName=bigfleet-ovh-gra-os \
  --set ssh.secretName=bigfleet-ovh-ssh \
  --set-file offerings.content=offerings.gra.json
```

The chart renders:

- a **Deployment** (`replicas: 1`, `Recreate`) with HTTP probes
  `livenessProbe: /healthz` and `readinessProbe: /readyz` on the metrics port,
  hardened to match the non-root, read-only-rootfs image, with the OS_*
  credentials sourced from the OpenStack Secret (`envFrom`) and the SSH key
  mounted read-only (mode 0400);
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
--set tls.enabled=true --set tls.mtls=true --set tls.secretName=bigfleet-ovh-tls
```

## Endpoints

- gRPC `CapacityProvider` + `grpc.health.v1` + reflection on `--addr` (`:9000`).
- HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) on
  `--metrics-addr` (`:9090`).

Metrics are namespaced `bigfleet_ovh_*` on an isolated registry (OpenStack API
calls, gRPC requests, reconcile runs), plus the standard Go/process collectors.

> **CI-validated.** The `deploy (ovhcloud)` CI job runs `helm lint` on the chart
> and a repo-root `docker build` of the Dockerfile on every change, so both are
> exercised automatically (the chart and Dockerfile are also modelled one-to-one
> on the certified `providers/hetzner` artifacts — same templates, probes, and
> security context). For a production rollout, still render the chart with your
> own values (`helm template … -f your-values.yaml`) to confirm the manifests
> match your environment.
