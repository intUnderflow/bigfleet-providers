# Deploying the Azure provider

Production deploy artifacts for the BigFleet Azure capacity provider: a container
image, a Helm chart, and the identity Terraform (managed identity + role +
Workload Identity federation).

The provider follows a **one-process-per-region** model. Each process owns a
single Azure region (`--location`), holds region-scoped inventory/state, and is
the single `CapacityProvider` for that region. To cover several regions, deploy
the chart once per region with a distinct release name, location, managed
identity, and offerings file — never scale a single release past `replicas: 1`.

> **Tooling note:** these artifacts were authored against the certified
> `providers/aws/deploy` and `providers/hetzner/deploy` references. `helm`,
> `terraform`/`tofu`, and a running Docker daemon were **not available** in the
> authoring environment, so the chart/Terraform were not rendered/validated
> there; the image's Go build path is the exact `go -C providers/azure build`
> that `make build-azure` runs green. Run `helm template`, `tofu validate`, and
> `docker build` in your environment before relying on them.

## 1. Build the image

The `providers/azure` Go module uses a `replace ... => ../..` to resolve the
shared `providerkit` (repo-root) module from the local checkout, so the build
needs the **whole repository** in context. Build from the repo root:

```sh
# from the repository root
docker build -f providers/azure/deploy/Dockerfile -t ghcr.io/intunderflow/bigfleet-azure:0.1.0 .
docker push ghcr.io/intunderflow/bigfleet-azure:0.1.0
```

The multi-stage build compiles with `go -C providers/azure build -o /out/azure .`
and ships a `distroless/static:nonroot` final image (uid 65532, no shell). It
exposes the gRPC port (`9000`) and the metrics/health port (`9090`).

## 2. Create the managed identity

The provider authenticates via `azidentity.DefaultAzureCredential` — use
**Workload Identity** on AKS (preferred), an env-var service principal elsewhere,
or `az login` locally. Nothing is hardcoded.

The Terraform in [`terraform/`](terraform) creates a user-assigned managed
identity, assigns it a least-privilege role scoped to the target resource group
(a custom role by default, or `Contributor`), and federates it to the chart's
ServiceAccount.

```sh
cd providers/azure/deploy/terraform
terraform init
terraform apply \
  -var 'name=bigfleet-azure-eastus' \
  -var 'location=eastus' \
  -var 'resource_group_name=bigfleet-eastus' \
  -var "oidc_issuer_url=$(az aks show -g aks-rg -n aks --query oidcIssuerProfile.issuerUrl -o tsv)" \
  -var 'service_account_namespace=bigfleet' \
  -var 'service_account_name=bigfleet-azure-eastus'
# -> outputs client_id, tenant_id
```

What the custom role grants, and why (each line maps to a call the code makes):

| Actions | When |
|---|---|
| `Microsoft.Compute/virtualMachines/{read,write,delete}` | Create / Delete / inventory |
| `Microsoft.Compute/virtualMachines/start/action` | power on a stopped host at Configure |
| `Microsoft.Compute/virtualMachines/extensions/{read,write,delete}` | Configure / Drain (CustomScript) |
| `Microsoft.Compute/disks/{read,write,delete}` | the OS managed disk |
| `Microsoft.Compute/skus/read` | allocatable lookup (Resource SKUs) |
| `Microsoft.Network/networkInterfaces/{read,write,delete}` | the VM's NIC |
| `Microsoft.Network/virtualNetworks/subnets/{join/action,read}` | attach the NIC to the subnet |

Set `-var use_custom_role=false` to assign the built-in **Contributor** role
scoped to the resource group instead. The role is scoped to the **resource
group**, not the subscription.

## 3. Install the chart

Write an offerings file (see the provider README / `docs/configuration.md` for
the schema), then install one release per region, pointing
`serviceAccount.clientId` / `serviceAccount.tenantId` at the Terraform outputs
from step 2:

```sh
helm install azure-eastus providers/azure/deploy/helm \
  --namespace bigfleet --create-namespace \
  --set image.repository=ghcr.io/intunderflow/bigfleet-azure \
  --set image.tag=0.1.0 \
  --set location=eastus \
  --set provider=azure-eastus \
  --set serviceAccount.name=bigfleet-azure-eastus \
  --set serviceAccount.clientId=11111111-1111-1111-1111-111111111111 \
  --set serviceAccount.tenantId=22222222-2222-2222-2222-222222222222 \
  --set azure.subscriptionId=00000000-0000-0000-0000-000000000000 \
  --set azure.resourceGroup=bigfleet-eastus \
  --set azure.subnetId=/subscriptions/.../subnets/nodes \
  --set azure.image=Canonical:ubuntu-24_04-lts:server:latest \
  --set-file offerings.content=offerings.eastus.json
```

The chart renders:

- a **Deployment** (`replicas: 1`, `Recreate`) with HTTP probes
  `livenessProbe: /healthz` and `readinessProbe: /readyz` on the metrics port,
  hardened to match the non-root, read-only-rootfs image, and labelled
  `azure.workload.identity/use: "true"`;
- a **Service** exposing the `grpc` port (BigFleet dials this) and a `metrics`
  port carrying `prometheus.io/scrape` annotations;
- a **ServiceAccount** annotated `azure.workload.identity/client-id` +
  `tenant-id` for Workload Identity;
- a **ConfigMap** for the offerings (and optional base user-data / SSH key);
- an optional **PVC** for durable `--state`.

### Common extras

```sh
# Durable state on a PersistentVolume (recommended in production):
--set state.enabled=true \
--set state.persistence.enabled=true \
--set state.persistence.size=1Gi

# An SSH public key authorised on every VM (break-glass; the lifecycle uses
# CustomScript extensions, not SSH):
--set-file azure.sshPublicKey=id.pub

# mTLS for the gRPC listener (Secret with tls.crt, tls.key, ca.crt):
--set tls.enabled=true --set tls.mtls=true --set tls.secretName=bigfleet-azure-tls
```

## Endpoints

- gRPC `CapacityProvider` + `grpc.health.v1` + reflection on `--addr` (`:9000`).
- HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) on
  `--metrics-addr` (`:9090`).

Metrics are namespaced `bigfleet_azure_*` on an isolated registry (Azure API
calls, gRPC requests, reconcile + price-refresh runs, observed Spot evictions),
plus the standard Go/process collectors.
