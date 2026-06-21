# DigitalOcean capacity provider

A BigFleet `CapacityProvider` for **DigitalOcean Droplets**. It implements only
the substrate-specific [`providerkit.Backend`](../../providerkit) (+ `Deleter`);
providerkit wraps it with the full `bigfleet.v1alpha1.CapacityProvider`
contract — fencing, idempotency, async dispatch, transition timeouts, the
`shard_metadata` lifecycle, the `Machine` field-shape, and `since_revision`.
This provider never re-implements any of that; it only maps the kit's lifecycle
calls onto DigitalOcean and fills in the substrate fields (`instance_type`,
`zone`, `capacity_type`, `price_per_hour`, `interruption_probability`,
`resources`, `allocatable`, `host`).

> **📖 Operator documentation:** the full operator guide — install & deploy,
> configuration, credentials, observability, security, troubleshooting, and
> certification — sources live in [`docs/`](docs) (published to the site). This
> README is the quick repo-facing reference.

## Running it

```sh
make build-digitalocean
./bin/digitalocean --provider digitalocean-nyc3 \
                   --region nyc3 \
                   --token "$DIGITALOCEAN_TOKEN" \
                   --image ubuntu-24-04-x64 \
                   --base-user-data /etc/bigfleet/agent-init.yaml \
                   --offerings ./offerings.json \
                   --state /var/lib/bigfleet-digitalocean/state.json \
                   --bootstrap-addr :9443 \
                   --bootstrap-endpoint https://do-provider.example:9443 \
                   --bootstrap-tls-cert boot.pem --bootstrap-tls-key boot-key.pem \
                   --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

One process serves a single DigitalOcean **region** (`--region`, e.g. `nyc3`).
`instance_type` is the DigitalOcean **size slug** (`s-4vcpu-8gb`) and `zone` is
the **region slug** (`nyc3`).

### Backend modes

`--do-backend` selects the substrate client:

- `digitalocean` — the real DigitalOcean client via `godo` (requires a token,
  `--region`, `--image`, and the bootstrap channel flags).
- `fake` — an in-memory simulator (dev + the credential-free conformance run).
- `auto` (default) — `digitalocean` when **both** a token (`--token` or
  `DIGITALOCEAN_TOKEN`) **and** a `--region` are set, otherwise `fake` (with a
  loud warning).

So a bare `./bin/digitalocean --seed-count 32` (no token, no region) comes up on
the fake backend — exactly how `make certify-digitalocean` runs credential-free.

### Key flags

| flag | meaning |
|---|---|
| `--addr` | gRPC listen address (default `:9000`) |
| `--provider` | label stamped on `HostRef.provider` (e.g. `digitalocean-nyc3`) |
| `--do-backend` | `digitalocean` \| `fake` \| `auto` (default `auto`) |
| `--token` | DigitalOcean Personal Access Token (or `DIGITALOCEAN_TOKEN`); read + write on Droplets |
| `--region` | DigitalOcean region slug this process serves (e.g. `nyc3`) |
| `--image` | base image / snapshot slug or id for `Droplets.Create` |
| `--base-user-data` | generic pre-binding cloud-init baked in at create (installs the on-host agent) |
| `--bootstrap-addr` / `--bootstrap-endpoint` | on-host agent TLS channel listen addr + externally-reachable URL |
| `--bootstrap-tls-cert` / `--bootstrap-tls-key` / `--bootstrap-ca` | TLS for the agent channel |
| `--bootstrap-secret` | HMAC secret minting per-machine agent tokens (or `BIGFLEET_BOOTSTRAP_SECRET`) |
| `--offerings` / `--seed-count` | offerings JSON file (or a default mix sized by seed-count) |
| `--region-a` / `--region-b` | regions for the default offerings (`nyc3`/`sfo3`) |
| `--state` | durable state file; empty = in-memory only |
| `--tls-cert` / `--tls-key` / `--tls-ca` | gRPC TLS / mTLS |

The full flag reference and the offerings schema are in
[`docs/configuration.md`](docs/configuration.md).

## Authentication

DigitalOcean has **no IAM/role chain** — the entire authorisation surface is a
single **Personal Access Token (PAT)**. Scope it to the minimum the provider
needs: **read + write on Droplets** (plus the Sizes/Tags catalogue it reads);
no account or billing scope. Pass it via `--token` or the `DIGITALOCEAN_TOKEN`
env var; store it as a Kubernetes Secret in production, never bake it into the
image, and rotate it. There is **one identity** — the PAT is the provider's only
cloud identity, and there is no separate node role. See
[`docs/credentials.md`](docs/credentials.md).

## Configure-bootstrap reconciliation (design choice)

A Droplet's `user_data` is consumed by cloud-init **only at first boot** and is
read-only afterward, but a slot's target cluster is only known when the shard
binds it. So the provider splits launch from cluster-join:

- **Create** (`Droplets.Create`) launches the Droplet from `--image` with the
  generic, cluster-agnostic `--base-user-data` (which installs the **on-host
  agent**), and blocks until the Droplet is `active` before settling the machine
  Idle.
- **Configure** delivers the opaque per-cluster `bootstrap_blob` — a **join
  secret** — to the already-running Droplet over the on-host agent's **TLS
  channel with mutual authentication** (the agent pins the provider's CA; the
  provider authenticates the agent with a per-machine bearer token — not mTLS).
  The blob is *not* delivered via
  `user_data`/Droplet metadata (which is immutable post-create). The provider
  serves the blob from its bootstrap channel; the agent fetches it over TLS,
  pinning the provider's CA, and the provider authorises only that Droplet via a
  per-machine bearer token = HMAC(secret, machine_id) injected into the
  Create-time `user_data`. This is the TLS analogue of the Hetzner provider's
  SSH host-key-pinned delivery.
- **Drain** is delivered over the same channel, then clears the cluster binding
  back to Idle.
- **Delete** (`Droplets.Delete`) tears the Droplet down; the slot returns to
  Speculative.

This keeps the kit's invariant that an Idle machine already carries a real,
reachable host. The blob is opaque — the provider never parses it.

## Pricing & interruption

`price_per_hour` is DigitalOcean's published **hourly** USD rate per size
(DigitalOcean prices a size identically across regions). It is sourced from a
pinned USD table (`pricing.go`), refreshed off the hot path from the live
`Sizes.List` catalogue (`--price-refresh`, default `30m`).
`interruption_probability` is a **genuine `0.0`**: DigitalOcean has **no
spot/preemptible product**, so the provider declares `ON_DEMAND` for every
machine and does **not** claim the `spot` conformance profile. A `spot` offering
is rejected at startup. See [`docs/configuration.md`](docs/configuration.md).

## Certification

```sh
make certify-digitalocean                                        # baseline + extension (credential-free, fake backend)
make report-digitalocean PROFILE=core,cloud,fault,durable,scale  # full runner -> JUnit + JSON, VERDICT: CERTIFIED
make test-digitalocean                                           # unit tests (race)
```

The provider claims the **core** and **cloud** profiles (it implements
`Delete` = `Droplets.Delete`), but **not** `spot` — DigitalOcean has no spot
product. Everything cross-cutting — fencing, idempotency, async dispatch,
transition timeouts, the `shard_metadata` lifecycle, field-shape — is handled by
`providerkit`. **This provider does not re-implement any of it.**
