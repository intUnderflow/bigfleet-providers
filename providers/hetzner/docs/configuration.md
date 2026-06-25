---
title: Configuration
description: Every flag, the offerings JSON schema, the backend modes, and the create-then-bootstrap (SSH) model for the BigFleet Hetzner Cloud provider.
sidebar:
  order: 2
  label: Configuration
---

You run one process per Hetzner location, and you configure it entirely with
command-line flags. You give it three things: a quota of capacity it may
provision for your fleet (the **offerings**), a base image plus the token to
create servers, and the addresses it listens on. Correctness concerns like
retry-safe creates and transition timeouts are handled for you and need no
tuning.

This page is the flag reference, the offerings schema, the backend modes, and
the create-then-bootstrap contract your image must satisfy. For the token the
flags imply see [Credentials](/providers/hetzner/credentials/); for how price is
sourced see [Pricing](/providers/hetzner/pricing-and-interruption/).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address (CapacityProvider + health + reflection). |
| `--provider` | `hetzner` | Provider/location label stamped on every `HostRef` (e.g. `hetzner-nbg1`). |
| `--hetzner-backend` | `auto` | `hetzner` \| `fake` \| `auto`. `auto` = `hetzner` when a token is set, else `fake`. See [Backend modes](#backend-modes). |
| `--token` | _(empty)_ | Hetzner Cloud API token. Falls back to the `HCLOUD_TOKEN` env var. Required for the `hetzner` backend. |
| `--offerings` | _(built-in)_ | Path to a JSON offerings file. Omit to use a built-in mix sized by `--seed-count`. |
| `--seed-count` | `32` | Number of Speculative slots in the default offerings (ignored when `--offerings` is set). |
| `--location-a` | `nbg1` | First location for the default offerings. |
| `--location-b` | `fsn1` | Second location for the default offerings. |
| `--state` | _(empty)_ | Durable state file. Empty = in-memory only (state is lost on restart). |
| `--image` | _(empty)_ | Base image name/id for `Server.Create`. **Required** for the `hetzner` backend. |
| `--ssh-key` | _(empty)_ | SSH private key path for Configure/Drain delivery. Without it, Configure cannot deliver the bootstrap blob. |
| `--ssh-user` | `root` | SSH user for Configure/Drain delivery. |
| `--bootstrap-hook` | `/opt/bigfleet/bootstrap` | Image path that consumes the delivered bootstrap blob and joins the cluster. See [the image contract](#the-image-hook-contract). |
| `--base-user-data` | _(empty)_ | Path to the generic, pre-binding cloud-init baked into user-data at create. |
| `--eur-usd` | `1.08` | EUR→USD conversion rate applied to Hetzner's EUR prices. See [Pricing](/providers/hetzner/pricing-and-interruption/). |
| `--price-refresh` | `30m` | Price refresh interval (never on the List hot path). |
| `--reconcile-interval` | `2m` | Background Hetzner→inventory reconcile interval (`0` = off). |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz`. Empty = disabled. |
| `--reflection` | `true` | Register gRPC server reflection (for `grpcurl`/debugging). |
| `--tls-cert` | _(empty)_ | Server certificate (PEM). With `--tls-key`, enables TLS. |
| `--tls-key` | _(empty)_ | Server private key (PEM). |
| `--tls-ca` | _(empty)_ | Client CA bundle (PEM). Enables mTLS (requires + verifies client certs). |

A minimal production invocation:

```sh
./bin/hetzner \
  --provider hetzner-nbg1 \
  --token "$HCLOUD_TOKEN" \
  --image ubuntu-24.04 \
  --ssh-key /etc/bigfleet/ssh/id_ed25519 \
  --offerings /etc/bigfleet/offerings.json \
  --state /var/lib/bigfleet-hetzner/state.json \
  --eur-usd 1.08 \
  --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

## Backend modes

`--hetzner-backend` selects the substrate client:

- **`hetzner`** — the real Hetzner Cloud client backed by `hcloud-go`. Requires a
  token **and** `--image`; startup fails without them. This is what creates real
  servers and delivers real SSH bootstrap.
- **`fake`** — an in-memory simulator. No Hetzner account, token, or network
  needed; no real servers are created. Used for dev and the credential-free
  conformance run. Selecting it logs a loud warning so it is never mistaken for
  production.
- **`auto`** (default) — resolves to `hetzner` when a token is set (via `--token`
  or `HCLOUD_TOKEN`), else the provider refuses to start unless `--use-fake-backend` is passed.

So a bare `./bin/hetzner` (no token) **refuses to start** — the fake is
testing/conformance only and must be requested with `--use-fake-backend` (which is
how `make conformance-hetzner` runs credential-free). Setting a token selects the
real backend.

## Offerings

An **offering** is one shape of capacity the provider is allowed to provision: a
server type, in a location, up to `count` slots. Each open slot is a
**Speculative** `Machine` the shard can actuate (the cloud analogue of a free
pool). The offerings are the provider's entire quota — it will never create a
type/location combination you did not list.

Pass a JSON file with `--offerings`. The file is a JSON array of objects:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `server_type` | string | yes | Hetzner server type, e.g. `cx22`, `cpx41`, `ccx33`. |
| `location` | string | yes | Hetzner location, e.g. `nbg1`. Locationless offerings are rejected at startup (the provider is multi-location). |
| `capacity_type` | string | no | `on_demand` (default) is the only accepted value. Hetzner Cloud is on-demand only, so `spot`, `reserved`, and `bare_metal` are all rejected at startup (bare metal is the separate Robot substrate, not this provider). |
| `count` | int | yes | Number of Speculative slots this offering provides. |
| `resources` | map[string]string | no | The per-replica request shape the offering serves (the `Machine.resources`). Distinct from `allocatable`, which is derived from the server type. |
| `labels` | map[string]string | no | Extra labels carried on the slot. Arm64 (`cax*`) families also get an automatic `kubernetes.io/arch=arm64` label. |

Example `offerings.json`:

```json
[
  {
    "server_type": "cx22",
    "location": "nbg1",
    "capacity_type": "on_demand",
    "count": 8,
    "resources": { "cpu": "1", "memory": "2Gi" }
  },
  {
    "server_type": "cpx41",
    "location": "fsn1",
    "capacity_type": "on_demand",
    "count": 16,
    "resources": { "cpu": "2", "memory": "4Gi" }
  },
  {
    "server_type": "cax31",
    "location": "hel1",
    "capacity_type": "on_demand",
    "count": 4,
    "resources": { "cpu": "2", "memory": "4Gi" },
    "labels": { "team": "arm-builders" }
  }
]
```

The `cax31` offering above carries both `team` and the automatic
`kubernetes.io/arch=arm64` label.

If you omit `--offerings`, the provider synthesizes a representative mix of
`cx22`/`cpx41` slots across `--location-a`/`--location-b`, distributing
`--seed-count` slots evenly. That default is for dev and conformance; **real
deployments supply `--offerings`.**

Shrinking an offering (or removing it) does not delete live servers: a labelled,
running server keeps owning its slot, and any labelled server with no matching
offering is surfaced as Idle under its machine id rather than being lost.

## Allocatable (server-type capacity)

`resources` (above) is the per-replica *request* shape an offering serves;
`allocatable` is the server type's *real hardware* capacity (`cpu`, `memory`),
which the engine compares against demand (density = `floor(allocatable /
resources)`). You never set `allocatable` — the provider derives it from the
server type.

It is resolved **authoritatively from Hetzner**: at startup the provider reads
each offered type's cores and memory from the Hetzner ServerType API and caches
them. A **pinned fallback table** of common types (cx/cpx/cax/ccx) seeds the
cache, so the fake backend, credential-free conformance, and a ServerType API
outage all still produce correct `allocatable`. A type that is neither
offered-and-resolved nor pinned yields no `allocatable`, which the engine treats
as `allocatable == resources`.

:::caution
Never set `resources` to the server-type hardware total. `resources` is the
per-replica request (e.g. `{cpu:"1", memory:"2Gi"}`); `allocatable` is the box's
full vCPU/RAM (e.g. `cx22` → `{cpu:"2", memory:"4Gi"}`). Setting them equal forces
density = 1 and silently breaks the shard's packing math.
:::

## Create then bootstrap

The provider deliberately splits **create** from **cluster join**, because
Hetzner Cloud user-data is consumed only at first boot but a slot's target
cluster is only known when the shard binds it. The lifecycle:

1. **Create → `Server.Create`.** Creates the server from `--image` with
   `--base-user-data` as cloud-init, in the chosen location, with the BigFleet
   labels (`bigfleet-managed`, `bigfleet-machine-id`). The operation id makes the
   server name stable, so a retried Create maps to the same server instead of
   creating a second one. **Create blocks until the server is actually
   `running`** before returning Idle, so the immediately following Configure
   never races a still-initializing host.
2. **Configure → SSH.** Labels the server `bigfleet-cluster=<id>`, then delivers
   the opaque `bootstrap_blob` to the node over SSH (`--ssh-key`/`--ssh-user`)
   and runs the image's hook at `--bootstrap-hook`. Hetzner Cloud has no in-guest
   command API, so SSH is the delivery channel — the analogue of AWS SSM. We wait
   for the hook to **succeed**, so a failed bootstrap surfaces as `FAILED`.
3. **Drain → SSH.** Cordons and drains the kubelet (`kubectl cordon`/`drain`,
   honouring `grace_period_seconds`), then removes the cluster label — leaving the
   server running but unbound (Idle). `cluster` and `shard_metadata` are cleared.
4. **Delete → `Server.Delete`.** Deletes the server; the slot returns to
   Speculative (host cleared).

### The image hook contract

Your base image must satisfy two things:

- **Authorise `--ssh-key`.** The provider connects as `--ssh-user` (default
  `root`) using the private key you pass; bake the matching public key into the
  image (or via `--base-user-data` cloud-init).
- **Ship the bootstrap hook** at `--bootstrap-hook` (default
  `/opt/bigfleet/bootstrap`). On Configure the provider writes the decoded
  bootstrap blob to `<hook>.blob` and runs `<hook> <cluster-id>`; the hook joins
  the node to the cluster and must exit non-zero on failure (so a broken join
  becomes `FAILED`, not a falsely-Idle node). The blob is opaque — the hook
  consumes it verbatim.

If you run without `--ssh-key`, Configure cannot deliver the blob and the machine
ends up `FAILED`; Drain degrades to clearing the binding label only. For a real
deployment, always set `--ssh-key`.
