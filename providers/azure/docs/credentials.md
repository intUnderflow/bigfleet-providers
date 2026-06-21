---
title: Credentials & identity
description: The Azure identity the provider runs as — a user-assigned managed identity with a least-privilege role scoped to the resource group, federated to the chart ServiceAccount via Workload Identity on AKS.
sidebar:
  order: 3
  label: Credentials
---

The Azure provider talks to the Compute and Network resource providers (it
creates VMs, NICs, disks, and extensions, and reads VM-size SKUs) plus the public
Retail Prices API. There is **no hardcoded credential anywhere** — it authenticates
with `azidentity.NewDefaultAzureCredential`, which resolves, in order:

1. **Workload Identity / managed identity** — the production path on AKS. The pod
   gets a federated token for a **user-assigned managed identity**, with a role
   scoped to the target resource group. Nothing is stored in the provider.
2. **Environment service principal** — `AZURE_CLIENT_ID` / `AZURE_TENANT_ID` /
   `AZURE_CLIENT_SECRET` (or a federated-token file), for non-AKS hosts.
3. **Azure CLI** — `az login`, for local development.

You never put cluster-join secrets in provider config — those arrive per-Configure
inside the opaque `bootstrap_blob`.

## The least-privilege role

The provider calls exactly these resource-provider actions; nothing is padding.
The custom role the [Terraform](#terraform) assigns grants only these, scoped to
your resource group:

| Action | Lifecycle call | Why |
|---|---|---|
| `Microsoft.Compute/virtualMachines/read` | List / reconcile / Create-wait | Recover inventory + bindings from the `bigfleet-managed` tag; read VM state. |
| `Microsoft.Compute/virtualMachines/write` | `Create` | Create the VM (size, zone, image, Spot priority, BigFleet tags). |
| `Microsoft.Compute/virtualMachines/delete` | `Delete` | Tear the VM down. |
| `Microsoft.Compute/virtualMachines/start/action` | `Configure` | Power on a stopped/deallocated managed VM before configuring it. |
| `Microsoft.Compute/virtualMachines/extensions/write` | `Configure` / `Drain` | Create-or-update the single CustomScript extension that delivers the bootstrap blob and drains the node (no read/delete). |
| `Microsoft.Compute/disks/*` | `Create` / `Delete` | The OS managed disk the VM creates and releases. |
| `Microsoft.Compute/skus/read` | startup | Resolve each offered size's real vCPU/memory (Resource SKUs) for `Machine.allocatable`. |
| `Microsoft.Network/networkInterfaces/*` | `Create` / `Delete` | Create the VM's NIC and delete it on teardown. |
| `Microsoft.Network/virtualNetworks/subnets/join/action` | `Create` | Attach the NIC to the configured subnet (by id; never read). |

The Retail Prices API the Spot price refresh reads is **public** (no Azure auth),
so it needs no role. If you prefer the built-in **Contributor** role scoped to the
resource group, set `use_custom_role=false` in the Terraform — but the custom role
above is the tighter, recommended grant.

## How the pod gets the identity (AKS Workload Identity)

On AKS the provider gets its identity through **Workload Identity**: a federated
credential binds a user-assigned managed identity to the provider's Kubernetes
ServiceAccount, and the webhook projects a token the SDK exchanges automatically.
The wiring has three parts, all produced by the Terraform and chart:

1. The **managed identity** with the role above, scoped to the resource group.
2. A **federated identity credential** whose subject is
   `system:serviceaccount:<namespace>:<sa-name>` and whose issuer is your AKS
   cluster's OIDC issuer URL.
3. The **ServiceAccount** annotated `azure.workload.identity/client-id` +
   `tenant-id`, and the pod labelled `azure.workload.identity/use: "true"` — both
   rendered by the Helm chart from `serviceAccount.clientId` /
   `serviceAccount.tenantId`.

Enable the Workload Identity + OIDC issuer add-ons on the cluster first:

```sh
az aks update -g aks-rg -n aks --enable-oidc-issuer --enable-workload-identity
az aks show -g aks-rg -n aks --query oidcIssuerProfile.issuerUrl -o tsv
```

## Terraform

[`deploy/terraform`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/azure/deploy/terraform)
provisions exactly this: the user-assigned managed identity, the role assignment
(custom least-privilege role by default, or `Contributor`), and the federated
credential binding it to the chart's ServiceAccount.

```sh
terraform -chdir=providers/azure/deploy/terraform apply \
  -var name=bigfleet-azure-eastus \
  -var location=eastus \
  -var resource_group_name=bigfleet-eastus \
  -var oidc_issuer_url="$(az aks show -g aks-rg -n aks --query oidcIssuerProfile.issuerUrl -o tsv)" \
  -var service_account_namespace=bigfleet \
  -var service_account_name=bigfleet-azure-eastus
# outputs: client_id, tenant_id  -> set serviceAccount.clientId / .tenantId in Helm
```

Wire the outputs into the Helm values:

```yaml
serviceAccount:
  name: bigfleet-azure-eastus
  clientId: <client_id output>
  tenantId: <tenant_id output>
```

One identity per region is recommended (one provider process per region), each
scoped to that region's resource group.

## Verify

After install, confirm the pod resolved its identity — the provider's first
`ListVMs` against the resource group succeeds and the reconcile counter advances:

```sh
kubectl -n bigfleet logs deploy/bigfleet-azure-eastus | grep -i 'serving CapacityProvider'
kubectl -n bigfleet port-forward deploy/bigfleet-azure-eastus 9090:9090 &
curl -s localhost:9090/metrics | grep 'bigfleet_azure_api_calls_total{op="ListVMs"'
# outcome="success" climbing => the managed identity + role are working
```

A blanket authorization failure on the first `ListVMs`/`CreateVM` usually means
the federated credential subject doesn't match the ServiceAccount, or the role
assignment hasn't propagated yet (give it a minute). The local-dev fallback is to
`az login` (or export the `AZURE_*` service-principal env vars) and run the binary
directly. See also [Install & deploy](/providers/azure/install/) for the AKS pod
spec and [Security](/providers/azure/security/) for the trust model.
