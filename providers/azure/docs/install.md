---
title: Install & deploy
description: Run the Azure provider — the container image, the Helm chart, flags, mTLS, and running it on AKS with Workload Identity.
sidebar:
  order: 1
  label: Install & deploy
---

The Azure provider is **one process per region**. You run it next to BigFleet,
point it at a subscription + resource group + subnet, give it an Azure identity,
and BigFleet dials its `--addr`. This page covers the container image, the Helm
chart, the flags you actually need, mTLS, and the AKS + Workload Identity path.

Everything
below is for a real region.

## Container image

The binary is a single static Go binary; the image is built from
[`deploy/Dockerfile`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/azure/deploy/Dockerfile)
(distroless, non-root, no shell). Build and push it **from the repository root**
— the `providers/azure` module's `replace => ../..` needs the whole repo in
context to resolve the providerkit (root) module:

```sh
docker build -t ghcr.io/your-org/bigfleet-azure:latest \
  -f providers/azure/deploy/Dockerfile .
docker push ghcr.io/your-org/bigfleet-azure:latest
```

The entrypoint is the provider binary, so you pass [flags](#flags) as container
args. A bare smoke test (fake backend, no credentials) confirms the image runs:

```sh
docker run --rm -p 9000:9000 -p 9090:9090 \
  ghcr.io/your-org/bigfleet-azure:latest \
  --seed-count 32 --addr :9000 --metrics-addr :9090
# then: curl localhost:9090/healthz  -> ok
#       curl localhost:9090/readyz   -> ready
```

The container exposes two ports:

| Port | Flag | Serves |
|---|---|---|
| `9000` | `--addr` | gRPC `CapacityProvider` + `grpc.health.v1` + reflection |
| `9090` | `--metrics-addr` | HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) |

See [Observability](/providers/azure/observability/) for what `/metrics` exposes
and [Security](/providers/azure/security/) for the mTLS posture.

## Helm chart

The chart lives at
[`deploy/helm/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/azure/deploy/helm).
It renders a `Deployment` (single replica — one process per region, owns its
`--state`), a `Service` exposing the gRPC + metrics ports (with Prometheus
scrape annotations), a `ServiceAccount` (annotated for Workload Identity), and —
when enabled — a `ConfigMap` for the offerings and a `PersistentVolumeClaim` for
durable state.

Install it with a values file per region:

```sh
helm install bigfleet-azure-eastus providers/azure/deploy/helm \
  -n bigfleet --create-namespace \
  -f eastus.values.yaml \
  --set-file offerings.content=offerings.eastus.json
```

A minimal `eastus.values.yaml`:

```yaml
image:
  repository: ghcr.io/your-org/bigfleet-azure
  tag: latest

# One process per region. `location` sets --location; `provider` is the label
# stamped on every HostRef.
location: eastus
provider: azure-eastus

azure:
  subscriptionId: 00000000-0000-0000-0000-000000000000
  resourceGroup: bigfleet-eastus
  subnetId: /subscriptions/.../resourceGroups/net/providers/Microsoft.Network/virtualNetworks/vnet/subnets/nodes
  image: Canonical:ubuntu-24_04-lts:server:latest

# Workload Identity: the chart annotates the ServiceAccount with these (see below).
serviceAccount:
  clientId: 11111111-1111-1111-1111-111111111111
  tenantId: 22222222-2222-2222-2222-222222222222

# Durable state on a PersistentVolume: fence marks, the idempotency map, and
# bindings survive restarts. Without it the provider is in-memory only.
state:
  enabled: true
  persistence:
    enabled: true
    size: 1Gi
```

The offerings JSON is delivered through `offerings.content`: set it (or use
`--set-file offerings.content=offerings.json`) and the chart renders it into a
ConfigMap, mounts it at `/etc/bigfleet/offerings/offerings.json`, and passes
`--offerings`. The offerings shape is documented in
[Configuration](/providers/azure/configuration/). Always enable durable `state`
on a PersistentVolume in production — without it the provider is in-memory and
cannot recover bindings on restart.

## Flags

Every flag the binary accepts, grouped by what you touch first. The full
reference (defaults, semantics, the bootstrap model) is in
[Configuration](/providers/azure/configuration/).

**Core**

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address |
| `--provider` | `azure` | Label stamped on `HostRef.provider` (e.g. `azure-eastus`) |
| `--location` | _(empty)_ | Azure region; **required** for the `azure` backend (one process per region) |
| `--azure-backend` | `auto` | `azure` \| `fake` \| `auto` (auto = `azure` when `--location` is set, else `fake`) |
| `--state` | _(empty)_ | Durable state file; empty = in-memory only |

**Azure VM parameters (azure backend)**

| Flag | Default | Meaning |
|---|---|---|
| `--subscription-id` | _(env `AZURE_SUBSCRIPTION_ID`)_ | Azure subscription id |
| `--resource-group` | _(empty)_ | Target resource group for VMs |
| `--subnet-id` | _(empty)_ | VNet/subnet resource id NICs attach to |
| `--image` | `Canonical:ubuntu-24_04-lts:server:latest` | VM image URN or image resource id |
| `--admin-username` | `bigfleet` | VM admin username |
| `--ssh-public-key` | _(empty)_ | Path to an SSH public key authorised for the admin user |
| `--bootstrap-hook` | `/opt/bigfleet/bootstrap` | Image path that applies the delivered bootstrap blob |
| `--base-user-data` | _(empty)_ | File with the generic pre-binding cloud-init baked into customData at create |

**Offerings**

| Flag | Default | Meaning |
|---|---|---|
| `--offerings` | _(empty)_ | JSON offerings file (else a built-in mix sized by `--seed-count`) |
| `--seed-count` | `32` | Speculative slots for the default offerings |
| `--zone-a` / `--zone-b` | `<location>-1` / `<location>-2` | Zones for the default offerings |

**Pricing & background loops**

| Flag | Default | Meaning |
|---|---|---|
| `--price-refresh` | `1h` | On-demand + spot price refresh interval (`0` = off) |
| `--reconcile-interval` | `2m` | Background Azure→inventory reconcile interval (`0` = off) |

**Observability & TLS**

| Flag | Default | Meaning |
|---|---|---|
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
rather than silently serving plaintext.

The chart mounts a standard Kubernetes TLS Secret at `/etc/bigfleet/tls` and
wires `--tls-cert`/`--tls-key` (and `--tls-ca` when `mtls` is set) for you:

```yaml
tls:
  enabled: true
  mtls: true                       # mount ca.crt and require a verified client cert
  secretName: bigfleet-azure-tls   # Secret keys: tls.crt, tls.key, ca.crt
```

The startup log line reports the negotiated mode (`insecure` / `TLS` / `mTLS`).
The full trust model is in [Security](/providers/azure/security/).

## Running on AKS with Workload Identity

On AKS, give the provider its Azure identity with **Workload Identity** (a
federated user-assigned managed identity) rather than a service-principal secret —
`azidentity.DefaultAzureCredential` picks up the projected token automatically,
nothing is hardcoded.

**1. Create the managed identity and role.** Use the Terraform at
[`deploy/terraform`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/azure/deploy/terraform):
it creates a user-assigned managed identity, assigns it a least-privilege role
scoped to your resource group (a custom role by default, or `Contributor`), and
federates it to the chart's ServiceAccount. See
[Credentials](/providers/azure/credentials/) for the exact actions.

```sh
terraform -chdir=providers/azure/deploy/terraform apply \
  -var name=bigfleet-azure-eastus \
  -var location=eastus \
  -var resource_group_name=bigfleet-eastus \
  -var oidc_issuer_url="$(az aks show -g aks-rg -n aks --query oidcIssuerProfile.issuerUrl -o tsv)" \
  -var service_account_namespace=bigfleet \
  -var service_account_name=bigfleet-azure-eastus
# outputs client_id, tenant_id
```

**2. Bind the identity to the chart's ServiceAccount.** Point
`serviceAccount.clientId` / `serviceAccount.tenantId` at the Terraform outputs;
the chart stamps the `azure.workload.identity/client-id` + `tenant-id`
annotations and adds the `azure.workload.identity/use: "true"` pod label the
webhook needs:

```yaml
serviceAccount:
  name: bigfleet-azure-eastus
  clientId: 11111111-1111-1111-1111-111111111111
  tenantId: 22222222-2222-2222-2222-222222222222
```

**3. Install** and watch it come up:

```sh
helm install bigfleet-azure-eastus providers/azure/deploy/helm \
  -n bigfleet -f eastus.values.yaml --set-file offerings.content=offerings.eastus.json

kubectl -n bigfleet logs deploy/bigfleet-azure-eastus | grep 'serving CapacityProvider'
kubectl -n bigfleet port-forward deploy/bigfleet-azure-eastus 9090:9090 &
curl localhost:9090/readyz   # -> ready once gRPC is serving
```

The pod reports `/readyz` green only after the gRPC server is serving, so wire it
to a readiness probe and let BigFleet dial the `Service` once the probe passes.
From here, see [Configuration](/providers/azure/configuration/) for offerings and
the bootstrap model, and
[Pricing & interruption](/providers/azure/pricing-and-interruption/) for the Spot
feed.
