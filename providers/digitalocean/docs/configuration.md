---
title: Configuration
description: Every flag, the offerings JSON schema, the backend modes, and the create-then-bootstrap (on-host agent TLS) model for the BigFleet DigitalOcean provider.
sidebar:
  order: 2
  label: Configuration
---

You run one process per DigitalOcean region, and you configure it entirely with
command-line flags. You give it three things: a quota of capacity it may
provision for your fleet (the **offerings**), a base image plus the token and
region to create Droplets, and the addresses it listens on. Correctness concerns
like retry-safe creates and transition timeouts are handled for you and need no
tuning.

This page is the flag reference, the offerings schema, the backend modes,
pricing, and the create-then-bootstrap contract your image must satisfy. For the
token the flags imply see [Credentials](credentials.md).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address (CapacityProvider + health + reflection). |
| `--provider` | `digitalocean` | Provider/region label stamped on every `HostRef` (e.g. `digitalocean-nyc3`). |
| `--do-backend` | `auto` | `digitalocean` \| `fake` \| `auto`. `auto` = `digitalocean` when a token **and** region are set, else `fake`. See [Backend modes](#backend-modes). |
| `--token` | _(empty)_ | DigitalOcean Personal Access Token. Falls back to the `DIGITALOCEAN_TOKEN` env var. Required for the `digitalocean` backend. |
| `--region` | _(empty)_ | DigitalOcean region slug this process serves (e.g. `nyc3`). Required for the `digitalocean` backend. |
| `--offerings` | _(built-in)_ | Path to a JSON offerings file. Omit to use a built-in mix sized by `--seed-count`. |
| `--seed-count` | `32` | Number of Speculative slots in the default offerings (ignored when `--offerings` is set). |
| `--region-a` | `nyc3` | First region for the default offerings. |
| `--region-b` | `sfo3` | Second region for the default offerings. |
| `--state` | _(empty)_ | Durable state file. Empty = in-memory only (state is lost on restart). |
| `--image` | _(empty)_ | Base image / snapshot slug or id for `Droplets.Create`. **Required** for the `digitalocean` backend. |
| `--base-user-data` | _(empty)_ | Path to the generic, pre-binding cloud-init baked into `user_data` at create (installs the on-host agent). |
| `--bootstrap-addr` | _(empty)_ | Address to serve the on-host agent bootstrap channel (HTTPS). **Required** for the `digitalocean` backend. |
| `--bootstrap-endpoint` | _(empty)_ | Externally-reachable URL of the bootstrap channel, injected into Droplet `user_data`. **Required**. |
| `--bootstrap-tls-cert` | _(empty)_ | Server certificate (PEM) for the bootstrap channel. **Required**. |
| `--bootstrap-tls-key` | _(empty)_ | Server private key (PEM) for the bootstrap channel. **Required**. |
| `--bootstrap-ca` | _(server cert)_ | CA bundle (PEM) the on-host agent pins to verify the provider (default: the server cert). |
| `--bootstrap-secret` | _(random)_ | HMAC secret minting per-machine agent tokens (or set `BIGFLEET_BOOTSTRAP_SECRET`; random if unset — pin it in production). |
| `--price-refresh` | `30m` | Price refresh interval (never on the List hot path). |
| `--reconcile-interval` | `2m` | Background DigitalOcean→inventory reconcile interval (`0` = off). |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz`. Empty = disabled. |
| `--reflection` | `true` | Register gRPC server reflection (for `grpcurl`/debugging). |
| `--tls-cert` | _(empty)_ | gRPC server certificate (PEM). With `--tls-key`, enables TLS. |
| `--tls-key` | _(empty)_ | gRPC server private key (PEM). |
| `--tls-ca` | _(empty)_ | gRPC client CA bundle (PEM). Enables mTLS (requires + verifies client certs). |

A minimal production invocation:

```sh
./bin/digitalocean \
  --provider digitalocean-nyc3 \
  --region nyc3 \
  --token "$DIGITALOCEAN_TOKEN" \
  --image ubuntu-24-04-x64 \
  --base-user-data /etc/bigfleet/agent-init.yaml \
  --offerings /etc/bigfleet/offerings.json \
  --state /var/lib/bigfleet-digitalocean/state.json \
  --bootstrap-addr :9443 \
  --bootstrap-endpoint https://do-provider.example:9443 \
  --bootstrap-tls-cert boot.pem --bootstrap-tls-key boot-key.pem \
  --bootstrap-secret "$BIGFLEET_BOOTSTRAP_SECRET" \
  --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

## Backend modes

`--do-backend` selects the substrate client:

- **`digitalocean`** — the real DigitalOcean client backed by `godo`. Requires a
  token, `--region`, `--image`, and the bootstrap channel flags
  (`--bootstrap-addr`/`--bootstrap-tls-cert`/`--bootstrap-tls-key`); startup
  fails without them. This is what creates real Droplets and delivers real
  bootstrap over the agent channel.
- **`fake`** — an in-memory simulator. No DigitalOcean account, token, or network
  needed; no real Droplets are created. Used for dev and the credential-free
  conformance / certification run. Selecting it logs a loud warning so it is
  never mistaken for production.
- **`auto`** (default) — resolves to `digitalocean` when **both** a token (via
  `--token` or `DIGITALOCEAN_TOKEN`) **and** a `--region` are set, otherwise
  `fake`.

So a bare `./bin/digitalocean --seed-count 32` (no token, no region) comes up on
the fake backend — exactly how `make certify-digitalocean` runs credential-free —
while setting a token and region opts you into the real backend.

## Offerings

An **offering** is one shape of capacity the provider is allowed to provision: a
Droplet size, in a region, up to `count` slots. Each open slot is a
**Speculative** `Machine` the shard can actuate (the cloud analogue of a free
pool). The offerings are the provider's entire quota — it will never create a
size/region combination you did not list.

Pass a JSON file with `--offerings`. The file is a JSON array of objects:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `size` | string | yes | DigitalOcean size slug, e.g. `s-2vcpu-4gb`, `s-4vcpu-8gb`, `g-4vcpu-16gb`. |
| `region` | string | yes | DigitalOcean region slug, e.g. `nyc3`. Regionless offerings are rejected at startup (the provider is multi-region). |
| `capacity_type` | string | no | `on_demand` (default) is the only accepted value. DigitalOcean Droplets are on-demand only, so `spot`, `reserved`, and `bare_metal` are all rejected at startup. |
| `count` | int | yes | Number of Speculative slots this offering provides. |
| `resources` | map[string]string | no | The per-replica request shape the offering serves (the `Machine.resources`). Distinct from `allocatable`, which is derived from the size. |
| `labels` | map[string]string | no | Extra labels carried on the slot. |

Example `offerings.json`:

```json
[
  {
    "size": "s-2vcpu-4gb",
    "region": "nyc3",
    "capacity_type": "on_demand",
    "count": 8,
    "resources": { "cpu": "1", "memory": "2Gi" }
  },
  {
    "size": "s-4vcpu-8gb",
    "region": "nyc3",
    "capacity_type": "on_demand",
    "count": 16,
    "resources": { "cpu": "2", "memory": "4Gi" }
  },
  {
    "size": "g-4vcpu-16gb",
    "region": "sfo3",
    "capacity_type": "on_demand",
    "count": 4,
    "resources": { "cpu": "2", "memory": "4Gi" },
    "labels": { "team": "general-purpose" }
  }
]
```

If you omit `--offerings`, the provider synthesizes a representative mix of
`s-2vcpu-4gb`/`s-4vcpu-8gb` slots across `--region-a`/`--region-b`, distributing
`--seed-count` slots evenly. That default is for dev and conformance; **real
deployments supply `--offerings`.**

Shrinking an offering (or removing it) does not delete live Droplets: a tagged,
running Droplet keeps owning its slot, and any tagged Droplet with no matching
offering is surfaced as Idle under its machine id rather than being lost.

## Pricing & interruption

`price_per_hour` is DigitalOcean's published **hourly** on-demand rate per size,
in **USD**. DigitalOcean prices a size identically across all regions, so price
is keyed by size slug alone (unlike a region-priced cloud).

- **Prices come from a pinned USD table** (`pricing.go`) so `List`/`Get` never
  block on the network. It is refreshed off the hot path: at startup, and every
  `--price-refresh` (default `30m`), the provider reads the live `Sizes.List`
  catalogue and overlays the authoritative hourly price for each offered size.
- **The pinned table is the fallback.** Common `s-*`/`g-*`/`c-*`/`m-*` sizes have
  pinned hourly prices, so the fake backend, credential-free conformance, and a
  Sizes API outage all still produce a sensible `price_per_hour`.

`interruption_probability` is a **genuine `0.0`**. DigitalOcean has **no
spot/preemptible Droplet product** — it does not reclaim a running on-demand
Droplet to satisfy other demand — so the correct, real, provider-declared value
is exactly `0.0` for every machine. This is *not* a forgotten field: a zero on a
spot machine would be a bug, but here the substrate has no spot tier. Because of
that, the provider:

- declares `capacity_type = ON_DEMAND` for every machine,
- sets `interruption_probability = 0.0`,
- and **does not claim the `spot` conformance profile** — the SPOT
  `interruption_probability > 0` behaviors skip-as-pass.

The provider also **rejects** a `spot` `capacity_type` in an offering at startup,
rather than silently mis-declaring a zero interruption probability for capacity
that doesn't exist.

## Allocatable (size capacity)

`resources` (above) is the per-replica *request* shape an offering serves;
`allocatable` is the size's *real hardware* capacity (`cpu`, `memory`), which the
engine compares against demand (density = `floor(allocatable / resources)`). You
never set `allocatable` — the provider derives it from the size.

It is resolved **authoritatively from DigitalOcean**: at startup the provider
reads each offered size's vCPU and memory from the Sizes API and caches them
(specs are immutable, so this runs once). A **pinned fallback table** of common
sizes (`s-*`/`g-*`/`c-*`/`m-*`) seeds the cache, so the fake backend,
credential-free conformance, and a Sizes API outage all still produce correct
`allocatable`. A size that is neither offered-and-resolved nor pinned yields no
`allocatable`, which the engine treats as `allocatable == resources`.

:::caution
Never set `resources` to the size's hardware total. `resources` is the
per-replica request (e.g. `{cpu:"1", memory:"2Gi"}`); `allocatable` is the box's
full vCPU/RAM (e.g. `s-4vcpu-8gb` → `{cpu:"4", memory:"8Gi"}`). Setting them
equal forces density = 1 and silently breaks the shard's packing math.
:::

## Create then bootstrap

The provider deliberately splits **create** from **cluster join**, because a
Droplet's `user_data` is consumed by cloud-init only at first boot and is
read-only afterward, but a slot's target cluster is only known when the shard
binds it. The lifecycle:

1. **Create → `Droplets.Create`.** Creates the Droplet from `--image` with
   `--base-user-data` as the generic pre-binding cloud-init, in `--region`, with
   the BigFleet tags (`bigfleet-managed`, the machine-id tag). The operation id
   makes the Droplet name stable, so a retried Create maps to the same Droplet
   instead of creating a second one. **Create blocks until the Droplet is
   actually `active`** before returning Idle, so the immediately following
   Configure never races a still-initializing host.
2. **Configure → on-host agent TLS channel.** Delivers the opaque
   `bootstrap_blob` — a **join secret** — to the already-running Droplet over the
   on-host agent's mutually-authenticated TLS channel, waits for the agent to
   apply it, and only then records the cluster binding tag. We wait for the agent
   to **succeed**, so a failed bootstrap surfaces as `FAILED`.
3. **Drain → the same channel.** Cordons and drains the kubelet (honouring
   `grace_period_seconds`) via the agent, then removes the cluster binding tag —
   leaving the Droplet running but unbound (Idle).
4. **Delete → `Droplets.Delete`.** Deletes the Droplet; the slot returns to
   Speculative (host cleared).

### The on-host agent bootstrap channel

This is the important design choice, and it differs from how the generic
pre-binding bootstrap is delivered. The two halves:

- **Generic bootstrap → `user_data` (at Create).** The cluster-agnostic agent
  bootstrap is baked into `user_data` from `--base-user-data`. It installs and
  starts the **on-host agent** and writes the agent's config — the provider's
  `--bootstrap-endpoint`, the pinned CA, the machine id, and a per-machine bearer
  token = `HMAC(--bootstrap-secret, machine_id)`. This is fine in `user_data`
  because it carries **no cluster secret** — it only tells the agent where and how
  to fetch one later.
- **Cluster-specific blob → the TLS channel (at Configure).** The per-cluster
  `bootstrap_blob` is a **join secret**. It is *not* put in `user_data` (which is
  immutable after first boot and would expose the secret in Droplet metadata).
  Instead the provider serves it from its bootstrap channel (an HTTPS endpoint on
  `--bootstrap-addr`). The on-host agent long-polls that channel over TLS,
  fetches the blob, applies it, and acks. Drain is delivered over the same
  channel.

The authentication is **mutual**, mirroring the Hetzner provider's SSH
host-key-pinned delivery:

- The **agent verifies the provider** by pinning `--bootstrap-ca` (or the server
  certificate) — an on-path attacker cannot impersonate the provider and feed the
  Droplet a malicious blob.
- The **provider authorises only that Droplet** via the per-machine bearer token.
  The token is `HMAC(secret, machine_id)`, so it is restart-safe (re-derivable,
  never stored) and per-machine (no cross-machine read).

The blob is **opaque** — the provider never parses it; the agent consumes it
verbatim.

Your base image must therefore satisfy two things:

- **Ship the on-host agent.** `--base-user-data` configures and starts it, but
  the agent binary itself must already be on the image (or installed by your
  `--base-user-data`).
- **The Droplets must reach `--bootstrap-endpoint`.** If the agent cannot reach
  the provider's bootstrap channel, Configure times out and the machine ends up
  `FAILED`. See [Troubleshooting](troubleshooting.md).
