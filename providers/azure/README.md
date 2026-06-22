# Azure capacity provider

A BigFleet `CapacityProvider` for **Microsoft Azure**. It implements only the
substrate-specific [`providerkit.Backend`](../../providerkit) (+ `Deleter`);
providerkit wraps it with the full `bigfleet.v1alpha1.CapacityProvider`
contract — fencing, idempotency, async dispatch, transition timeouts, the
`shard_metadata` lifecycle, the `Machine` field-shape, and `since_revision`.
This provider never re-implements any of that; it only maps the kit's lifecycle
calls onto Azure (standalone Virtual Machines, one VM per machine) and fills in
the substrate fields (`instance_type`/VM size, `zone`, `capacity_type`,
`price_per_hour`, `interruption_probability`, `resources`, `allocatable`,
`host`).

> **📖 Operator documentation:** the full operator guide — install & deploy,
> configuration, credentials, pricing, observability, security, troubleshooting,
> and certification — sources live in [`docs/`](docs) (published to the site).
> This README is the quick repo-facing reference.

## Running it

```sh
make build-azure
./bin/azure --provider azure-eastus \
            --location eastus \
            --subscription-id "$AZURE_SUBSCRIPTION_ID" \
            --resource-group bigfleet-eastus \
            --subnet-id /subscriptions/.../subnets/nodes \
            --image Canonical:ubuntu-24_04-lts:server:latest \
            --ssh-public-key /etc/bigfleet/ssh/id.pub \
            --offerings ./offerings.json \
            --state /var/lib/bigfleet-azure/state.json \
            --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

### Backend modes

`--azure-backend` selects the substrate client:

- `azure` — the real client via `azure-sdk-for-go` (armcompute + armnetwork) and
  `azidentity.DefaultAzureCredential` (requires `--location`, `--subscription-id`,
  `--resource-group`, `--subnet-id`).
- `fake` — an in-memory simulator (dev + the credential-free conformance run).
- `auto` (default) — `azure` when `--location` is set, otherwise `fake` (with a
  loud warning).

So a bare `./bin/azure --seed-count 32` (no `--location`) comes up on the fake
backend — exactly how `make certify-azure` runs credential-free.

### Key flags

| flag | meaning |
|---|---|
| `--addr` | gRPC listen address (default `:9000`) |
| `--provider` | label stamped on `HostRef.provider` (e.g. `azure-eastus`) |
| `--azure-backend` | `azure` \| `fake` \| `auto` (default `auto`) |
| `--location` | Azure region (required for the real backend) |
| `--subscription-id` | Azure subscription id (or `AZURE_SUBSCRIPTION_ID`) |
| `--resource-group` | target resource group for VMs |
| `--subnet-id` | VNet/subnet resource id NICs attach to |
| `--image` | VM image URN or image id for Create |
| `--admin-username` / `--ssh-public-key` | admin user + authorised SSH public key |
| `--bootstrap-hook` | image path that applies the delivered bootstrap blob |
| `--offerings` / `--seed-count` | offerings JSON file (or a default mix sized by seed-count) |
| `--zone-a` / `--zone-b` | zones for the default offerings (`<location>-1`/`-2`) |
| `--price-refresh` | On-demand + spot price refresh interval (default `1h`) |
| `--state` | durable state file; empty = in-memory only |
| `--tls-cert` / `--tls-key` / `--tls-ca` | TLS / mTLS |

The full flag reference and the offerings schema are in
[`docs/configuration.md`](docs/configuration.md).

## Authentication

The provider authenticates with `azidentity.NewDefaultAzureCredential`: on AKS a
**user-assigned managed identity** federated to the chart ServiceAccount via
**Workload Identity** (the production path), an env-var service principal on
other hosts, or `az login` locally. The least-privilege role is scoped to the
target **resource group** — a custom role granting only the compute/network
actions the code calls, or `Contributor`. Cluster-join secrets are never in
provider config; they arrive per-Configure in the opaque `bootstrap_blob`. See
[`docs/credentials.md`](docs/credentials.md) and the Terraform in
[`deploy/terraform`](deploy/terraform).

## Create-bootstrap reconciliation (design choice)

Azure customData (cloud-init) is consumed by cloud-init **only at first boot**,
but a slot's target cluster is only known when the shard binds it. So the
provider splits create from cluster-join:

- **Create** creates a NIC then the VM from `--image` with the generic,
  cluster-agnostic `--base-user-data` as customData (Spot offerings set
  `priority=Spot`, `evictionPolicy=Delete`, `maxPrice=-1`), and the create poller
  runs to completion before settling the machine Idle.
- **Configure** delivers the opaque per-cluster `bootstrap_blob` via a
  **CustomScript VM extension** that writes the blob and runs `<bootstrap-hook>`,
  and tags `bigfleet-cluster`.
- **Drain** runs the hook's cordon/drain path (honouring the grace period) and
  clears the cluster tag back to Idle.
- **Delete** (`VirtualMachines.BeginDelete`) tears the VM + NIC down; the slot
  returns to Speculative.

This keeps the kit's invariant that an Idle machine already carries a real,
reachable host.

## Pricing & interruption

`price_per_hour` is sourced **live** from the public Azure Retail Prices API —
pay-as-you-go for `on_demand`/`reserved` and the current **Spot** price for
`spot`, both cached and refreshed off the hot path (the pinned, region-keyed
table is only a startup seed/fallback). SPOT
`interruption_probability` is a **real, non-zero** value: Azure's per-(size,
region) eviction-rate band converted to an hourly probability via
`p = 1 - (1 - m)^(1/720)`, raised toward `1.0` by an observed Scheduled Events
`Preempt` notice. An unknown SPOT size falls back to the non-zero middle band, so
spot is never `0`. See [`docs/pricing-and-interruption.md`](docs/pricing-and-interruption.md).

## Certification

```sh
make certify-azure                              # upstream baseline + extension (credential-free, fake backend)
make report-azure PROFILE=core,cloud,spot,fault,durable,scale   # full runner -> JUnit + JSON, VERDICT: CERTIFIED
make test-azure                                 # unit tests (race)
```

Profiles claimed: **core, cloud, spot** (plus fault/durable/scale by
construction from `providerkit`). Everything cross-cutting — fencing,
idempotency, async dispatch, transition timeouts, the `shard_metadata`
lifecycle, field-shape — is handled by `providerkit`. **This provider does not
re-implement any of it.**
