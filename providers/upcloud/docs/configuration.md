---
title: Configuration
description: Every flag, the offerings JSON schema, the backend modes, live-refreshed EUR→USD pricing, and the create-then-bootstrap (SSH host-key-pinned) model for the BigFleet UpCloud provider.
sidebar:
  order: 2
  label: Configuration
---

You run one process per UpCloud zone, and you configure it entirely with
command-line flags. You give it three things: a quota of capacity it may
provision for your fleet (the **offerings**), an OS template plus the credentials
and zone to create servers, and the addresses it listens on. Correctness concerns
like retry-safe creates and transition timeouts are handled for you and need no
tuning.

This page is the flag reference, the offerings schema, the backend modes,
pricing, and the create-then-bootstrap contract your image must satisfy. For the
API sub-account the flags imply, see [Credentials](credentials.md).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address (CapacityProvider + health + reflection). |
| `--provider` | `upcloud` | Provider/zone label stamped on every `HostRef` (e.g. `upcloud-fi-hel1`). |
| `--upcloud-backend` | `auto` | `upcloud` \| `fake` \| `auto`. `auto` = `upcloud` when credentials **and** `--zone` are set, else `fake`. See [Backend modes](#backend-modes). |
| `--username` | _(empty)_ | UpCloud API sub-account username. Falls back to `UPCLOUD_USERNAME`. Required for the `upcloud` backend. |
| `--password` | _(empty)_ | UpCloud API sub-account password. Falls back to `UPCLOUD_PASSWORD`. Required for the `upcloud` backend. |
| `--zone` | _(empty)_ | UpCloud zone id this process serves (e.g. `fi-hel1`). Required for the `upcloud` backend. |
| `--offerings` | _(built-in)_ | Path to a JSON offerings file. Omit to use a built-in mix sized by `--seed-count`. |
| `--seed-count` | `32` | Number of Speculative slots in the default offerings (ignored when `--offerings` is set). |
| `--zone-a` | `fi-hel1` | First zone for the default offerings. |
| `--zone-b` | `de-fra1` | Second zone for the default offerings. |
| `--state` | _(empty)_ | Durable state file. Empty = in-memory only (state is lost on restart). |
| `--template` | _(empty)_ | OS template storage UUID to clone at server create (e.g. an Ubuntu 24.04 cloud-init template). **Required** for the `upcloud` backend. |
| `--base-user-data` | _(empty)_ | Path to the generic, pre-binding cloud-init baked into the server at create. Installs the on-host hook **only** — never the bootstrap secret. |
| `--bootstrap-hook` | `/opt/bigfleet/bootstrap` | Image path of the executable that applies the delivered bootstrap blob. |
| `--ssh-key` | _(empty)_ | SSH private key (PEM) used to deliver Configure/Drain over SSH. |
| `--ssh-pubkey` | _(empty)_ | Authorized SSH public key injected into servers at create, so `--ssh-key` can authenticate. |
| `--ssh-user` | `root` | SSH user for Configure/Drain delivery. |
| `--eur-usd` | `1.08` | EUR→USD conversion rate applied to UpCloud prices (live-refreshed and the pinned fallback table). |
| `--price-refresh` | `45m` | Live price refresh interval (`0` = off; the pinned table still seeds startup). |
| `--reconcile-interval` | `2m` | Background UpCloud→inventory reconcile interval (`0` = off). |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz`. Empty = disabled. |
| `--reflection` | `true` | Register gRPC server reflection (for `grpcurl`/debugging). |
| `--tls-cert` | _(empty)_ | gRPC server certificate (PEM). With `--tls-key`, enables TLS. |
| `--tls-key` | _(empty)_ | gRPC server private key (PEM). |
| `--tls-ca` | _(empty)_ | gRPC client CA bundle (PEM). Enables mTLS (requires + verifies client certs). |

A minimal production invocation:

```sh
./bin/upcloud \
  --provider upcloud-fi-hel1 \
  --zone fi-hel1 \
  --template 01000000-0000-4000-8000-000030240200 \
  --offerings /etc/bigfleet/offerings.json \
  --state /var/lib/bigfleet-upcloud/state.json \
  --ssh-key /etc/bigfleet/ssh/id \
  --ssh-pubkey "$(cat /etc/bigfleet/ssh/id.pub)" \
  --ssh-user root \
  --base-user-data /etc/bigfleet/hook-init.yaml \
  --eur-usd 1.08 \
  --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
# UPCLOUD_USERNAME / UPCLOUD_PASSWORD come from the environment (a Secret).
```

## Backend modes

`--upcloud-backend` selects the substrate client:

- **`upcloud`** — the real UpCloud client backed by
  `github.com/UpCloudLtd/upcloud-go-api/v8`. Requires credentials, `--zone`, and
  `--template`; startup fails without them. This is what creates real servers and
  delivers real bootstrap over SSH.
- **`fake`** — an in-memory simulator. No UpCloud account, credentials, or network
  needed; no real servers are created. Used for dev and the credential-free
  conformance / certification run. Selecting it logs a loud warning so it is never
  mistaken for production.
- **`auto`** (default) — resolves to `upcloud` when **both** credentials (via
  `--username`/`--password` or `UPCLOUD_USERNAME`/`UPCLOUD_PASSWORD`) **and** a
  `--zone` are set, otherwise `fake`.

So a bare `./bin/upcloud --seed-count 32` (no credentials, no zone) comes up on
the fake backend — exactly how `make certify-upcloud` runs credential-free — while
setting credentials and a zone opts you into the real backend.

## Offerings

An **offering** is one shape of capacity the provider is allowed to provision: a
plan, in a zone, up to `count` slots. Each open slot is a **Speculative**
`Machine` the shard can actuate (the cloud analogue of a free pool). The offerings
are the provider's entire quota — it will never create a plan/zone combination you
did not list.

Pass a JSON file with `--offerings`. The file is a JSON array of objects:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `plan` | string | yes | UpCloud plan name, e.g. `2xCPU-4GB`, `4xCPU-8GB`, `1xCPU-1GB`, `DEV-1xCPU-1GB`. |
| `zone` | string | yes | UpCloud zone id, e.g. `fi-hel1`. The provider is multi-zone, so a zoneless offering is rejected at startup. |
| `capacity_type` | string | no | `on_demand` (default) is the only accepted value. UpCloud cloud servers are on-demand only, so `spot`, `reserved`, and `bare_metal` are all rejected at startup. |
| `count` | int | yes | Number of Speculative slots this offering provides. |
| `resources` | map[string]string | no | The per-replica request shape the offering serves (the `Machine.resources`). Operator-declared. Distinct from `allocatable`, which the provider derives from the plan. |
| `labels` | map[string]string | no | Extra labels carried on the slot. |
| `price_usd_per_hour` | float | no | Operator override of the per-offering USD/hour price. Zero (the default) uses the live UpCloud price, falling back to the pinned EUR table × `--eur-usd`. |

Example `offerings.json`:

```json
[
  {
    "plan": "2xCPU-4GB",
    "zone": "fi-hel1",
    "capacity_type": "on_demand",
    "count": 8,
    "resources": { "cpu": "1", "memory": "2Gi" }
  },
  {
    "plan": "4xCPU-8GB",
    "zone": "fi-hel1",
    "capacity_type": "on_demand",
    "count": 16,
    "resources": { "cpu": "2", "memory": "4Gi" }
  },
  {
    "plan": "DEV-1xCPU-1GB",
    "zone": "de-fra1",
    "capacity_type": "on_demand",
    "count": 4,
    "resources": { "cpu": "1", "memory": "512Mi" },
    "price_usd_per_hour": 0.0041,
    "labels": { "team": "burstable" }
  }
]
```

If you omit `--offerings`, the provider synthesizes a representative mix of
`2xCPU-4GB`/`4xCPU-8GB` slots across `--zone-a`/`--zone-b`, distributing
`--seed-count` slots evenly. That default is for dev and conformance; **real
deployments supply `--offerings`.**

Shrinking an offering (or removing it) does not delete live servers: a labelled,
running server keeps owning its slot, and any labelled server with no matching
offering is surfaced under its machine id rather than being lost.

### Plans and zones

A **plan** is an UpCloud server shape (cores + RAM). The general-purpose line runs
`1xCPU-1GB`, `1xCPU-2GB`, `2xCPU-4GB`, `4xCPU-8GB`, `6xCPU-16GB`, … up to the
large `20xCPU-128GB`; the `DEV-` line (e.g. `DEV-1xCPU-1GB`, `DEV-2xCPU-4GB`) is
the cheaper burstable/developer tier. A **zone** is an UpCloud data centre:
`fi-hel1`, `fi-hel2` (Helsinki), `de-fra1` (Frankfurt), `nl-ams1` (Amsterdam),
`uk-lon1` (London), `us-nyc1`, `us-chi1` (US), `sg-sin1` (Singapore), and more.
You run **one process per zone**, so each offering's `zone` should match that
process's `--zone`.

## Pricing & interruption

`price_per_hour` is the published UpCloud hourly on-demand rate for a plan,
reported to the engine in **USD/hour**. UpCloud bills in EUR or USD depending on
the account, so the provider keeps the cost field currency-consistent by always
emitting USD. Prices are **live-refreshed from the UpCloud API** — a frozen table
would silently drift from the real bill — with the pinned table as seed and
fallback. In precedence order:

- **Per-offering override.** If an offering sets `price_usd_per_hour`, that exact
  USD value is used. Use this for plans not priced by the API/table, or to pin a
  rate you've negotiated.
- **Live UpCloud price × `--eur-usd`.** Otherwise the price comes from the UpCloud
  `/price` endpoint (the plan/zone pricing exposed alongside `1.3/plan`),
  refreshed off the `List` hot path every `--price-refresh` (default `45m`) into a
  mutex-guarded map that `List`/`Describe` read. UpCloud quotes a plan in
  account-currency credits per hour (1 credit = one cent); the provider converts
  credits→EUR and applies the configurable `--eur-usd` rate (default `1.08`) so the
  value reaches the engine as USD. This is the **source of truth**.
- **Pinned EUR table × `--eur-usd`.** The pinned per-plan **EUR** table
  (`pricing.go`) seeds the cache before the first refresh and is the fallback when
  a refresh fails or UpCloud does not price a plan, so the provider produces a
  roughly-correct price offline (the fake backend, a credential-free
  conformance / certification run) and survives a pricing-API outage. Pin a
  current FX rate via `--eur-usd`; the cost field is a *relative* ranking signal,
  so an approximate rate is acceptable, but a stale one skews effective-cost.

The provider **fails closed on an unpriced plan**: after the startup refresh, an
offered plan with no live price, no pinned fallback, and no `price_usd_per_hour`
override would advertise a free (`0.0`) machine and corrupt the engine's cost
ranking, so the provider **refuses to start** and names the offending plan — add
it to the override or the table. (A recovered orphan of a truly unknown plan can
still report `0.0` with a loud warning, since dropping a billed server from
inventory is worse.) Last-successful-refresh age and success/failure counts are
exported as metrics (see [Observability](observability.md)).

`interruption_probability` is a **genuine `0.0`**. UpCloud has **no
spot/preemptible product** — it does not reclaim a running on-demand server to
satisfy other demand — so the correct, provider-declared value is exactly `0.0`
for every machine. This is *not* a forgotten field: a zero on a spot machine would
be a bug, but here the substrate has no spot tier. Because of that, the provider:

- declares `capacity_type = ON_DEMAND` for every machine,
- sets `interruption_probability = 0.0`,
- and **does not claim the `spot` conformance profile** — the SPOT
  `interruption_probability > 0` behaviors skip-as-pass.

The provider also **rejects** a `spot` (or `reserved` / `bare_metal`)
`capacity_type` in an offering at startup, rather than silently mis-declaring a
zero interruption probability for capacity that doesn't exist.

## Allocatable (plan capacity)

`resources` (above) is the per-replica *request* shape an offering serves;
`allocatable` is the plan's *real hardware* capacity (`cpu`, `memory`), which the
engine compares against demand. The two are **distinct on purpose** — the density
the shard packs is `floor(allocatable / resources)`. You never set `allocatable`;
the provider derives it from the plan.

It is resolved **authoritatively from UpCloud**: at startup the provider reads each
offered plan's cores and memory from the **Plans API** and caches them (specs are
immutable, so this runs once). A **pinned fallback table** of common plans
(`1xCPU-1GB` … `20xCPU-128GB`, `DEV-*`) seeds the cache, so the fake backend,
credential-free conformance, and a Plans API outage all still produce correct
`allocatable`. A plan that is neither offered-and-resolved nor pinned yields no
`allocatable`, which the engine treats as `allocatable == resources`.

:::caution
Never set `resources` to the plan's hardware total. `resources` is the per-replica
request (e.g. `{cpu:"1", memory:"2Gi"}`); `allocatable` is the box's full
cores/RAM (e.g. `4xCPU-8GB` → `{cpu:"4", memory:"8Gi"}`). Setting them equal forces
density = 1 and silently breaks the shard's packing math.
:::

## Create then bootstrap

The provider deliberately splits **create** from **cluster join**, because a
server's cloud-init `user_data` is consumed only at first boot and is read-only
afterward, but a slot's target cluster is only known when the shard binds it. The
lifecycle:

1. **Create → `CreateServer`.** Clones `--template` into a new server's OS disk in
   `--zone`, with `--base-user-data` as the generic pre-binding cloud-init, the
   BigFleet labels (`bigfleet-managed`, the machine-id label), and a freshly-minted
   SSH **host key** (injected via cloud-init, its fingerprint pinned in a label).
   The server title is derived from the operation id, so a retried Create maps to
   the same server instead of creating a second one. **Create blocks until the
   server is `started`** before returning Idle, so the immediately-following
   Configure never races a still-initializing host.
2. **Configure → SSH delivery.** Delivers the opaque `bootstrap_blob` — a **join
   secret** — to the already-running server over SSH (host key verified against the
   pinned fingerprint), writes it next to `--bootstrap-hook`, and runs the hook to
   join the cluster. We wait for the hook to **succeed**, so a failed bootstrap
   surfaces as `FAILED`. Only then is the cluster binding label recorded.
3. **Drain → the same SSH channel.** Cordons and drains the kubelet (honouring
   `grace_period_seconds`), then removes the cluster binding label — leaving the
   server running but unbound (Idle).
4. **Delete → stop + `DeleteServerAndStorages`.** Stops the server, then deletes it
   **together with its storage** (the OS disk is a separate billable resource).
   The slot returns to Speculative.

### What your template and base user-data must satisfy

- **Ship the bootstrap hook.** `--bootstrap-hook` (default `/opt/bigfleet/bootstrap`)
  must be an executable on the image; it receives the delivered blob at
  `<hook>.blob` and joins the cluster. The `--base-user-data` may install/configure
  it, but it must end up on the host.
- **The base user-data installs the hook ONLY — never the secret.** The
  cluster-join blob is a secret and is delivered later over SSH, never baked into
  `user_data` (which is immutable and readable from server metadata). See
  [Security](security.md).
- **The server must be reachable over SSH** from the provider, using `--ssh-user`
  and the key pair (`--ssh-key` / `--ssh-pubkey`). If Configure can't reach the
  server, it times out and the machine ends up `FAILED`. See
  [Troubleshooting](troubleshooting.md).
