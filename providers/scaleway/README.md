# Scaleway capacity provider

A BigFleet `CapacityProvider` for **Scaleway** (the French/EU cloud). It
implements only the substrate-specific [`providerkit.Backend`](../../providerkit)
(+ `Deleter` for the Instances substrate); providerkit wraps it with the full
`bigfleet.v1alpha1.CapacityProvider` contract — fencing, idempotency, async
dispatch, transition timeouts, the `shard_metadata` lifecycle, the `Machine`
field-shape, and `since_revision`. This provider never re-implements any of that;
it only maps the kit's lifecycle calls onto Scaleway and fills in the substrate
fields (`instance_type`, `zone`, `capacity_type`, `price_per_hour`,
`interruption_probability`, `resources`, `allocatable`, `host`).

> **📖 Operator documentation:** the full operator guide — install & deploy,
> configuration, credentials, pricing, observability, security, troubleshooting,
> and certification — sources live in [`docs/`](docs) (published to the site).
> This README is the quick repo-facing reference.

## Two substrates

One process serves one substrate, selected by `--substrate`:

- `instances` (default) — Scaleway **Instances**, `capacity_type = ON_DEMAND`.
  Real cloud VMs that can be torn down, so this path implements `Delete`
  (poweroff + `DeleteServer`). Claims the **cloud** conformance profile.
- `elastic-metal` — Scaleway **Elastic Metal**, `capacity_type = BARE_METAL`.
  Physical servers that return to a free pool, so `Delete` is
  `codes.Unimplemented`. Claims the **bare-metal** profile. `price_per_hour = 0`
  (owned hardware); commissioning is slow, so transition timeouts are generous.

## Running it

```sh
make build-scaleway
./bin/scaleway --provider scaleway-fr-par \
               --substrate instances \
               --zone-a fr-par-1 \
               --image ubuntu_jammy \
               --agent-token "$BIGFLEET_AGENT_TOKEN" \
               --offerings ./offerings.json \
               --state /var/lib/bigfleet-scaleway/state.json \
               --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

### Backend modes

`--scaleway-backend` selects the substrate client:

- `scaleway` — the real Scaleway client via `scaleway-sdk-go` (requires an API
  key and `--image`).
- `fake` — an in-memory simulator (dev + the credential-free certification run).
- `auto` (default) — `scaleway` when credentials are set (`--access-key`/
  `--secret-key` or `SCW_ACCESS_KEY`/`SCW_SECRET_KEY`), otherwise `fake` (with a
  loud warning).

So a bare `./bin/scaleway --seed-count 32` (no credentials) comes up on the fake
backend — exactly how `make certify-scaleway` runs credential-free.

### Key flags

| flag | meaning |
|---|---|
| `--addr` | gRPC listen address (default `:9000`) |
| `--provider` | label stamped on `HostRef.provider` (e.g. `scaleway-fr-par`) |
| `--substrate` | `instances` \| `elastic-metal` (default `instances`) |
| `--scaleway-backend` | `scaleway` \| `fake` \| `auto` (default `auto`) |
| `--access-key` / `--secret-key` | Scaleway API key (or `SCW_ACCESS_KEY` / `SCW_SECRET_KEY`) |
| `--project-id` | Scaleway project (or `SCW_DEFAULT_PROJECT_ID`) |
| `--image` | base image for `CreateServer` (required for the real backend) |
| `--agent-token` | shared secret authorising the on-host agent's bootstrap fetch |
| `--eur-usd` | EUR→USD rate applied to Scaleway prices (default `1.08`) |
| `--offerings` / `--seed-count` | offerings JSON file (or a default mix sized by seed-count) |
| `--zone-a` / `--zone-b` | zones for the default offerings (`fr-par-1`/`nl-ams-1`) |
| `--state` | durable state file; empty = in-memory only |
| `--tls-cert` / `--tls-key` / `--tls-ca` | TLS / mTLS |

The full flag reference and the offerings schema are in
[`docs/configuration.md`](docs/configuration.md).

## Authentication

Scaleway auth is **API-key based**: an IAM-application **access key + secret key**
plus a default **project id**, read from `--access-key`/`--secret-key`/
`--project-id` or the SDK's `SCW_ACCESS_KEY` / `SCW_SECRET_KEY` /
`SCW_DEFAULT_PROJECT_ID` env vars. The least-privilege grant (an IAM application +
policy + API key) ships as Terraform in [`deploy/iam/`](deploy/iam); store the key
as a Kubernetes Secret in production. See [`docs/credentials.md`](docs/credentials.md).

## Configure-bootstrap reconciliation (design choice)

Scaleway cloud-init `user_data` is consumed **only at first boot**, but a slot's
target cluster is only known when the shard binds it. So the provider splits
launch from cluster-join:

- **Create** (`CreateServer` + poweron, wait for `running`) launches the server
  from `--image` with the generic, cluster-agnostic `--base-user-data` (which
  installs a small on-host agent), and blocks until the server is running before
  settling the machine to Idle.
- **Configure** delivers the opaque per-cluster `bootstrap_blob` over a
  **mutually-authenticated TLS** channel: the on-host agent fetches its OWN
  machine-specific blob (authorised by a per-machine token derived from
  `--agent-token` + the server id) and applies it. This is the HTTP/agent
  analogue of the Hetzner provider's SSH host-key-pinned delivery, and delivers
  the blob exactly once when the binding is established.
- **Drain** cordons/drains the kubelet via the agent (honouring the grace period)
  and clears the cluster binding back to Idle.
- **Delete** (Instances only) powers off + tears the server down; the slot
  returns to Speculative. Elastic Metal returns `codes.Unimplemented`.

This keeps the kit's invariant that an Idle machine already carries a real,
reachable host.

## Pricing & interruption

`price_per_hour` is Scaleway's published **hourly** rate converted from **EUR to
USD** with `--eur-usd`, cached and refreshed off the hot path (a pinned EUR table
is the offline fallback); Elastic Metal is `0` (owned hardware).
`interruption_probability` is a **genuine `0.0`**: Scaleway has no spot/
preemptible market, so the provider does not claim the `spot` conformance
profile, and a `spot` offering is rejected at startup. See
[`docs/pricing-and-interruption.md`](docs/pricing-and-interruption.md).

## Certification

```sh
make certify-scaleway                              # upstream baseline + extension (credential-free, fake backend)
make report-scaleway PROFILE=core,cloud,fault,durable,scale   # full runner -> JUnit + JSON, VERDICT: CERTIFIED
make test-scaleway                                 # unit tests (race)
```

Add `bare-metal` to the profile set when certifying an Elastic Metal deployment.
Everything cross-cutting — fencing, idempotency, async dispatch, transition
timeouts, the `shard_metadata` lifecycle, field-shape — is handled by
`providerkit`. **This provider does not re-implement any of it.**
