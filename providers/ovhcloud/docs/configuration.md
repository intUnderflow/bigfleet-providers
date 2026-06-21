---
title: Configuration
description: Every flag, the offerings JSON schema, the backend modes, and the create-then-bootstrap (SSH) model for the BigFleet OVHcloud Public Cloud provider.
sidebar:
  order: 2
  label: Configuration
---

You run one process per OVH region, and you configure it with command-line flags
plus the OS_* OpenStack credentials in the environment. You give it three things:
a quota of capacity it may provision for your fleet (the **offerings**), a base
image plus the OpenStack user to create instances, and the addresses it listens
on. Correctness concerns like retry-safe creates and transition timeouts are
handled for you and need no tuning.

This page is the flag reference, the offerings schema, the backend modes, and the
create-then-bootstrap contract your image must satisfy. For the OpenStack user the
flags imply see [Credentials](/providers/ovhcloud/credentials/); for how price is
sourced see [Pricing](/providers/ovhcloud/pricing-and-interruption/).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address (CapacityProvider + health + reflection). |
| `--provider` | `ovh-public` | Provider/region label stamped on every `HostRef` (e.g. `ovh-public-GRA`). |
| `--ovh-backend` | `auto` | `ovh` \| `fake` \| `auto`. `auto` = `ovh` when `--region` is set, else `fake`. See [Backend modes](#backend-modes). |
| `--region` | _(empty)_ | OVH/OpenStack region (e.g. `GRA`, `SBG`, `BHS`). Required for the `ovh` backend; selects the OpenStack service endpoint. |
| `--offerings` | _(built-in)_ | Path to a JSON offerings file. Omit to use a built-in mix sized by `--seed-count`. |
| `--seed-count` | `32` | Number of Speculative slots in the default offerings (ignored when `--offerings` is set). |
| `--region-a` | `<region>`/`GRA` | First region for the default offerings. |
| `--region-b` | `SBG` | Second region for the default offerings. |
| `--state` | _(empty)_ | Durable state file. Empty = in-memory only (state is lost on restart). |
| `--image` | _(empty)_ | Base image **id (UUID)** for server create. **Required** for the `ovh` backend. |
| `--key-name` | _(empty)_ | OpenStack keypair name injected at create, so the provider can SSH in. |
| `--network` | `Ext-Net` | OpenStack network name or UUID to attach. `Ext-Net` is OVH's public network, so instances get a **public IPv4** by default (used for SSH bootstrap delivery). For hardened deploys, attach a **private network** the provider can reach instead, and front it appropriately — see [Security → network exposure](/providers/ovhcloud/security/#network-exposure). Empty = project default. |
| `--ssh-key` | _(empty)_ | SSH private key path for Configure/Drain delivery. Without it, Configure cannot deliver the bootstrap blob. |
| `--ssh-user` | `ubuntu` | SSH user for Configure/Drain delivery (the base image's default cloud user). |
| `--bootstrap-hook` | `/opt/bigfleet/bootstrap` | Image path that consumes the delivered bootstrap blob and joins the cluster. See [the image contract](#the-image-hook-contract). |
| `--base-user-data` | _(empty)_ | Path to the generic, pre-binding cloud-init baked into user_data at create. |
| `--eur-usd` | `1.08` | EUR→USD conversion rate applied to OVH's EUR prices. See [Pricing](/providers/ovhcloud/pricing-and-interruption/). |
| `--reconcile-interval` | `2m` | Background OpenStack→inventory reconcile interval (`0` = off). |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz`. Empty = disabled. |
| `--reflection` | `true` | Register gRPC server reflection (for `grpcurl`/debugging). |
| `--tls-cert` | _(empty)_ | Server certificate (PEM). With `--tls-key`, enables TLS. |
| `--tls-key` | _(empty)_ | Server private key (PEM). |
| `--tls-ca` | _(empty)_ | Client CA bundle (PEM). Enables mTLS (requires + verifies client certs). |

The OpenStack credentials are **not** flags — they are read from the standard
`OS_*` environment (`OS_AUTH_URL`, `OS_USERNAME`, `OS_PASSWORD`, `OS_PROJECT_ID`,
`OS_USER_DOMAIN_NAME`, `OS_PROJECT_DOMAIN_NAME`, `OS_IDENTITY_API_VERSION=3`), so
they arrive from a mounted Secret rather than a process argument. See
[Credentials](/providers/ovhcloud/credentials/).

A minimal production invocation (OS_* sourced from the environment):

```sh
./bin/ovhcloud \
  --provider ovh-public-GRA \
  --region GRA \
  --image <BASE_IMAGE_UUID> \
  --key-name bigfleet-ovh \
  --ssh-key /etc/bigfleet/ssh/id_ed25519 \
  --offerings /etc/bigfleet/offerings.json \
  --state /var/lib/bigfleet-ovhcloud/state.json \
  --eur-usd 1.08 \
  --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

## Backend modes

`--ovh-backend` selects the substrate client:

- **`ovh`** — the real OVH Public Cloud client backed by `gophercloud/v2`.
  Requires `--region`, `--image`, and OS_* credentials; startup fails without
  them. This is what creates real instances and delivers real SSH bootstrap.
- **`fake`** — an in-memory simulator. No OVH account, credentials, or network
  needed; no real instances are created. Used for dev and the credential-free
  conformance run. Selecting it logs a loud warning so it is never mistaken for
  production.
- **`auto`** (default) — resolves to `ovh` when `--region` is set, otherwise
  `fake`.

So a bare `./bin/ovhcloud --seed-count 32` (no region) comes up on the fake
backend — exactly how `make conformance-ovhcloud` runs credential-free — while
setting `--region` (and OS_* creds) opts you into the real backend.

## Offerings

An **offering** is one shape of capacity the provider is allowed to provision: a
flavor, in a region, up to `count` slots. Each open slot is a **Speculative**
`Machine` the shard can actuate (the cloud analogue of a free pool). The
offerings are the provider's entire quota — it will never create a flavor/region
combination you did not list.

Pass a JSON file with `--offerings`. The file is a JSON array of objects:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `flavor` | string | yes | OVH flavor name, e.g. `b2-7`, `c2-15`, `r2-30`. |
| `region` | string | yes | OVH region, e.g. `GRA`. Regionless offerings are rejected at startup (the provider is multi-region). |
| `capacity_type` | string | no | `on_demand` (default) is the only accepted value. OVH Public Cloud is on-demand only, so `spot`, `reserved`, and `bare_metal` are all rejected at startup (bare metal is the separate Dedicated Servers substrate, not this provider). |
| `count` | int | yes | Number of Speculative slots this offering provides. |
| `resources` | map[string]string | no | The per-replica request shape the offering serves (the `Machine.resources`). Distinct from `allocatable`, which is derived from the flavor. |
| `labels` | map[string]string | no | Extra labels carried on the slot. GPU families (`t1`/`t2`/`a10`/`l4`/`l40s`) also get an automatic `bigfleet.io/accelerator` label. |

Example `offerings.json`:

```json
[
  {
    "flavor": "b2-7",
    "region": "GRA",
    "capacity_type": "on_demand",
    "count": 8,
    "resources": { "cpu": "1", "memory": "2Gi" }
  },
  {
    "flavor": "c2-15",
    "region": "SBG",
    "capacity_type": "on_demand",
    "count": 16,
    "resources": { "cpu": "2", "memory": "4Gi" }
  },
  {
    "flavor": "r2-30",
    "region": "BHS",
    "capacity_type": "on_demand",
    "count": 4,
    "resources": { "cpu": "2", "memory": "8Gi" },
    "labels": { "team": "memory-heavy" }
  }
]
```

If you omit `--offerings`, the provider synthesizes a representative mix of
`b2-7`/`c2-15` slots across `--region-a`/`--region-b`, distributing `--seed-count`
slots evenly. That default is for dev and conformance; **real deployments supply
`--offerings`.**

Shrinking an offering (or removing it) does not delete live instances: a tagged,
running instance keeps owning its slot, and any tagged instance with no matching
offering is surfaced as Idle under its machine id rather than being lost.

## Allocatable (flavor capacity)

`resources` (above) is the per-replica *request* shape an offering serves;
`allocatable` is the flavor's *real hardware* capacity (`cpu`, `memory`), which
the engine compares against demand (density = `floor(allocatable / resources)`).
You never set `allocatable` — the provider derives it from the flavor.

It is resolved **authoritatively from OpenStack**: at startup the provider reads
each offered flavor's vCPUs and RAM from the Nova flavors API and caches them. A
**pinned fallback table** of common OVH flavors (b2/c2/r2/c3/r3/b3/d2/GPU) seeds
the cache, so the fake backend, credential-free conformance, and a flavors-API
outage all still produce correct `allocatable`. A flavor that is neither
offered-and-resolved nor pinned yields no `allocatable`, which the engine treats
as `allocatable == resources`.

:::caution
Never set `resources` to the flavor hardware total. `resources` is the per-replica
request (e.g. `{cpu:"1", memory:"2Gi"}`); `allocatable` is the box's full vCPU/RAM
(e.g. `b2-7` → `{cpu:"2", memory:"7Gi"}`). Setting them equal forces density = 1
and silently breaks the shard's packing math.
:::

## Create then bootstrap

The provider deliberately splits **create** from **cluster join**, because
OpenStack `user_data` is consumed by cloud-init only at first boot but a slot's
target cluster is only known when the shard binds it. The lifecycle:

1. **Create → `servers.Create`.** Boots the instance from `--image` with
   `--base-user-data` as cloud-init (plus an injected SSH host key), on the chosen
   network, with the BigFleet metadata (`bigfleet-managed`, `bigfleet-machine-id`,
   `bigfleet-host-key-fp`). The operation id makes the server name stable, so a
   retried Create maps to the same instance instead of creating a second one.
   **Create blocks until the instance is actually `ACTIVE`** before returning
   Idle, so the immediately following Configure never races a still-building host.
2. **Configure → SSH.** Delivers the opaque `bootstrap_blob` to the node over SSH
   (`--ssh-key`/`--ssh-user`), runs the image's hook at `--bootstrap-hook`, then
   records `bigfleet-cluster=<id>` in metadata. OpenStack `user_data` cannot
   re-bootstrap a running instance, so SSH is the delivery channel — the analogue
   of AWS SSM. We wait for the hook to **succeed**, so a failed bootstrap surfaces
   as `FAILED`.
3. **Drain → SSH.** Cordons and drains the kubelet (`kubectl cordon`/`drain`,
   honouring `grace_period_seconds`), then clears the cluster metadata — leaving
   the instance running but unbound (Idle). `cluster` and `shard_metadata` are
   cleared.
4. **Delete → `servers.Delete`.** Deletes the instance; the slot returns to
   Speculative (host cleared).

### The image hook contract

Your base image must satisfy two things:

- **Authorise the injected keypair.** The provider connects as `--ssh-user`
  (default `ubuntu`) using `--ssh-key`; the matching public key is injected at
  create via the OpenStack keypair named in `--key-name`. (You can also bake the
  public key in via `--base-user-data` cloud-init.)
- **Ship the bootstrap hook** at `--bootstrap-hook` (default
  `/opt/bigfleet/bootstrap`). On Configure the provider writes the decoded
  bootstrap blob to `<hook>.blob` and runs `sudo <hook> <cluster-id>`; the hook
  joins the node to the cluster and must exit non-zero on failure (so a broken
  join becomes `FAILED`, not a falsely-Idle node). The blob is opaque — the hook
  consumes it verbatim. The blob carries the cluster **join secrets**, so the
  provider **removes `<hook>.blob` from the node** as soon as the hook returns
  (via a shell `trap` that fires on any exit, success or failure) — the secret
  never lingers on disk. Your hook should consume the blob synchronously (read it
  during its run), not assume it persists afterward.

> **Reachability for SSH delivery.** Configure/Drain SSH to the instance's
> reachable IPv4 — a floating address if the instance has one, else its fixed
> (private) address. With the default public `Ext-Net` that's a public IP; with a
> private-only network the provider's pod must be able to **route to the fixed
> IP**, or Configure/Drain fail with "no reachable IPv4". See
> [Security → network exposure](/providers/ovhcloud/security/#network-exposure).

If you run without `--ssh-key`, Configure cannot deliver the blob and the machine
ends up `FAILED`; Drain degrades to clearing the binding metadata only. For a real
deployment, always set `--ssh-key` and `--key-name`.
