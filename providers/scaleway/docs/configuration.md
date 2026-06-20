---
title: Configuration
description: Every flag, the offerings JSON schema, the backend modes, and the create-then-bootstrap (on-host agent control channel) model for the BigFleet Scaleway provider.
sidebar:
  order: 2
  label: Configuration
---

You run one process per Scaleway zone and one substrate per process, and you
configure it entirely with command-line flags. You give it three things: a quota
of capacity it may provision for your fleet (the **offerings**), a base image plus
the API key to create servers, and the addresses it listens on. Correctness
concerns like retry-safe creates and transition timeouts are handled for you and
need no tuning.

This page is the flag reference, the offerings schema, the backend modes, and the
create-then-bootstrap contract your image must satisfy. For the API key the flags
imply see [Credentials](/providers/scaleway/credentials/); for how price is
sourced see [Pricing](/providers/scaleway/pricing-and-interruption/).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address (CapacityProvider + health + reflection). |
| `--provider` | `scaleway` | Provider/region label stamped on every `HostRef` (e.g. `scaleway-fr-par`). |
| `--substrate` | `instances` | `instances` (ON_DEMAND, deletable) \| `elastic-metal` (BARE_METAL, `Delete` = `Unimplemented`). |
| `--scaleway-backend` | `auto` | `scaleway` \| `fake` \| `auto`. `auto` = `scaleway` when credentials are set, else `fake`. See [Backend modes](#backend-modes). |
| `--access-key` | _(empty)_ | Scaleway access key. Falls back to `SCW_ACCESS_KEY`. |
| `--secret-key` | _(empty)_ | Scaleway secret key. Falls back to `SCW_SECRET_KEY`. |
| `--project-id` | _(empty)_ | Scaleway project id. Falls back to `SCW_DEFAULT_PROJECT_ID`. |
| `--offerings` | _(built-in)_ | Path to a JSON offerings file. Omit to use a built-in mix sized by `--seed-count`. |
| `--seed-count` | `32` | Number of Speculative slots in the default offerings (ignored when `--offerings` is set). |
| `--zone-a` | `fr-par-1` | First zone for the default offerings, and the zone this process serves. |
| `--zone-b` | `nl-ams-1` | Second zone for the default offerings. |
| `--state` | _(empty)_ | Durable state file. Empty = in-memory only (state is lost on restart). |
| `--image` | _(empty)_ | Base image label/id for `CreateServer`. **Required** for the `scaleway` backend. |
| `--base-user-data` | _(empty)_ | Path to the generic, pre-binding cloud-init baked into `user_data` at create (installs the on-host agent). |
| `--bootstrap-addr` | _(empty)_ | Address the provider serves the on-host agent bootstrap channel on (HTTPS, e.g. `:9443`). Empty disables it; **required** for the `scaleway` backend. See [Create then bootstrap](#create-then-bootstrap). |
| `--bootstrap-endpoint` | _(empty)_ | Externally-reachable URL of the bootstrap channel, injected into the server's `user_data` so the agent can dial back (e.g. `https://scaleway-fr-par.bigfleet.svc:9443`). **Required** for the `scaleway` backend. |
| `--bootstrap-tls-cert` | _(empty)_ | Server certificate (PEM) for the bootstrap channel. **Required** for the `scaleway` backend (the blob is a join secret, so the channel is always TLS). |
| `--bootstrap-tls-key` | _(empty)_ | Server private key (PEM) for the bootstrap channel. **Required** for the `scaleway` backend. |
| `--bootstrap-ca` | _(server cert)_ | CA bundle (PEM) the on-host agent pins to verify the provider. Defaults to the server certificate. |
| `--bootstrap-secret` | _(random)_ | HMAC secret minting each agent's per-machine bearer token. Falls back to `BIGFLEET_BOOTSTRAP_SECRET`; if neither is set a random one is generated (with a warning) and tokens won't survive a restart — pin it in production. |
| `--eur-usd` | `1.08` | EUR→USD conversion rate applied to Scaleway's EUR prices. See [Pricing](/providers/scaleway/pricing-and-interruption/). |
| `--price-refresh` | `30m` | Price refresh interval (never on the List hot path). |
| `--reconcile-interval` | `2m` | Background Scaleway→inventory reconcile interval (`0` = off). |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz`. Empty = disabled. |
| `--reflection` | `true` | Register gRPC server reflection (for `grpcurl`/debugging). |
| `--tls-cert` | _(empty)_ | Server certificate (PEM). With `--tls-key`, enables TLS. |
| `--tls-key` | _(empty)_ | Server private key (PEM). |
| `--tls-ca` | _(empty)_ | Client CA bundle (PEM). Enables mTLS (requires + verifies client certs). |

A minimal production invocation:

```sh
./bin/scaleway \
  --provider scaleway-fr-par \
  --substrate instances \
  --image ubuntu_jammy \
  --offerings /etc/bigfleet/offerings.json \
  --bootstrap-addr :9443 \
  --bootstrap-endpoint https://scaleway-fr-par.bigfleet.svc:9443 \
  --bootstrap-tls-cert bootstrap.pem --bootstrap-tls-key bootstrap-key.pem \
  --state /var/lib/bigfleet-scaleway/state.json \
  --eur-usd 1.08 \
  --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
# SCW_ACCESS_KEY / SCW_SECRET_KEY / SCW_DEFAULT_PROJECT_ID and
# BIGFLEET_BOOTSTRAP_SECRET from the environment
```

## Backend modes

`--scaleway-backend` selects the substrate client:

- **`scaleway`** — the real Scaleway client backed by the Scaleway Go SDK.
  Requires complete credentials (access key + secret key + project id) **and**
  `--image`; startup fails without them. This is what creates real servers and
  delivers real bootstrap.
- **`fake`** — an in-memory simulator. No Scaleway account, key, or network
  needed; no real servers are created. Used for dev and the credential-free
  conformance run. Selecting it logs a loud warning so it is never mistaken for
  production.
- **`auto`** (default) — resolves to `scaleway` when complete credentials are set
  (via flags or the `SCW_*` env vars), otherwise `fake`.

So a bare `./bin/scaleway --seed-count 32` (no credentials) comes up on the fake
backend — exactly how `make certify-scaleway` runs credential-free — while setting
credentials opts you into the real backend.

## Offerings

An **offering** is one shape of capacity the provider is allowed to provision: a
commercial type, in a zone, at a capacity type, up to `count` slots. Each open
slot is a **Speculative** `Machine` the shard can actuate (the cloud analogue of a
free pool). The offerings are the provider's entire quota — it will never create a
type/zone combination you did not list.

Pass a JSON file with `--offerings`. The file is a JSON array of objects:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `commercial_type` | string | yes | Scaleway commercial type, e.g. `DEV1-S`, `GP1-XS`, `PRO2-S`, `COPARM1-4C-16G`, `EM-A210R-HDD`. |
| `zone` | string | yes | Scaleway zone, e.g. `fr-par-1`. Zoneless offerings are rejected at startup (the provider is multi-zone). |
| `capacity_type` | string | no | `on_demand` (Instances) or `bare_metal` (Elastic Metal); must match the process's `--substrate`. `spot` and `reserved` are rejected at startup — Scaleway has no spot/preemptible market. |
| `count` | int | yes | Number of Speculative slots this offering provides. |
| `resources` | map[string]string | no | The per-replica request shape the offering serves (the `Machine.resources`). Distinct from `allocatable`, which is derived from the commercial type. |
| `labels` | map[string]string | no | Extra labels carried on the slot. Arm64 (`COPARM1-*`) families get an automatic `kubernetes.io/arch=arm64` label; GPU families (`RENDER-*`, `H100-*`, `L4-*`) get an accelerator label. |

Example `offerings.json`:

```json
[
  {
    "commercial_type": "DEV1-S",
    "zone": "fr-par-1",
    "capacity_type": "on_demand",
    "count": 8,
    "resources": { "cpu": "1", "memory": "2Gi" }
  },
  {
    "commercial_type": "PRO2-S",
    "zone": "nl-ams-1",
    "capacity_type": "on_demand",
    "count": 16,
    "resources": { "cpu": "2", "memory": "4Gi" }
  },
  {
    "commercial_type": "COPARM1-4C-16G",
    "zone": "fr-par-1",
    "capacity_type": "on_demand",
    "count": 4,
    "resources": { "cpu": "2", "memory": "4Gi" },
    "labels": { "team": "arm-builders" }
  }
]
```

The `COPARM1-4C-16G` offering above carries both `team` and the automatic
`kubernetes.io/arch=arm64` label. An Elastic Metal process supplies
`bare_metal` offerings instead (e.g. `EM-A210R-HDD`).

If you omit `--offerings`, the provider synthesizes a representative mix across
`--zone-a`/`--zone-b` — `DEV1-S`/`GP1-XS` for Instances, `EM-A210R-HDD`/
`EM-B112X-SSD` for Elastic Metal — distributing `--seed-count` slots evenly. That
default is for dev and conformance; **real deployments supply `--offerings`.**

Shrinking an offering (or removing it) does not delete live servers: a labelled,
running server keeps owning its slot, and any labelled server with no matching
offering is surfaced as Idle under its machine id rather than being lost.

## Resources vs allocatable

`resources` (above) is the per-replica *request* shape an offering serves;
`allocatable` is the commercial type's *real hardware* capacity (`cpu`, `memory`,
and GPU for accelerator types), which the engine compares against demand
(density = `floor(allocatable / resources)`). You never set `allocatable` — the
provider derives it from the commercial type.

It is resolved **authoritatively from Scaleway**: at startup the provider reads
each offered type's vCPU/memory/GPU from the Scaleway product catalogue and caches
them (specs are immutable, so this runs once). A **pinned fallback table** of
common types seeds the cache, so the fake backend, credential-free conformance,
and a catalogue outage all still produce correct `allocatable`. A type that is
neither offered-and-resolved nor pinned yields no `allocatable`, which the engine
treats as `allocatable == resources`.

:::caution
Never set `resources` to the commercial-type hardware total. `resources` is the
per-replica request (e.g. `{cpu:"1", memory:"2Gi"}`); `allocatable` is the box's
full vCPU/RAM. Setting them equal forces density = 1 and silently breaks the
shard's packing math.
:::

## Create then bootstrap

The provider deliberately splits **create** from **cluster join**, because
Scaleway `user_data` is consumed only at first boot but a slot's target cluster is
only known when the shard binds it. The lifecycle:

1. **Create → `CreateServer`.** Creates the server from `--image` with
   `--base-user-data` as the first-boot `user_data`, in the chosen zone, with the
   BigFleet labels. The base `user_data` installs a small **on-host agent**; the
   provider additionally bakes a generic cloud-config that hands that agent its
   per-machine credentials — the provider's `--bootstrap-endpoint`, the pinned CA
   (`--bootstrap-ca`), the machine id, and a per-machine bearer token. None of
   this is cluster-specific, so it is safe to bake at create time before any
   cluster is chosen. The operation id makes the server name stable, so a retried
   Create maps to the same server instead of creating a second one. **Create
   blocks until the server is actually running** before returning Idle, so the
   immediately following Configure never races a still-initializing host.
2. **Configure → agent control channel.** The provider serves a small HTTPS
   **bootstrap channel** (`--bootstrap-addr`, e.g. `:9443`) with two endpoints —
   `GET /v1/command` and `POST /v1/ack`. At Configure it queues the opaque
   `bootstrap_blob` (as a `configure` command) for this machine. The on-host
   agent **dials the provider** (the provider needs no inbound path to the server,
   so no public IP / SSH is used): it long-polls `GET /v1/command?machine_id=…`
   with `Authorization: Bearer <token>`, receives the command, applies the blob,
   and POSTs the result to `/v1/ack`. Authentication is mutual — the agent
   verifies the provider via the pinned CA (TLS), and the provider authorises each
   agent with a per-machine bearer token =
   `base64(HMAC-SHA256(--bootstrap-secret, machine_id))`, which is re-derivable
   and never stored, so no other machine can read this one's blob. **Configure
   blocks until the agent acks:** a success ack settles the machine to Configured
   and only then sets the cluster-binding tag; a failure ack or the Configure
   transition timeout (ctx cancellation) drives the machine to `FAILED` with
   `last_error`. So we wait for join to **succeed**, and a failed bootstrap
   surfaces as `FAILED`.
3. **Drain → agent.** Sends a `drain` command (with `grace_period_seconds`) over
   the same channel, waits for the agent's ack, then clears the cluster binding —
   leaving the server running but unbound (Idle). `cluster` and `shard_metadata`
   are cleared.
4. **Delete → `DeleteServer`** (Instances only). Deletes the server; the slot
   returns to Speculative (host cleared). On Elastic Metal there is no `Delete`
   (the kit answers `Unimplemented`); a drained physical server returns to the
   free pool.

### The image / agent contract

Your base image must satisfy two things:

- **Ship and start the on-host agent.** The `--base-user-data` cloud-init installs
  and starts the agent at first boot. At create the provider writes the agent's
  per-machine config (its `/etc/bigfleet-agent/config.json`: the provider
  `--bootstrap-endpoint`, the pinned CA, the machine id, and the per-machine
  bearer token), so the agent has everything it needs to reach the channel — but
  no cluster secret.
- **Dial the channel, apply the blob, ack.** On Configure the agent long-polls the
  provider's `GET /v1/command` over TLS (pinning the provided CA, presenting its
  bearer token), applies the opaque blob it receives, and POSTs the result to
  `/v1/ack`. The command carries a `command_id` nonce that the agent **must echo**
  in its ack; the provider ignores an ack whose `command_id` does not match the
  currently-pending command, so a stale ack for a superseded command can never
  complete the wrong transition. The agent must ack a **failure** (or simply not
  ack a success) when the join fails, so a broken join becomes `FAILED` rather
  than a falsely-Idle node. The blob is opaque — the agent consumes it verbatim.

The provider serves this channel only on the real backend, and it **requires**
`--bootstrap-addr`, `--bootstrap-tls-cert`/`--bootstrap-tls-key`, and
`--bootstrap-endpoint`; without them the backend refuses to start. Pin
`--bootstrap-secret` (or `BIGFLEET_BOOTSTRAP_SECRET`) in production so per-machine
tokens survive a provider restart — if it is left to the random default, tokens
minted before a restart stop authenticating and an in-flight Configure ends up
`FAILED`. The secret and the bootstrap TLS cert are each stored as their own
Secret — see [Credentials](/providers/scaleway/credentials/).
