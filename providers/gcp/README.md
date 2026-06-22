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
| `--ssh-key` / `--ssh-user` | SSH key + user for in-band Configure/Drain delivery |
| `--bootstrap-hook` | image path that applies the delivered bootstrap blob |
| `--use-external-ip` | reach instances over an external IP for SSH (default: internal) |
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
run as. Configure/Drain additionally reach the host **over SSH** (`--ssh-key`,
authorised via `ssh-keys` metadata with host-key pinning), so the provider also
holds a dedicated SSH private key. See [`docs/credentials.md`](docs/credentials.md)
and the Terraform in [`deploy/sa/`](deploy/sa).

## Configure-bootstrap reconciliation (design choice)

A slot's target cluster is only known when the shard binds it, so the provider
splits launch from cluster-join and delivers the bootstrap **in-band over SSH**
to the running host (no reboot, secret never persisted — matching the certified
AWS/Hetzner providers):

- **Create** (`Instances.Insert`) launches the instance from `--image` with the
  generic, cluster-agnostic `--base-startup-script`, authorises the provider's
  `--ssh-key` (via `ssh-keys` metadata) and injects a pinned SSH host key, then
  blocks until the instance is `RUNNING` before settling the machine to Idle. Spot
  offerings set `scheduling.provisioningModel = SPOT`.
- **Configure** SSHes to the running host (verifying the pinned host key), writes
  the opaque per-cluster `bootstrap_blob` to `<bootstrap-hook>.blob` (`umask 077`)
  and runs `<bootstrap-hook> <cluster-id>`, waiting for success. The blob is never
  persisted in metadata; the `bigfleet-cluster` binding is recorded only after the
  hook succeeds. No reboot.
- **Drain** cordons/drains the kubelet over SSH (honouring the grace period), then
  clears the `bigfleet-cluster` binding back to Idle. No reboot.
- **Delete** (`Instances.Delete`) tears the instance down; the slot returns to
  Speculative.

This keeps the kit's invariant that an Idle machine already carries a real,
reachable host, and delivers the blob exactly once when the binding is set.
Inventory and bindings are recoverable from GCE alone — the `bigfleet-managed`
and `bigfleet-capacity` **labels** plus the `bigfleet-machine-id` and
`bigfleet-cluster` instance **metadata** (the ids are too long for label values)
— so `Describe` rebuilds state after a lost cache.

## Pricing & interruption

`price_per_hour` is **refreshed live** from the **Cloud Billing Catalog API**
(GCE on-demand core + memory SKUs, composed per `(machine_type, region)`) on a
cadence (`--price-refresh`, default 45m) into an in-memory table that `List`/`Get`
read **off the hot path**; the pinned, region-keyed table is the startup **seed**
and outage **fallback** only. **Spot** is modelled as a fixed fraction of the
on-demand rate (always non-zero), and the provider **fails closed** on a machine
type with no seed price rather than publishing `0`. `interruption_probability`
is **`0.0` for on-demand/reserved** and a **real, non-zero** per-family forecast
for **Spot** (raised toward `1.0` on an observed preemption) — so the provider
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
