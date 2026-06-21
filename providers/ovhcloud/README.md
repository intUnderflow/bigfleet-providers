# OVHcloud Public Cloud capacity provider

A BigFleet `CapacityProvider` for **OVHcloud Public Cloud** (OpenStack-based
instances). It implements only the substrate-specific
[`providerkit.Backend`](../../providerkit) (+ `Deleter`); providerkit wraps it
with the full `bigfleet.v1alpha1.CapacityProvider` contract — fencing,
idempotency, async dispatch, transition timeouts, the `shard_metadata` lifecycle,
the `Machine` field-shape, and `since_revision`. This provider never
re-implements any of that; it only maps the kit's lifecycle calls onto OVH Public
Cloud (Nova, via gophercloud) and fills in the substrate fields (`instance_type`,
`zone`, `capacity_type`, `price_per_hour`, `interruption_probability`,
`resources`, `allocatable`, `host`).

> **📖 Operator documentation:** the full operator guide — install & deploy,
> configuration, credentials, pricing, observability, security, troubleshooting,
> and certification — sources live in [`docs/`](docs) (published to the site).
> This README is the quick repo-facing reference.

## Running it

```sh
make build-ovhcloud
./bin/ovhcloud --provider ovh-public-GRA \
               --region GRA \
               --image <BASE_IMAGE_UUID> \
               --key-name bigfleet-ovh \
               --ssh-key /etc/bigfleet/ssh/id_ed25519 \
               --offerings ./offerings.json \
               --state /var/lib/bigfleet-ovhcloud/state.json \
               --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

The OpenStack credentials come from the standard `OS_*` environment
(`OS_AUTH_URL`, `OS_USERNAME`, `OS_PASSWORD`, `OS_PROJECT_ID`,
`OS_USER_DOMAIN_NAME`, `OS_PROJECT_DOMAIN_NAME`, `OS_IDENTITY_API_VERSION=3`),
not flags — so they arrive from a mounted Secret.

### Backend modes

`--ovh-backend` selects the substrate client:

- `ovh` — the real OVH Public Cloud client via `gophercloud/v2` (requires
  `--region`, `--image`, and OS_* credentials).
- `fake` — an in-memory simulator (dev + the credential-free conformance run).
- `auto` (default) — `ovh` when `--region` is set, otherwise `fake` (with a loud
  warning).

So a bare `./bin/ovhcloud --seed-count 32` (no region) comes up on the fake
backend — exactly how `make conformance-ovhcloud` runs credential-free.

### Key flags

| flag | meaning |
|---|---|
| `--addr` | gRPC listen address (default `:9000`) |
| `--provider` | label stamped on `HostRef.provider` (e.g. `ovh-public-GRA`) |
| `--ovh-backend` | `ovh` \| `fake` \| `auto` (default `auto`) |
| `--region` | OVH/OpenStack region (required for the real backend, e.g. `GRA`) |
| `--image` | base image **id (UUID)** for server create (required for the real backend) |
| `--key-name` | OpenStack keypair injected for SSH access |
| `--network` | network name or UUID to attach (default `Ext-Net`) |
| `--ssh-key` / `--ssh-user` | SSH key + user for Configure/Drain delivery |
| `--bootstrap-hook` | image path that applies the delivered bootstrap blob |
| `--eur-usd` | EUR→USD rate applied to OVH prices (default `1.08`) |
| `--offerings` / `--seed-count` | offerings JSON file (or a default mix sized by seed-count) |
| `--region-a` / `--region-b` | regions for the default offerings (`GRA`/`SBG`) |
| `--state` | durable state file; empty = in-memory only |
| `--tls-cert` / `--tls-key` / `--tls-ca` | TLS / mTLS |

The full flag reference and the offerings schema are in
[`docs/configuration.md`](docs/configuration.md).

## Authentication

OVH Public Cloud is **OpenStack**, so auth is a **project-scoped Keystone v3
user** with the project `member` role — the OVH analogue of cloud IAM (no
AWS-style role graph). The provider creates and deletes instances and filters
everything to instances carrying its own `bigfleet-managed` metadata, so it never
touches anything it did not create. Configure/Drain reach the instance over
**SSH** (OpenStack `user_data` cannot re-bootstrap a running instance), so the
provider also needs an SSH private key (`--ssh-key`) whose public key the
OpenStack keypair (`--key-name`) injects at create. See
[`docs/credentials.md`](docs/credentials.md).

## Configure-bootstrap reconciliation (design choice)

OpenStack `user_data` is consumed by cloud-init **only at first boot**, but a
slot's target cluster is only known when the shard binds it. So the provider
splits launch from cluster-join:

- **Create** (`servers.Create`) boots the instance from `--image` with the
  generic, cluster-agnostic `--base-user-data` (plus an injected SSH host key),
  and blocks until the instance is `ACTIVE` before settling the machine to Idle.
- **Configure** delivers the opaque per-cluster `bootstrap_blob` **over SSH**: it
  writes the blob to `<bootstrap-hook>.blob` and runs `sudo <bootstrap-hook>
  <cluster-id>`, waiting for success, then records the cluster binding in
  metadata. This is OVH's analogue of AWS SSM, and it delivers the secret-bearing
  blob exactly once when the binding is established, over a host-key-verified
  channel.
- **Drain** cordons/drains the kubelet over SSH (honouring the grace period) and
  clears the cluster binding back to Idle.
- **Delete** (`servers.Delete`) tears the instance down; the slot returns to
  Speculative.

This keeps the kit's invariant that an Idle machine already carries a real,
reachable host. The alternative (defer the real create to Configure) was rejected
because it breaks that invariant.

## Pricing & interruption

`price_per_hour` is OVH's published **hourly** on-demand rate from a pinned,
version-controlled **EUR table** (OVH exposes no reliable price API for v1)
converted to **USD** with `--eur-usd`, read in memory off the hot path.
`interruption_probability` is a **genuine `0.0`**: OVH Public Cloud is on-demand
only, with no spot market, so the provider declares `ON_DEMAND` for every machine
and does not claim the `spot` conformance profile. A `spot` offering is rejected
at startup. See [`docs/pricing-and-interruption.md`](docs/pricing-and-interruption.md).

## Certification

```sh
make certify-ovhcloud                                       # upstream baseline + extension (credential-free, fake backend)
make report-ovhcloud PROFILE=core,cloud,fault,durable,scale # full runner -> JUnit + JSON, VERDICT: CERTIFIED
make test-ovhcloud                                          # unit tests (race)
```

The OVHcloud provider claims the **core** and **cloud** profiles (it implements
`Delete`); it does **not** claim `spot` (no OVH spot market). The fault, durable,
and scale lanes pass by construction on `providerkit`. A full run reports
**92/92 behaviors** — **VERDICT: CERTIFIED**.

Everything cross-cutting — fencing, idempotency, async dispatch, transition
timeouts, the `shard_metadata` lifecycle, field-shape — is handled by
`providerkit`. **This provider does not re-implement any of it.**
