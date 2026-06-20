# Hetzner Cloud capacity provider

A BigFleet `CapacityProvider` for **Hetzner Cloud**. It implements only the
substrate-specific [`providerkit.Backend`](../../providerkit) (+ `Deleter`);
providerkit wraps it with the full `bigfleet.v1alpha1.CapacityProvider`
contract — fencing, idempotency, async dispatch, transition timeouts, the
`shard_metadata` lifecycle, the `Machine` field-shape, and `since_revision`.
This provider never re-implements any of that; it only maps the kit's lifecycle
calls onto Hetzner Cloud and fills in the substrate fields (`instance_type`,
`zone`, `capacity_type`, `price_per_hour`, `interruption_probability`,
`resources`, `allocatable`, `host`).

> **📖 Operator documentation:** the full operator guide — install & deploy,
> configuration, credentials, pricing, observability, security, troubleshooting,
> and certification — sources live in [`docs/`](docs) (published to the site).
> This README is the quick repo-facing reference.

## Running it

```sh
make build-hetzner
./bin/hetzner --provider hetzner-nbg1 \
              --token "$HCLOUD_TOKEN" \
              --image ubuntu-24.04 \
              --ssh-key /etc/bigfleet/ssh/id_ed25519 \
              --offerings ./offerings.json \
              --state /var/lib/bigfleet-hetzner/state.json \
              --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

### Backend modes

`--hetzner-backend` selects the substrate client:

- `hetzner` — the real Hetzner Cloud client via `hcloud-go` (requires a token and `--image`).
- `fake` — an in-memory simulator (dev + the credential-free conformance run).
- `auto` (default) — `hetzner` when a token is set (`--token` or `HCLOUD_TOKEN`), otherwise `fake` (with a loud warning).

So a bare `./bin/hetzner --seed-count 32` (no token) comes up on the fake
backend — exactly how `make conformance-hetzner` runs credential-free.

### Key flags

| flag | meaning |
|---|---|
| `--addr` | gRPC listen address (default `:9000`) |
| `--provider` | label stamped on `HostRef.provider` (e.g. `hetzner-nbg1`) |
| `--hetzner-backend` | `hetzner` \| `fake` \| `auto` (default `auto`) |
| `--token` | Hetzner Cloud API token (or `HCLOUD_TOKEN`); Read & Write |
| `--image` | base image for `Server.Create` (required for the real backend) |
| `--ssh-key` / `--ssh-user` | SSH key + user for Configure/Drain delivery |
| `--bootstrap-hook` | image path that applies the delivered bootstrap blob |
| `--eur-usd` | EUR→USD rate applied to Hetzner prices (default `1.08`) |
| `--offerings` / `--seed-count` | offerings JSON file (or a default mix sized by seed-count) |
| `--location-a` / `--location-b` | locations for the default offerings (`nbg1`/`fsn1`) |
| `--state` | durable state file; empty = in-memory only |
| `--tls-cert` / `--tls-key` / `--tls-ca` | TLS / mTLS |

The full flag reference and the offerings schema are in
[`docs/configuration.md`](docs/configuration.md).

## Authentication

Hetzner Cloud has **no IAM/role model** — the entire authorisation surface is a
single **project-scoped, Read & Write API token** (the provider creates and
deletes servers). Pass it via `--token` or the `HCLOUD_TOKEN` env var; store it
as a Kubernetes Secret in production. Configure/Drain reach the server over
**SSH** (Hetzner has no in-guest command API), so the provider also needs an SSH
private key (`--ssh-key`) whose public key the base image authorises. See
[`docs/credentials.md`](docs/credentials.md).

## Configure-bootstrap reconciliation (design choice)

Hetzner Cloud `UserData` is consumed by cloud-init **only at first boot** and is
immutable afterwards, but a slot's target cluster is only known when the shard
binds it. So the provider splits launch from cluster-join:

- **Create** (`Server.Create`) launches the server from `--image` with the
  generic, cluster-agnostic `--base-user-data`, and blocks until the server is
  `running` before settling the machine to Idle.
- **Configure** delivers the opaque per-cluster `bootstrap_blob` **over SSH**:
  it writes the blob to `<bootstrap-hook>.blob` and runs `<bootstrap-hook>
  <cluster-id>`, waiting for success. This is Hetzner's analogue of AWS SSM, and
  it delivers the blob exactly once when the binding is established.
- **Drain** cordons/drains the kubelet over SSH (honouring the grace period) and
  clears the cluster binding back to Idle.
- **Delete** (`Server.Delete`) tears the server down; the slot returns to
  Speculative.

This keeps the kit's invariant that an Idle machine already carries a real,
reachable host. The alternative (defer the real create to Configure) was
rejected because it breaks that invariant.

## Pricing & interruption

`price_per_hour` is Hetzner's published **hourly** rate (`ServerType.Pricings`,
location-matched) converted from **EUR to USD** with `--eur-usd`, cached and
refreshed off the hot path (a pinned EUR table is the offline fallback).
`interruption_probability` is a **genuine `0.0`**: Hetzner Cloud is on-demand
only, with no spot market, so the provider declares `ON_DEMAND` for every machine
and does not claim the `spot` conformance profile. A `spot` offering is rejected
at startup. See [`docs/pricing-and-interruption.md`](docs/pricing-and-interruption.md).

## Certification

```sh
make certify-hetzner                     # upstream baseline + extension (credential-free, fake backend)
make report-hetzner PROFILE=core,cloud   # full runner -> JUnit + JSON, VERDICT: CERTIFIED
make test-hetzner                        # unit tests (race)
```

Everything cross-cutting — fencing, idempotency, async dispatch, transition
timeouts, the `shard_metadata` lifecycle, field-shape — is handled by
`providerkit`. **This provider does not re-implement any of it.**
