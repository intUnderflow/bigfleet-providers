# Deploying the GCP (GCE) provider

Production deploy artifacts for the BigFleet GCP capacity provider: a container
image, a Helm chart, and the GCP service account (Workload Identity / role
binding) setup.

The provider follows a **one-process-per-region** model. Each process owns a
single GCP region (`--region`), holds region-scoped inventory/state, and is the
single `CapacityProvider` for that region. To cover several regions, deploy the
chart once per region with a distinct release name, region, provider service
account, and offerings file — never scale a single release past `replicas: 1`.

## 1. Build the image

The `providers/gcp` Go module uses a `replace ... => ../..` to resolve the
shared `providerkit` (repo-root) module from the local checkout, so the build
needs the **whole repository** in context. Build from the repo root:

```sh
# from the repository root
docker build -f providers/gcp/deploy/Dockerfile -t ghcr.io/intunderflow/bigfleet-gcp:0.1.0 .
docker push ghcr.io/intunderflow/bigfleet-gcp:0.1.0
```

The multi-stage build compiles with `go -C providers/gcp build -o /out/gcp .`
and ships a `distroless/static:nonroot` final image (uid 65532, no shell). It
exposes the gRPC port (`9000`) and the metrics/health port (`9090`).

## 2. Create the provider service account

The provider authenticates via **Application Default Credentials** — use
**Workload Identity** on GKE (preferred, no key files), or a key-file Secret
off-GKE. The least-privilege role + Workload-Identity binding live in
[`sa/main.tf`](sa/main.tf):

```sh
cd providers/gcp/deploy/sa
terraform init
terraform apply \
  -var 'project_id=my-gcp-project' \
  -var 'name=bigfleet-gcp-us-central1' \
  -var 'k8s_namespace=bigfleet' \
  -var 'k8s_service_account=bigfleet-gcp' \
  -var 'instance_service_account=bigfleet-node@my-gcp-project.iam.gserviceaccount.com'
# -> outputs provider_service_account_email
```

What it grants, and why (each line maps to a call the code makes):

| Binding | Role | When |
|---|---|---|
| Compute lifecycle + inventory | `roles/compute.instanceAdmin.v1` (insert/delete/setMetadata/get/list, machineTypes.get) | always |
| Act as the node SA | `roles/iam.serviceAccountUser` on `--instance-service-account` | only with `--instance-service-account` (omit `instance_service_account` otherwise) |
| Workload Identity | `roles/iam.workloadIdentityUser` for `PROJECT.svc.id.goog[NS/KSA]` | always (GKE) |

The **instance** service account (`--instance-service-account`) is a separate
identity the launched nodes run as; give it only what the workloads need. Off-GKE,
use a key-file Secret instead — see [`secret/gcp-key.example.yaml`](secret/gcp-key.example.yaml).

## 3. Install the chart

Write an offerings file (see the provider README for the schema), then install
one release per region, pointing the ServiceAccount at the Google SA from step 2:

```sh
helm install gcp-us-central1 providers/gcp/deploy/helm \
  --namespace bigfleet --create-namespace \
  --set image.repository=ghcr.io/intunderflow/bigfleet-gcp \
  --set image.tag=0.1.0 \
  --set project=my-gcp-project \
  --set region=us-central1 \
  --set provider=gcp-us-central1 \
  --set serviceAccount.name=bigfleet-gcp \
  --set serviceAccount.gcpServiceAccount=bigfleet-gcp-us-central1@my-gcp-project.iam.gserviceaccount.com \
  --set gce.image=projects/debian-cloud/global/images/family/debian-12 \
  --set gce.instanceServiceAccount=bigfleet-node@my-gcp-project.iam.gserviceaccount.com \
  --set-file offerings.content=offerings.us-central1.json
```

The chart renders:

- a **Deployment** (`replicas: 1`, `Recreate`) with HTTP probes
  `livenessProbe: /healthz` and `readinessProbe: /readyz` on the metrics port,
  hardened to match the non-root, read-only-rootfs image;
- a **Service** exposing the `grpc` port (BigFleet dials this) and a `metrics`
  port carrying `prometheus.io/scrape` annotations;
- a **ServiceAccount** annotated `iam.gke.io/gcp-service-account` for Workload
  Identity;
- a **ConfigMap** for the offerings (and optional base startup script);
- an optional **PVC** for durable `--state`.

### Common extras

```sh
# Durable state on a PersistentVolume (recommended in production):
--set state.enabled=true \
--set state.persistence.enabled=true \
--set state.persistence.size=1Gi

# Off-GKE key-file credentials instead of Workload Identity:
--set credentials.secretName=bigfleet-gcp-key

# mTLS for the gRPC listener (Secret with tls.crt, tls.key, ca.crt):
--set tls.enabled=true --set tls.mtls=true --set tls.secretName=bigfleet-gcp-tls
```

## Endpoints

- gRPC `CapacityProvider` + `grpc.health.v1` + reflection on `--addr` (`:9000`).
- HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) on
  `--metrics-addr` (`:9090`).

Metrics are namespaced `bigfleet_gcp_*` on an isolated registry (GCE API calls,
gRPC requests, reconcile runs), plus the standard Go/process collectors.
