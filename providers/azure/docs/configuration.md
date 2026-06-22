---
title: Configuration
description: Every flag, the offerings JSON schema, the three Azure backend modes, and the create-then-bootstrap (CustomScript extension) model for the BigFleet Azure provider.
sidebar:
  order: 2
  label: Configuration
---

You run one process per Azure region, and you configure it entirely with
command-line flags. You give it three things: a quota of capacity it may
provision for your fleet (the **offerings**), a base image plus the networking to
create into, and the addresses it listens on. Correctness concerns like
retry-safe creates and transition timeouts are handled for you and need no
tuning.

This page is the flag reference, the offerings schema, the backend modes, and the
create-then-bootstrap contract your image must satisfy. For the identity the
flags imply see [Credentials](/providers/azure/credentials/); for how price and
interruption are sourced see
[Pricing & interruption](/providers/azure/pricing-and-interruption/).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address (CapacityProvider + health + reflection). |
| `--provider` | `azure` | Provider/region label stamped on every `HostRef` (e.g. `azure-eastus`). |
| `--location` | _(empty)_ | Azure region. Required for the `azure` backend; also what flips `auto` to `azure`. |
| `--azure-backend` | `auto` | `azure` \| `fake` \| `auto`. `auto` = `azure` when `--location` is set, else `fake`. See [Backend modes](#backend-modes). |
| `--offerings` | _(built-in)_ | Path to a JSON offerings file. Omit to use a built-in mix sized by `--seed-count`. |
| `--seed-count` | `32` | Number of Speculative slots in the default offerings (ignored when `--offerings` is set). |
| `--zone-a` | `<location>-1` | First zone for the default offerings. |
| `--zone-b` | `<location>-2` | Second zone for the default offerings. |
| `--state` | _(empty)_ | Durable state file. Empty = in-memory only (state is lost on restart). |
| `--subscription-id` | _(env `AZURE_SUBSCRIPTION_ID`)_ | Azure subscription id. **Required** for the `azure` backend. |
| `--resource-group` | _(empty)_ | Resource group the provider creates VMs in. **Required** for the `azure` backend. |
| `--subnet-id` | _(empty)_ | VNet/subnet resource id NICs attach to. **Required** for the `azure` backend. |
| `--image` | `Canonical:ubuntu-24_04-lts:server:latest` | VM image URN (`publisher:offer:sku:version`) or a managed-image/gallery resource id. |
| `--admin-username` | `bigfleet` | Admin username set on the VM. |
| `--ssh-public-key` | _(empty)_ | Path to an SSH public key authorised for the admin user (password auth is disabled when set). |
| `--bootstrap-hook` | `/opt/bigfleet/bootstrap` | Image path that consumes the delivered bootstrap blob and joins the cluster. See [the image contract](#the-image-hook-contract). |
| `--base-user-data` | _(empty)_ | Path to the generic, pre-binding cloud-init baked into the VM's customData at create. |
| `--price-refresh` | `1h` | On-demand + spot price refresh interval (never on the List hot path; `0` = off). |
| `--reconcile-interval` | `2m` | Background Azure→inventory reconcile interval (`0` = off). |
| `--eviction-token` | _(empty)_ | Shared bearer token the in-node Scheduled Events agent presents to `POST /internal/eviction`. Prefer the `BIGFLEET_EVICTION_TOKEN` env var (Secret-mounted; the Helm chart wires it from `evictionToken.secretName`) over this flag, which appears in cleartext in the pod spec. **Empty = the endpoint is disabled (fail-closed)** — it mutates interruption state, so it is never exposed unauthenticated. See [Pricing & interruption](/providers/azure/pricing-and-interruption/). |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz`. Empty = disabled. |
| `--reflection` | `true` | Register gRPC server reflection (for `grpcurl`/debugging). |
| `--tls-cert` | _(empty)_ | Server certificate (PEM). With `--tls-key`, enables TLS. |
| `--tls-key` | _(empty)_ | Server private key (PEM). |
| `--tls-ca` | _(empty)_ | Client CA bundle (PEM). Enables mTLS (requires + verifies client certs). |

A minimal production invocation:

```sh
./bin/azure \
  --location eastus --provider azure-eastus \
  --subscription-id 00000000-0000-0000-0000-000000000000 \
  --resource-group bigfleet-eastus \
  --subnet-id /subscriptions/.../subnets/nodes \
  --image Canonical:ubuntu-24_04-lts:server:latest \
  --ssh-public-key /etc/bigfleet/ssh/id.pub \
  --offerings /etc/bigfleet/offerings.json \
  --state /var/lib/bigfleet-azure/state.json \
  --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

:::note
The pinned on-demand price and Spot eviction-band tables ship for `eastus` and
`westeurope`. Running another region logs a startup warning — verify those
tables for your region. See
[Pricing & interruption](/providers/azure/pricing-and-interruption/).
:::

## Backend modes

`--azure-backend` selects the substrate client:

- **`azure`** — the real client backed by `azure-sdk-for-go` (armcompute +
  armnetwork) and `azidentity.DefaultAzureCredential`. Requires `--location`,
  `--subscription-id`, `--resource-group`, and `--subnet-id`; startup fails
  without them. This is what creates real VMs and runs real CustomScript
  extensions.
- **`fake`** — an in-memory simulator. No Azure account, credentials, or network
  needed; no real VMs are created. Used for dev and the credential-free
  conformance run. Selecting it logs a loud warning so it is never mistaken for
  production.
- **`auto`** (default) — resolves to `azure` when `--location` is set, otherwise
  `fake`.

So a bare `./bin/azure --seed-count 32` (no `--location`) comes up on the fake
backend — exactly how `make certify-azure` runs credential-free — while setting
`--location` opts you into the real backend.

## Offerings

An **offering** is one shape of capacity the provider is allowed to provision: a
VM size, in a zone, at a capacity type, up to `count` slots. Each open slot is a
**Speculative** `Machine` the shard can actuate (the cloud analogue of a free
pool). The offerings are the provider's entire quota — it will never create a
size/zone/capacity combination you did not list.

Pass a JSON file with `--offerings`. The file is a JSON array of objects:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `vm_size` | string | yes | Azure VM size, e.g. `Standard_D4s_v5`. |
| `zone` | string | yes | Availability zone, e.g. `eastus-1`. Zoneless offerings are rejected at startup (the provider is multi-zone). |
| `capacity_type` | string | no | `on_demand` (default) \| `spot` \| `reserved`. Empty = `on_demand`. |
| `count` | int | yes | Number of Speculative slots this offering provides. |
| `resources` | map[string]string | no | The per-replica request shape the offering serves (the `Machine.resources`). Distinct from `allocatable`, which is derived from the VM size. |
| `labels` | map[string]string | no | Extra labels carried on the slot. GPU families also get an automatic `bigfleet.io/accelerator` label. |

`capacity_type` accepts a few spellings (`on-demand`/`ondemand`); `bare_metal` is
**rejected** — standalone Azure VMs are always billed, so declaring bare metal
would mis-set `capacity_type` and force `price_per_hour=0`. An unknown value
fails startup.

Example `offerings.json`:

```json
[
  {
    "vm_size": "Standard_D4s_v5",
    "zone": "eastus-1",
    "capacity_type": "on_demand",
    "count": 8,
    "resources": { "cpu": "1", "memory": "2Gi" }
  },
  {
    "vm_size": "Standard_F8s_v2",
    "zone": "eastus-1",
    "capacity_type": "spot",
    "count": 16,
    "resources": { "cpu": "1", "memory": "2Gi" }
  },
  {
    "vm_size": "Standard_NC24ads_A100_v4",
    "zone": "eastus-2",
    "capacity_type": "on_demand",
    "count": 2,
    "resources": { "cpu": "12", "memory": "110Gi", "nvidia.com/gpu": "1" },
    "labels": { "team": "ml" }
  }
]
```

GPU families get an accelerator label automatically (e.g. `Standard_NC*_A100*` →
`bigfleet.io/accelerator=nvidia-a100`). You do not need to set it yourself; the
`Standard_NC24ads_A100_v4` offering above carries both `team` and the accelerator
label.

If you omit `--offerings`, the provider synthesizes a representative mix of
on-demand `Standard_D4s_v5` and **Spot** `Standard_F8s_v2` slots across
`--zone-a`/`--zone-b`, distributing `--seed-count` slots evenly. That default is
for dev and conformance (the Spot bucket exercises the interruption path); **real
deployments supply `--offerings`.**

Shrinking an offering (or removing it) does not delete live VMs: a tagged,
running VM keeps owning its slot, and any tagged VM with no matching offering is
surfaced as Idle under its machine id rather than being lost.

## Allocatable (VM-size capacity)

`resources` (above) is the per-replica *request* shape an offering serves;
`allocatable` is the VM size's *real hardware* capacity (`cpu`, `memory`), which
the engine compares against demand (density = `floor(allocatable / resources)`).
You never set `allocatable` — the provider derives it from the VM size.

It is resolved **authoritatively from Azure**: at startup the provider lists the
Resource SKUs for the offered sizes and caches each size's `vCPUs` and
`MemoryGB`. So any VM size you offer resolves correctly, not just a
hand-maintained subset. Two safety nets keep this robust:

- A **pinned fallback table** of common sizes (D/F/E/NC families) seeds the cache,
  so the fake backend, credential-free conformance, and a Resource-SKUs outage
  all still produce correct `allocatable` for those sizes.
- Memory is rendered as `Gi` when it is a whole number of GiB, else `Mi`, so
  fractional-GiB sizes are exact rather than truncated.

A size that is neither offered-and-resolved nor pinned yields no `allocatable`,
which the engine treats as `allocatable == resources`.

## Create then bootstrap

The provider deliberately splits **create** from **cluster join**, because a VM's
customData (cloud-init) is consumed only at first boot but a slot's target
cluster is only known when the shard binds it. The lifecycle:

1. **Create → NIC + `VirtualMachines.BeginCreateOrUpdate`.** Creates a NIC in the
   configured subnet, then the VM from `--image` with `--base-user-data` as
   customData, the chosen size/zone, and the BigFleet tags (`bigfleet-managed`,
   `bigfleet-machine-id`, `bigfleet-capacity`). The operation id is folded into
   the VM name, so a retried Create maps to the same VM instead of provisioning a
   second one. Spot offerings are created with `priority=Spot`,
   `evictionPolicy=Delete`, and `maxPrice=-1` (pay up to the pay-as-you-go price
   to minimise eviction). The OS managed disk and the NIC are created with
   `DeleteOption=Delete`, so they cascade away with the VM — including on an
   out-of-band Spot eviction, where Azure deletes the VM with no provider involved
   (Azure's default is to *detach* the disk, which would orphan it). The create
   poller runs to completion before the machine settles Idle, so the immediately
   following Configure never races a still-provisioning host.
2. **Configure → power-on + CustomScript extension.** First powers on the host
   (idempotent `BeginStart`) so a recovered stopped/deallocated VM can run the
   extension, then delivers the opaque `bootstrap_blob` via a `CustomScript` VM
   extension that writes and runs the blob through the image hook, then tags the
   VM `bigfleet-cluster=<id>`. The extension poller runs to completion — a failed
   bootstrap becomes `FAILED`, never a false Configured.
3. **Drain → CustomScript extension.** Runs the image hook's drain path
   (cordon/drain the kubelet within the grace period), then clears the
   `bigfleet-cluster` tag. The VM is left running but unbound (Idle). Configure and
   Drain reuse a **single** `CustomScript` extension (re-run via `forceUpdateTag`):
   Azure allows only one extension per handler type per VM, so a second
   differently-named CustomScript extension would be rejected.
4. **Delete → `VirtualMachines.BeginDelete`.** Deletes the VM; its OS disk and
   NIC cascade away via `DeleteOption=Delete` (Delete also best-effort removes the
   NIC explicitly). The slot returns to Speculative.

### The image hook contract

Configure does not bake cluster-join logic into the provider — it delivers an
opaque blob and runs a hook your image ships. The contract:

- The image must ship an executable at `--bootstrap-hook` (default
  `/opt/bigfleet/bootstrap`).
- Configure writes the decoded bootstrap blob to a file and invokes the hook with
  that file as its argument; the hook reads it, joins the cluster, and **exits
  non-zero on any failure** — a non-zero exit is what turns a botched bootstrap
  into a `FAILED` machine instead of a silently-broken node.
- Drain invokes the same hook with `--drain --grace=<seconds>`; it must cordon and
  drain the kubelet (honouring PodDisruptionBudgets up to the grace period) and
  exit non-zero if the drain does not complete.

A minimal hook skeleton:

```sh
#!/usr/bin/env bash
# /opt/bigfleet/bootstrap — invoked as: bootstrap <blob-file>  OR  bootstrap --drain --grace=N
set -euo pipefail
case "${1:-}" in
  --drain)
    grace="${2#--grace=}"
    kubectl cordon "$(hostname)" && kubectl drain "$(hostname)" \
      --ignore-daemonsets --delete-emptydir-data --grace-period="${grace:-0}"
    ;;
  *)
    blob="$1"   # the opaque bootstrap blob; interpret however your join flow needs
    join-the-cluster --config "$blob"
    ;;
esac
```

Use `--base-user-data` for anything that must run at first boot, before any
cluster is chosen — installing the hook's dependencies, pulling images,
configuring the kubelet's static bits. It is generic by design: the same
customData runs on every slot, regardless of which cluster eventually binds it.
