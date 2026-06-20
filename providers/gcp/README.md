# GCP (GCE) capacity provider

A BigFleet `CapacityProvider` for **Google Compute Engine (GCE)**. It implements
only the substrate-specific [`providerkit.Backend`](../../providerkit) (+
`Deleter`); providerkit wraps it with the full
`bigfleet.v1alpha1.CapacityProvider` contract — fencing, idempotency, async
dispatch, transition timeouts, the `shard_metadata` lifecycle, the `Machine`
field-shape, and `since_revision`. This provider never re-implements any of that;
it only maps the kit's lifecycle calls onto GCE and fills in the substrate fields
(`instance_type`, `zone`, `capacity_type`, `price_per_hour`,
`interruption_probability`, `resources`, `allocatable`, `host`).

> **📖 Operator documentation:** the full operator guide — install & deploy,
> configuration, credentials, pricing, observability, security, troubleshooting,
> and certification — sources live in [`docs/`](docs) (published to the site).
> This README is the quick repo-facing reference.

## Running it

```sh
make build-gcp
./bin/gcp --provider gcp-us-central1 \
          --project my-gcp-project \
          --region us-central1 \
          --image projects/debian-cloud/global/images/family/debian-12 \
          --instance-service-account bigfleet-node@my-gcp-project.iam.gserviceaccount.com \
          --offerings ./offerings.json \
          --state /var/lib/bigfleet-gcp/state.json \
          --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

### Backend modes

`--gcp-backend` selects the substrate client:

- `gcp` — the real GCE client via `cloud.google.com/go/compute` (requires `--project` and `--region`).
- `fake` — an in-memory simulator (dev + the credential-free certification run).
- `auto` (default) — `gcp` when `--region` is set, otherwise `fake` (with a loud warning).

So a bare `./bin/gcp --seed-count 32` (no region) comes up on the fake backend —
exactly how `make certify-gcp` runs credential-free.

### Key flags

| flag | meaning |
|---|---|
| `--addr` | gRPC listen address (default `:9000`) |
| `--provider` | label stamped on `HostRef.provider` (e.g. `gcp-us-central1`) |
| `--gcp-backend` | `gcp` \| `fake` \| `auto` (default `auto`) |
| `--project` / `--region` | GCP project + region (required for the real backend) |
| `--image` | boot disk source image for `Instances.Insert` |
| `--network` / `--subnetwork` | VPC network + subnetwork for the instance NIC |
| `--disk-size-gb` | boot disk size in GiB (default `20`) |
| `--instance-service-account` | identity the launched instances run as |
| `--base-startup-script` | generic pre-binding startup script baked in at Insert |
| `--offerings` / `--seed-count` | offerings JSON file (or a default mix sized by seed-count) |
| `--zone-a` / `--zone-b` | zones for the default offerings (`<region>-a`/`<region>-b`) |
| `--state` | durable state file; empty = in-memory only |
| `--tls-cert` / `--tls-key` / `--tls-ca` | TLS / mTLS |

The full flag reference and the offerings schema are in
[`docs/configuration.md`](docs/configuration.md).

## Authentication

The provider authenticates to GCE via **Application Default Credentials** — no
token flag. There are **two identities**: the **provider** service account (the
process; least-privilege `roles/compute.instanceAdmin.v1`), obtained via
**Workload Identity** on GKE (no key files) or a key-file Secret off-GKE; and the
**instance** service account (`--instance-service-account`) the launched nodes
run as. See [`docs/credentials.md`](docs/credentials.md) and the Terraform in
[`deploy/sa/`](deploy/sa).

## Configure-bootstrap reconciliation (design choice)

A GCE instance consumes its `startup-script` metadata **only at boot**, but a
slot's target cluster is only known when the shard binds it. So the provider
splits launch from cluster-join:

- **Create** (`Instances.Insert`) launches the instance from `--image` with the
  generic, cluster-agnostic `--base-startup-script`, and blocks until the instance
  is `RUNNING` before settling the machine to Idle. Spot offerings set
  `scheduling.provisioningModel = SPOT`.
- **Configure** delivers the opaque per-cluster `bootstrap_blob` by writing it to
  the instance's `startup-script` metadata (`SetMetadata`) and **resetting** the
  instance (`Reset`) so it runs and the node joins; the `bigfleet-cluster` label
  is set only after the blob applied.
- **Drain** strips the `startup-script` metadata (so a future boot won't rejoin)
  and clears the cluster binding back to Idle, honouring the grace period.
- **Delete** (`Instances.Delete`) tears the instance down; the slot returns to
  Speculative.

This keeps the kit's invariant that an Idle machine already carries a real,
reachable host, and delivers the blob exactly once when the binding is set.
Inventory and bindings are recoverable from instance labels alone
(`bigfleet-managed`, `bigfleet-machine-id`, `bigfleet-capacity`,
`bigfleet-cluster`), so `Describe` rebuilds state after a lost cache.

## Pricing & interruption

`price_per_hour` comes from a pinned, region-keyed **USD** table (the v1 model):
on-demand rates per `(machine_type, region)`, with **Spot** modelled as a fixed
fraction of on-demand (always non-zero). `interruption_probability` is **`0.0`
for on-demand/reserved** and a **real, non-zero** per-family forecast for
**Spot** (raised toward `1.0` on an observed preemption) — so the provider
claims the `spot` profile and its default offerings include Spot slots. See
[`docs/pricing-and-interruption.md`](docs/pricing-and-interruption.md).

## Certification

```sh
make certify-gcp                                        # upstream baseline + extension (credential-free, fake backend)
make report-gcp PROFILE=core,cloud,spot,fault,durable,scale  # full runner -> JUnit + JSON, VERDICT: CERTIFIED
make test-gcp                                           # unit tests (race)
```

Everything cross-cutting — fencing, idempotency, async dispatch, transition
timeouts, the `shard_metadata` lifecycle, field-shape — is handled by
`providerkit`. **This provider does not re-implement any of it.**
