# Latitude.sh capacity provider

A BigFleet `CapacityProvider` for **Latitude.sh** on-demand bare metal. It
implements only the substrate-specific [`providerkit.Backend`](../../providerkit)
(+ `Deleter`); providerkit wraps it with the full
`bigfleet.v1alpha1.CapacityProvider` contract — fencing, idempotency, async
dispatch, transition timeouts, the `shard_metadata` lifecycle, the `Machine`
field-shape, and `since_revision`. This provider never re-implements any of that;
it only maps the kit's lifecycle calls onto Latitude.sh and fills in the
substrate fields (`instance_type`, `zone`, `capacity_type`, `price_per_hour`,
`interruption_probability`, `resources`, `allocatable`, `host`).

> **📖 Operator documentation:** the full operator guide — install & deploy,
> configuration, credentials, pricing, observability, security, troubleshooting,
> and certification — sources live in [`docs/`](docs) (published to the site).
> This README is the quick repo-facing reference.

## Running it

```sh
make build-latitude
./bin/latitude --provider latitude-ash \
               --token "$LATITUDESH_API_TOKEN" \
               --project proj_yourprojectid \
               --operating-system ubuntu_22_04_x64_lts \
               --ssh-key /etc/bigfleet/ssh/id_ed25519 \
               --offerings ./offerings.json \
               --state /var/lib/bigfleet-latitude/state.json \
               --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

### Backend modes

`--latitude-backend` selects the substrate client:

- `latitude` — the real Latitude.sh client via `latitudesh-go-sdk` (requires a
  token **and** a project).
- `fake` — an in-memory simulator (dev + the credential-free conformance run).
- `auto` (default) — `latitude` when **both** a token (`--token` /
  `LATITUDESH_API_TOKEN`) **and** a project (`--project` / `LATITUDESH_PROJECT`)
  are set, otherwise `fake` (with a loud warning).

So a bare `./bin/latitude --seed-count 32` (no creds) comes up on the fake
backend — exactly how `make certify-latitude` runs credential-free.

### Key flags

| flag | meaning |
|---|---|
| `--addr` | gRPC listen address (default `:9000`) |
| `--provider` | label stamped on `HostRef.provider` (e.g. `latitude-ash`) |
| `--latitude-backend` | `latitude` \| `fake` \| `auto` (default `auto`) |
| `--token` | Latitude.sh API token (or `LATITUDESH_API_TOKEN`) |
| `--project` | Latitude project id/slug (or `LATITUDESH_PROJECT`); required for the real backend |
| `--operating-system` | OS slug deployed at create (default `ubuntu_22_04_x64_lts`) |
| `--ssh-key` / `--ssh-user` | SSH key + user for Configure/Drain delivery |
| `--bootstrap-hook` | image path that applies the delivered bootstrap blob |
| `--offerings` / `--seed-count` | offerings JSON file (or a default mix sized by seed-count) |
| `--site-a` / `--site-b` | sites for the default offerings (`ASH`/`NYC`) |
| `--state` | durable state file; empty = in-memory only |
| `--tls-cert` / `--tls-key` / `--tls-ca` | TLS / mTLS |

The full flag reference and the offerings schema are in
[`docs/configuration.md`](docs/configuration.md).

## Authentication

Latitude.sh has **no IAM/role model** — the entire authorisation surface is a
single **project-scoped API token** plus the **project id/slug** every server
operation is scoped to. Pass the token via `--token` or `LATITUDESH_API_TOKEN`
and the project via `--project` or `LATITUDESH_PROJECT`; store the token as a
Kubernetes Secret in production. Configure/Drain reach the server over **SSH**
(Latitude has no in-guest command API), so the provider also needs an SSH private
key (`--ssh-key`) — it injects a generated **host** key via first-boot
`user_data` and authorises this key's public half on every server it deploys. See
[`docs/credentials.md`](docs/credentials.md).

## Configure-bootstrap reconciliation (design choice)

Latitude `user_data` is consumed by cloud-init **only at first boot** (and is
stored as a named Latitude UserData resource), but a slot's target cluster is
only known when the shard binds it. So the provider splits deploy from
cluster-join, and — because the `bootstrap_blob` carries the cluster-**JOIN
SECRET** — never puts that secret in `user_data`:

- **Create** (`POST /servers`) deploys the bare-metal server from
  `--operating-system` with only the generic, cluster-agnostic
  `--base-user-data` plus an injected SSH host key, and blocks until the server
  is powered on before settling the machine to Idle. A bare-metal deploy takes
  **minutes**, so the Create timeout is sized at 30m.
- **Configure** delivers the opaque per-cluster `bootstrap_blob` **over SSH on
  the pinned host key**: it writes the blob to `<bootstrap-hook>.blob` and runs
  `<bootstrap-hook> <cluster-id>`, waiting for success. The host key is verified
  against the fingerprint pinned at deploy — never `InsecureIgnoreHostKey`.
- **Drain** cordons/drains the kubelet over the same SSH channel (honouring the
  grace period) and clears the cluster binding back to Idle.
- **Delete** (`DELETE /servers/{id}`) deprovisions the physical server and tears
  down the per-server UserData resource it created; the slot returns to
  Speculative.

**EnsureRunning:** a tracked server can be powered off out-of-band, so Configure
and Drain both power the server on and wait for reachability **before** delivering
the bootstrap / draining. `Describe` never powers servers on.

## `capacity_type` = `ON_DEMAND` (not `BARE_METAL`)

Latitude is physical hardware, but its lifecycle is on-demand with a **real
Delete** (`DELETE /servers/{id}` deprovisions the box). Since BigFleet **M73**,
the shard's idle-release path only ever emits `Delete` for machines whose
`capacity_type` is `ON_DEMAND` or `SPOT`. Declaring `BARE_METAL` would stop the
shard ever reclaiming a deployed server — leaking physical servers (and money)
forever. So every machine is `ON_DEMAND`, with a genuine, provider-declared
`interruption_probability = 0.0` (Latitude bare metal is not a preemptible
market). The provider claims the `core` + `cloud` conformance profiles, and
rejects a `spot`/`bare_metal` offering at startup.

## Pricing

`price_per_hour` is Latitude's published **hourly USD** rate from the Plans API
(per-site), cached and refreshed off the hot path (a pinned USD table is the
offline fallback). No FX conversion is needed — Latitude prices in USD. See
[`docs/pricing-and-interruption.md`](docs/pricing-and-interruption.md).

## `resources` vs `allocatable`

`allocatable` is the plan's full hardware capacity (looked up from the Plans
catalogue, e.g. a 24-core / 128 GiB box). `resources` is the **per-replica**
request shape the offering serves (operator-declared, e.g. `{cpu:1, memory:2Gi}`).
They are deliberately distinct: density = `floor(allocatable / resources)`, so on
a bare-metal box one server packs many replicas. Setting `resources` =
`allocatable` forces density 1 and wastes the whole box.

## Certification

```sh
make certify-latitude                     # upstream baseline + extension (credential-free, fake backend)
make report-latitude PROFILE=core,cloud   # full runner -> JUnit + JSON, VERDICT: CERTIFIED
make test-latitude                        # unit tests (race)
```

Everything cross-cutting — fencing, idempotency, async dispatch, transition
timeouts, the `shard_metadata` lifecycle, field-shape — is handled by
`providerkit`. **This provider does not re-implement any of it.**
