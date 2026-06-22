# UpCloud capacity provider

A BigFleet `CapacityProvider` for **UpCloud cloud servers**. It implements only
the substrate-specific [`providerkit.Backend`](../../providerkit) (+ `Deleter`);
providerkit wraps it with the full `bigfleet.v1alpha1.CapacityProvider` contract —
fencing, idempotency, async dispatch, transition timeouts, the `shard_metadata`
lifecycle, the `Machine` field-shape, and `since_revision`. This provider never
re-implements any of that; it only maps the kit's lifecycle calls onto UpCloud and
fills in the substrate fields (`instance_type`, `zone`, `capacity_type`,
`price_per_hour`, `interruption_probability`, `resources`, `allocatable`, `host`).

> **📖 Operator documentation:** the full operator guide — install & deploy,
> configuration, credentials, observability, security, troubleshooting, and
> certification — sources live in [`docs/`](docs) (published to the site). This
> README is the quick repo-facing reference.

## Running it

```sh
make build-upcloud
./bin/upcloud --provider upcloud-fi-hel1 \
              --zone fi-hel1 \
              --template <ubuntu-24.04-template-uuid> \
              --ssh-key /etc/bigfleet/ssh/id_ed25519 \
              --ssh-pubkey "$(cat id_ed25519.pub)" \
              --offerings ./offerings.json \
              --state /var/lib/bigfleet-upcloud/state.json \
              --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

Credentials come from the environment: `UPCLOUD_USERNAME` / `UPCLOUD_PASSWORD` of
a dedicated API sub-account.

### Backend modes

`--upcloud-backend` selects the substrate client:

- `upcloud` — the real UpCloud client via `upcloud-go-api/v8` (requires API
  credentials, `--zone`, and `--template`).
- `fake` — an in-memory simulator (dev + the credential-free conformance run). It
  models a server that can be **stopped out-of-band**, so the EnsureRunning path
  is covered by real tests.
- `auto` (default) — `upcloud` when credentials (`UPCLOUD_USERNAME` /
  `UPCLOUD_PASSWORD`) **and** `--zone` are set, otherwise `fake` (with a loud
  warning).

So a bare `./bin/upcloud --seed-count 32` (no credentials) comes up on the fake
backend — exactly how `make conformance-upcloud` runs credential-free.

### Key flags

| flag | meaning |
|---|---|
| `--addr` | gRPC listen address (default `:9000`) |
| `--provider` | label stamped on `HostRef.provider` (e.g. `upcloud-fi-hel1`) |
| `--upcloud-backend` | `upcloud` \| `fake` \| `auto` (default `auto`) |
| `--username` / `--password` | UpCloud API sub-account (or `UPCLOUD_USERNAME` / `UPCLOUD_PASSWORD`) |
| `--zone` | UpCloud zone served by this process (required for the real backend) |
| `--template` | OS template storage UUID cloned at create (required for the real backend) |
| `--ssh-key` / `--ssh-pubkey` / `--ssh-user` | SSH key, injected public key, and user for Configure/Drain delivery |
| `--bootstrap-hook` | image path that applies the delivered bootstrap blob |
| `--eur-usd` | EUR→USD rate applied to UpCloud prices, live and pinned (default `1.08`) |
| `--price-refresh` | live price refresh interval (default `45m`; `0` = off) |
| `--offerings` / `--seed-count` | offerings JSON file (or a default mix sized by seed-count) |
| `--zone-a` / `--zone-b` | zones for the default offerings (`fi-hel1`/`de-fra1`) |
| `--state` | durable state file; empty = in-memory only |
| `--tls-cert` / `--tls-key` / `--tls-ca` | TLS / mTLS |

The full flag reference and the offerings schema are in
[`docs/configuration.md`](docs/configuration.md).

## Authentication

UpCloud has **no IAM/role model** — the authorisation surface is a dedicated
**API sub-account** (created in the Control Panel's *People* page, scoped to API
access), authenticated with a **username + password over HTTP Basic**. Pass them
via `--username`/`--password` or the `UPCLOUD_USERNAME`/`UPCLOUD_PASSWORD` env
vars; store them as a Kubernetes Secret in production and never log them.
Configure/Drain reach the server over **SSH**, so the provider also needs an SSH
private key (`--ssh-key`) whose public key (`--ssh-pubkey`) is injected into every
server at create. See [`docs/credentials.md`](docs/credentials.md).

## Configure-bootstrap reconciliation (design choice)

UpCloud user-data is consumed by cloud-init **only at first boot** and is the
wrong channel for a per-cluster secret, but a slot's target cluster is only known
when the shard binds it. So the provider splits launch from cluster-join:

- **Create** (`CreateServer`) launches the server in `--zone`, cloning the
  `--template` OS disk, with the generic, cluster-agnostic `--base-user-data`
  (which installs the on-host hook **only**) and a freshly minted SSH **host key**
  whose fingerprint is **pinned** in a server label. It waits until the server is
  `started` before settling the machine to Idle.
- **Configure** first **powers the server on if it was stopped out-of-band**
  (`EnsureRunning`), then delivers the opaque per-cluster `bootstrap_blob` **over
  SSH with the host key VERIFIED against the pinned fingerprint** (a mismatch is a
  possible MITM and fails hard). It writes the blob to `<bootstrap-hook>.blob` and
  runs `<bootstrap-hook> <cluster-id>`, waiting for success.
- **Drain** also `EnsureRunning`s first, then cordons/drains the kubelet over the
  same verified SSH channel (honouring the grace period) and clears the binding
  back to Idle.
- **Delete** stops the server, then deletes the server **and its attached
  storage** (`DeleteServerAndStorages`) — UpCloud storage is a separate billable
  resource, so deleting only the server would leak the disk. Idempotent if already
  gone; the slot returns to Speculative.

This keeps the kit's invariant that an Idle machine already carries a real,
reachable host, and delivers the secret exactly once over an authenticated,
confidential channel.

## Pricing & interruption

`price_per_hour` is UpCloud's published **hourly** plan rate, reported in
**USD**. It is **live-refreshed from the UpCloud `/price` API** off the `List`
hot path (every `--price-refresh`, default `45m`), with a pinned EUR table as the
startup seed and outage fallback — both converted with `--eur-usd` (or an
operator-declared `price_usd_per_hour` per offering, which wins). The provider
**fails closed**: an offered plan with no price (live, pinned, or override) would
advertise free capacity, so the provider refuses to start rather than emit
`0.0`. `interruption_probability` is a **genuine
`0.0`**: UpCloud cloud servers are on-demand only, with no spot market, so the
provider declares `ON_DEMAND` for every machine and does not claim the `spot`
conformance profile. A `spot` offering is rejected at startup.

`allocatable` (the plan's hardware total, from the UpCloud Plans API) is kept
**distinct** from `resources` (the operator-declared per-replica request shape) —
collapsing them would force density = 1 and break the shard's packing math.

## Certification

```sh
make certify-upcloud                     # upstream baseline + extension (credential-free, fake backend)
make report-upcloud PROFILE=core,cloud   # full runner -> JUnit + JSON, VERDICT: CERTIFIED
make test-upcloud                        # unit tests (race)
```

Everything cross-cutting — fencing, idempotency, async dispatch, transition
timeouts, the `shard_metadata` lifecycle, field-shape — is handled by
`providerkit`. **This provider does not re-implement any of it.**
