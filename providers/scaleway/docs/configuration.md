---
title: Configuration
description: Every flag, the offerings JSON schema, the backend modes, and the create-then-bootstrap (on-host agent) model for the BigFleet Scaleway provider.
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
| `--agent-token` | _(empty)_ | Shared token the on-host agent presents to fetch its bootstrap blob at Configure. See [Create then bootstrap](#create-then-bootstrap). |
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
  --agent-token "$AGENT_TOKEN" \
  --state /var/lib/bigfleet-scaleway/state.json \
  --eur-usd 1.08 \
  --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
# SCW_ACCESS_KEY / SCW_SECRET_KEY / SCW_DEFAULT_PROJECT_ID from the environment
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
   BigFleet labels. The generic base `user_data` installs a small **on-host
   agent** — it carries no cluster-specific material, so it is safe to bake at
   create time before any cluster is chosen. The operation id makes the server
   name stable, so a retried Create maps to the same server instead of creating a
   second one. **Create blocks until the server is actually running** before
   returning Idle, so the immediately following Configure never races a
   still-initializing host.
2. **Configure → agent fetch.** The provider records the cluster binding on the
   server, then makes the opaque `bootstrap_blob` available to the on-host agent.
   The agent fetches it over a **mutually-authenticated TLS** channel: it presents
   a **per-machine token derived from `--agent-token` + the server id**, the
   provider verifies it before releasing the blob, and the agent verifies the
   provider's TLS certificate — so neither side trusts an impostor. The agent then
   joins the node to the cluster. We wait for join to **succeed**, so a failed
   bootstrap surfaces as `FAILED`.
3. **Drain → agent.** Cordons and drains the kubelet (honouring
   `grace_period_seconds`), then clears the cluster binding — leaving the server
   running but unbound (Idle). `cluster` and `shard_metadata` are cleared.
4. **Delete → `DeleteServer`** (Instances only). Deletes the server; the slot
   returns to Speculative (host cleared). On Elastic Metal there is no `Delete`
   (the kit answers `Unimplemented`); a drained physical server returns to the
   free pool.

### The image / agent contract

Your base image must satisfy two things:

- **Run the generic base `user_data`.** The `--base-user-data` cloud-init installs
  and starts the on-host agent at first boot. The agent carries no cluster secret
  — only the means to authenticate its later fetch.
- **Let the agent join with the delivered blob.** At Configure the agent
  authenticates with its per-machine token (derived from `--agent-token` + the
  server id), fetches the opaque blob, and joins the cluster. The agent must exit
  non-zero on failure (so a broken join becomes `FAILED`, not a falsely-Idle
  node). The blob is opaque — the agent consumes it verbatim.

If you run without `--agent-token`, the agent cannot authenticate its fetch and
the machine ends up `FAILED`. For a real deployment, always set `--agent-token`
(stored as its own Secret — see [Credentials](/providers/scaleway/credentials/)).
