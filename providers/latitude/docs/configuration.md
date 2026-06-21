---
title: Configuration
description: Every flag, the offerings JSON schema, the backend modes, and the deploy-then-bootstrap (SSH) model for the BigFleet Latitude.sh provider.
sidebar:
  order: 2
  label: Configuration
---

You run one process per Latitude.sh site, and you configure it entirely with
command-line flags. You give it three things: a quota of capacity it may
provision for your fleet (the **offerings**), an OS slug plus the token and
project to deploy servers, and the addresses it listens on. Correctness concerns
like retry-safe deploys and transition timeouts are handled for you and need no
tuning.

This page is the flag reference, the offerings schema, the backend modes, and
the deploy-then-bootstrap contract your image must satisfy. For the token and
project the flags imply see [Credentials](/providers/latitude/credentials/); for
how price is sourced see [Pricing](/providers/latitude/pricing-and-interruption/).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address (CapacityProvider + health + reflection). |
| `--provider` | `latitude` | Provider/site label stamped on every `HostRef` (e.g. `latitude-ash`). |
| `--latitude-backend` | `auto` | `latitude` \| `fake` \| `auto`. `auto` = `latitude` when **both** a token and a project are set, else `fake`. See [Backend modes](#backend-modes). |
| `--token` | _(empty)_ | Latitude.sh API token. Falls back to the `LATITUDESH_API_TOKEN` env var. Required for the `latitude` backend. |
| `--project` | _(empty)_ | Latitude.sh project id or slug. Falls back to `LATITUDESH_PROJECT`. Required for the `latitude` backend (every server op is scoped to it). |
| `--offerings` | _(built-in)_ | Path to a JSON offerings file. Omit to use a built-in mix sized by `--seed-count`. |
| `--seed-count` | `32` | Number of Speculative slots in the default offerings (ignored when `--offerings` is set). |
| `--site-a` | `ASH` | First site for the default offerings. |
| `--site-b` | `NYC` | Second site for the default offerings. |
| `--state` | _(empty)_ | Durable kit state file (fence marks, idempotency, inventory, bindings). Empty = in-memory only (state is lost on restart). |
| `--substrate-state` | _(empty)_ | Durable provider-owned substrate index (machine_id → server / host-key / user-data map). Empty derives a sibling of `--state`, else in-memory. Put it on the same volume as `--state`. |
| `--operating-system` | `ubuntu_22_04_x64_lts` | OS slug deployed at `Server` create (latitude backend). |
| `--ssh-key` | _(empty)_ | SSH private key path for Configure/Drain delivery. Without it, Configure cannot deliver the bootstrap blob. |
| `--ssh-user` | `root` | SSH user for Configure/Drain delivery. |
| `--bootstrap-hook` | `/opt/bigfleet/bootstrap` | OS path that consumes the delivered bootstrap blob and joins the cluster. See [the image contract](#the-image-hook-contract). |
| `--base-user-data` | _(empty)_ | Path to the generic, pre-binding cloud-init baked into user-data at deploy. |
| `--price-refresh` | `30m` | Price refresh interval (never on the List hot path). |
| `--reconcile-interval` | `2m` | Background Latitude→inventory reconcile interval (`0` = off). |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz`. Empty = disabled. |
| `--reflection` | `true` | Register gRPC server reflection (for `grpcurl`/debugging). |
| `--tls-cert` | _(empty)_ | Server certificate (PEM). With `--tls-key`, enables TLS. |
| `--tls-key` | _(empty)_ | Server private key (PEM). |
| `--tls-ca` | _(empty)_ | Client CA bundle (PEM). Enables mTLS (requires + verifies client certs). |

A minimal production invocation:

```sh
./latitude \
  --provider latitude-ash \
  --token "$LATITUDESH_API_TOKEN" \
  --project proj_yourprojectid \
  --operating-system ubuntu_22_04_x64_lts \
  --ssh-key /etc/bigfleet/ssh/id_ed25519 \
  --offerings /etc/bigfleet/offerings.json \
  --state /var/lib/bigfleet-latitude/state.json \
  --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

## Backend modes

`--latitude-backend` selects the substrate client:

- **`latitude`** — the real Latitude.sh client backed by `latitudesh-go-sdk`.
  Requires a token **and** a project; startup fails without either. This is what
  deploys real servers and delivers real SSH bootstrap.
- **`fake`** — an in-memory simulator. No Latitude account, token, project, or
  network needed; no real servers are deployed. Used for dev and the
  credential-free conformance run. Selecting it logs a loud warning so it is
  never mistaken for production.
- **`auto`** (default) — resolves to `latitude` when **both** a token (via
  `--token` or `LATITUDESH_API_TOKEN`) **and** a project (via `--project` or
  `LATITUDESH_PROJECT`) are set, otherwise `fake`.

So a bare `./latitude --seed-count 32` (no token, no project) comes up on the
fake backend — exactly how `make certify-latitude` runs credential-free — while
setting **both** token and project opts you into the real backend.

## Offerings

An **offering** is one shape of capacity the provider is allowed to provision: a
plan, in a site, up to `count` slots. Each open slot is a **Speculative**
`Machine` the shard can actuate (the cloud analogue of a free pool). The
offerings are the provider's entire quota — it will never deploy a plan/site
combination you did not list.

Pass a JSON file with `--offerings`. The file is a JSON array of objects:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `plan` | string | yes | Latitude.sh plan slug, e.g. `c2-small-x86`, `c3-large-x86`, `g3-xlarge-x86`. |
| `site` | string | yes | Latitude.sh site slug, e.g. `ASH`. Siteless offerings are rejected at startup (the provider is multi-site). |
| `capacity_type` | string | no | `on_demand` (default) is the only accepted value. `spot` and `bare_metal` are **rejected** at startup — see [Why on_demand, not bare_metal](#why-on_demand-not-bare_metal). |
| `count` | int | yes | Number of Speculative slots this offering provides. |
| `resources` | map[string]string | no | The per-replica request shape the offering serves (the `Machine.resources`). Distinct from `allocatable`, which is derived from the plan's hardware. |
| `labels` | map[string]string | no | Extra labels carried on the slot. GPU plan families (`*h100*`, `*l40s*`, `*a100*`, `g3*`/`g4*`) also get an automatic `bigfleet.io/accelerator` label. |

Example `offerings.json`:

```json
[
  {
    "plan": "c2-small-x86",
    "site": "ASH",
    "capacity_type": "on_demand",
    "count": 8,
    "resources": { "cpu": "1", "memory": "2Gi" }
  },
  {
    "plan": "c3-large-x86",
    "site": "NYC",
    "capacity_type": "on_demand",
    "count": 16,
    "resources": { "cpu": "2", "memory": "4Gi" }
  },
  {
    "plan": "g3-xlarge-x86",
    "site": "ASH",
    "capacity_type": "on_demand",
    "count": 2,
    "resources": { "cpu": "4", "memory": "16Gi" },
    "labels": { "team": "ml-builders" }
  }
]
```

The `g3-xlarge-x86` offering above carries both `team` and the automatic
`bigfleet.io/accelerator` label.

The `plan` and `site` become the machine's top-level `instance_type` and `zone`,
not labels — so `node.kubernetes.io/instance-type` and
`topology.kubernetes.io/zone` selectors match without any extra wiring.

If you omit `--offerings`, the provider synthesizes a representative mix of
`c2-small-x86`/`c3-large-x86` slots across `--site-a`/`--site-b`, distributing
`--seed-count` slots evenly. That default is for dev and conformance; **real
deployments supply `--offerings`.**

Shrinking an offering (or removing it) does not deprovision live servers: a
managed, running server keeps owning its slot, and any managed server with no
matching offering is surfaced as Idle under its machine id rather than being
lost.

### Why on_demand, not bare_metal

Latitude.sh **is** physical hardware, but the lifecycle is on-demand with a
**real Delete** (`DELETE /servers/{id}` deprovisions the box). The capacity type
is therefore `ON_DEMAND`, not `BARE_METAL`:

- Since BigFleet M73 the shard only emits `Delete` for `ON_DEMAND`/`SPOT`
  capacity. Declaring `BARE_METAL` would stop the shard ever reclaiming a
  deployed server — leaking a paid-for box forever.
- So the provider **rejects** `capacity_type: bare_metal` (and `reserved`) in an
  offering at startup with an explicit error rather than silently suppressing
  `Delete`.
- It also rejects `capacity_type: spot` — Latitude has no spot/preemptible tier,
  and a `spot` declaration would mis-state `interruption_probability`. See
  [Pricing & interruption](/providers/latitude/pricing-and-interruption/).

## Allocatable (plan hardware)

`resources` (above) is the per-replica *request* shape an offering serves;
`allocatable` is the plan's *real hardware* capacity (`cpu`, `memory`), which the
engine compares against demand (density = `floor(allocatable / resources)`). You
never set `allocatable` — the provider derives it from the plan.

It is resolved **authoritatively from Latitude**: at startup the provider reads
each offered plan's vCPU (`cpu.cores × cpu.count`) and memory from the Plans API
and caches them. A **pinned fallback table** of common plans (c/s/m/g series)
seeds the cache, so the fake backend, credential-free conformance, and a Plans
API outage all still produce correct `allocatable`. A plan that is neither
offered-and-resolved nor pinned yields no `allocatable`, which the engine treats
as `allocatable == resources`.

:::caution
Never set `resources` to the plan's hardware total. These are **bare-metal**
boxes, so the whole point is density: a 24-core plan (`c3-large-x86`) with a
per-replica `resources` of `{cpu:"1", memory:"2Gi"}` packs many replicas
(`floor(allocatable / resources) >> 1`). Setting `resources` **equal** to
`allocatable` forces density = 1 and wastes the box — you pay for a 24-core
server and run a single replica on it. Keep `resources` much smaller than the
plan's full hardware.
:::

## Deploy then bootstrap

The provider deliberately splits **deploy** from **cluster join**, because
Latitude.sh user-data is consumed only at first boot (and stored as a Latitude
`UserData` resource) but a slot's target cluster is only known when the shard
binds it. The lifecycle:

1. **Create → `POST /servers`.** Deploys the server on `--operating-system` in the
   chosen site, with the generic `--base-user-data` (plus an injected host key —
   see [Security](/providers/latitude/security/)) as first-boot cloud-init. The
   provider keys identity on the machine id via a small persisted index (and a
   collision-free deploy hostname as a backstop), so a retried Create adopts the
   existing server instead of deploying a second one. **Create blocks until the
   server is actually powered on** before returning Idle, so the immediately
   following Configure never races a still-deploying box. A bare-metal deploy is
   **minutes**, which is why the Create transition timeout is 30m.
2. **Configure → SSH.** Powers the server on if it was stopped out-of-band
   (**EnsureRunning**), then delivers the opaque `bootstrap_blob` to the node over
   SSH (`--ssh-key`/`--ssh-user`) on a **pinned host key** and runs the image's
   hook at `--bootstrap-hook`. Latitude.sh has no in-guest command API, so SSH is
   the delivery channel — the analogue of AWS SSM. We wait for the hook to
   **succeed**, so a failed bootstrap surfaces as `FAILED`.
3. **Drain → SSH.** EnsureRunning, then cordons and drains the kubelet (`kubectl
   cordon`/`drain`, honouring `grace_period_seconds`) and clears the cluster
   binding — leaving the server running but unbound (Idle).
4. **Delete → `DELETE /servers/{id}`.** Deprovisions the server (and the
   per-server `UserData` resource it created); the slot returns to Speculative.

### EnsureRunning

A tagged server the provider tracks as Idle/bound may be powered off
out-of-band. **Configure and Drain both power it on and wait for reachability**
before delivering the bootstrap or draining, so they never act on a stopped
server. `Describe` does **not** power servers on — a tagged-but-stopped server
stays Idle and reapable in the inventory, owning its slot.

### The image hook contract

Your OS image must satisfy two things:

- **Authorise `--ssh-key`.** The provider registers the matching public key with
  Latitude and authorises it on every server it deploys, then connects as
  `--ssh-user` (default `root`) using the private key you pass.
- **Ship the bootstrap hook** at `--bootstrap-hook` (default
  `/opt/bigfleet/bootstrap`). On Configure the provider writes the decoded
  bootstrap blob to `<hook>.blob` and runs `<hook> <cluster-id>`; the hook joins
  the node to the cluster and must exit non-zero on failure (so a broken join
  becomes `FAILED`, not a falsely-Idle node). The blob is opaque — the hook
  consumes it verbatim.

If you run without `--ssh-key`, Configure cannot deliver the blob and the machine
ends up `FAILED`; Drain degrades to clearing the binding only. For a real
deployment, always set `--ssh-key`.
