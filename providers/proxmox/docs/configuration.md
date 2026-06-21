---
title: Configuration
description: Every flag, the offerings JSON schema, the instance-type catalog, the two backend modes, and the clone-then-bootstrap (guest agent) model for the BigFleet Proxmox VE provider.
sidebar:
  order: 2
  label: Configuration
---

You run one process per Proxmox cluster, and you configure it entirely with
command-line flags. You give it a few things: a quota of capacity it may
provision for your fleet (the **offerings**), an instance-type catalog naming a
source template plus the clone's hardware, the connection to your cluster API,
and the addresses it listens on. Correctness concerns like retry-safe clones and
transition timeouts are handled for you and need no tuning.

This page is the flag reference, the offerings and instance-type schemas, the
backend modes, and the clone-then-bootstrap contract your template must satisfy.
For the API token and TLS trust the connection flags imply, see
[Credentials](/providers/proxmox/credentials/).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address (CapacityProvider + health + reflection). |
| `--provider` | `proxmox` | Provider/cluster label stamped on every `HostRef` (e.g. `proxmox-dc1`). |
| `--proxmox-backend` | `auto` | `proxmox` \| `fake` \| `auto`. `auto` = `proxmox` when `--proxmox-api-url` is set, else `fake`. See [Backend modes](#backend-modes). |
| `--proxmox-api-url` | _(empty)_ | Proxmox API URL, e.g. `https://host:8006/api2/json`. Required for the `proxmox` backend; also what flips `auto` to `proxmox`. |
| `--proxmox-token-id` | _(empty)_ | API token id, `USER@REALM!TOKENID`. |
| `--proxmox-token-secret` | _(empty)_ | API token secret. Prefer `--proxmox-token-file`. |
| `--proxmox-token-file` | _(empty)_ | File holding the API token secret (kept out of the arg list). Wins over `--proxmox-token-secret` when both are set. |
| `--proxmox-ca-file` | _(empty)_ | PEM CA bundle verifying the Proxmox API cert (e.g. `/etc/pve/pve-root-ca.pem`). |
| `--proxmox-tls-fingerprint` | _(empty)_ | Pinned SHA-256 fingerprint of the API cert (alternative to `--proxmox-ca-file`). |
| `--proxmox-pool` | _(empty)_ | Resource pool clones are placed in (least-privilege scope). |
| `--nodes` | _(empty)_ | Comma list of cluster node names, each a BigFleet zone. Required for the `proxmox` backend. |
| `--default-zone` | `pve` | Zone seed for the fake backend's two synthetic zones (`<seed>-a`, `<seed>-b`). |
| `--offerings` | _(built-in)_ | Path to a JSON offerings file. Omit for a built-in mix sized by `--seed-count`. |
| `--seed-count` | `32` | Number of Speculative slots in the default offerings (ignored when `--offerings` is set). |
| `--instance-types` | _(built-in)_ | JSON instance-type catalog (`name -> {vcpu, memory_mib, template_vmid}`). Omit for the built-in `pve.*` sizes. |
| `--template-vmid` | `9000` | Default source template VMID the default catalog (and zero-template entries) clone from. |
| `--prices` | _(empty)_ | Explicit USD/hour per type as `type=usd` pairs. Empty = synthetic pricing. |
| `--price-per-vcpu-hour` | `0.0030` | Synthetic USD/hour per vCPU when no explicit price is set. |
| `--price-per-gib-hour` | `0.0008` | Synthetic USD/hour per GiB RAM when no explicit price is set. |
| `--bootstrap-path` | `/run/bigfleet-bootstrap` | In-guest path the bootstrap blob is written to before it is executed. |
| `--bootstrap-exec` | `/bin/sh` | Comma-separated argv that runs the bootstrap; the path is appended as the final arg. |
| `--reconcile-interval` | `2m` | Background Proxmox→inventory reconcile interval (`0` = off). |
| `--state` | _(empty)_ | Durable state file. Empty = in-memory only (state is lost on restart). |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz`. Empty = disabled. |
| `--reflection` | `true` | Register gRPC server reflection (for `grpcurl`/debugging). |
| `--tls-cert` | _(empty)_ | Server certificate (PEM) for the gRPC listener. With `--tls-key`, enables TLS. |
| `--tls-key` | _(empty)_ | Server private key (PEM) for the gRPC listener. |
| `--tls-ca` | _(empty)_ | Client CA bundle (PEM). Enables mTLS on the gRPC listener (requires + verifies client certs). |

A minimal production invocation:

```sh
./bin/proxmox \
  --provider proxmox-dc1 \
  --proxmox-api-url https://pve1.example.internal:8006/api2/json \
  --proxmox-token-id 'bigfleet@pve!bigfleet' \
  --proxmox-token-file /etc/bigfleet/proxmox-token/token \
  --proxmox-ca-file /etc/pve/pve-root-ca.pem \
  --proxmox-pool bigfleet \
  --nodes pve1,pve2,pve3 \
  --template-vmid 9000 \
  --offerings /etc/bigfleet/offerings.json \
  --state /var/lib/bigfleet-proxmox/state.json \
  --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

:::note
With the real backend every offering must place onto a node listed in
`--nodes`, or Create would only fail at runtime. The provider validates this at
startup and refuses to serve if an offering names an unknown node.
:::

## Backend modes

`--proxmox-backend` selects the substrate client:

- **`proxmox`** — the real client backed by the pure-Go go-proxmox library
  against the cluster's `/api2/json` REST API. Requires `--proxmox-api-url`,
  `--proxmox-token-id`, a token secret, **and** `--nodes`; startup fails without
  them. This is what creates real VMs and drives the real guest agent.
- **`fake`** — an in-memory simulator. No Proxmox cluster, credentials, or
  network needed; no real VMs are created. Used for dev and the credential-free
  conformance run. Selecting it logs a loud warning so it is never mistaken for
  production.
- **`auto`** (default) — resolves to `proxmox` when `--proxmox-api-url` is set,
  otherwise `fake`.

So a bare `./bin/proxmox --seed-count 32` (no `--proxmox-api-url`) comes up on the
fake backend — exactly how `make certify-proxmox` runs credential-free — while
setting `--proxmox-api-url` opts you into the real backend.

## Offerings

An **offering** is one shape of capacity the provider is allowed to provision: an
instance type, on a node (zone), at a capacity type, up to `count` slots. Each
open slot is a **Speculative** `Machine` the shard can actuate into a real VM
(the cloud analogue of a free pool). The offerings are the provider's entire
quota — it will never clone a type/zone combination you did not list.

Pass a JSON file with `--offerings`. The file is a JSON array of objects:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `instance_type` | string | yes | Catalog instance type, e.g. `pve.medium`. Must be in the instance-type catalog. |
| `zone` | string | yes | The Proxmox node the clone lands on, e.g. `pve1`. Zoneless offerings are rejected at startup (the provider is multi-zone). With the real backend the node must be in `--nodes`. |
| `capacity_type` | string | no | Only `on_demand` is meaningful (default). `spot`, `reserved`, and `bare_metal` are rejected — see below. |
| `count` | int | yes | Number of Speculative slots this offering provides. Must be positive. |
| `resources` | map[string]string | no | The per-replica request shape the offering serves (the `Machine.resources`). Distinct from `allocatable`, which is the instance type's full hardware. |
| `labels` | map[string]string | no | Extra labels carried on the slot. |

`capacity_type` accepts the on-demand spellings (`on_demand`/`on-demand`/
`ondemand`, or empty). `spot`, `reserved`, and `bare_metal` are **rejected at
startup**: a self-hosted cluster has no preemption market (no spot) and no
reservation/commitment billing (no reserved), and these VMs are deletable clones
rather than a fixed free pool (no bare-metal). Every machine is `ON_DEMAND` with
`interruption_probability` of exactly `0`.

Example `offerings.json`:

```json
[
  {
    "instance_type": "pve.medium",
    "zone": "pve1",
    "capacity_type": "on_demand",
    "count": 8,
    "resources": { "cpu": "1", "memory": "2Gi" }
  },
  {
    "instance_type": "pve.large",
    "zone": "pve2",
    "capacity_type": "on_demand",
    "count": 4,
    "resources": { "cpu": "2", "memory": "4Gi" },
    "labels": { "team": "ml" }
  }
]
```

If you omit `--offerings`, the provider synthesizes a representative mix of two
sizes across two nodes, distributing `--seed-count` slots evenly. The nodes are
the first two `--nodes` entries (or two synthetic zones derived from
`--default-zone` on the fake backend). That default is for dev and conformance;
**real deployments supply `--offerings`.**

Shrinking an offering (or removing it) does not destroy live VMs: a tagged VM
backing a slot keeps owning it, and any tagged VM with no matching offering is
surfaced under its machine id (Idle, with its host) rather than being lost — so
the engine can still scale it in and `Delete` tears it down.

## Instance-type catalog

Unlike a cloud, Proxmox has no upstream API listing instance types — there is no
`DescribeInstanceTypes`. So the catalog is declared in config. An **instance
type** maps a flavor name to the clone's `vcpu`, `memory_mib`, and the source
`template_vmid` it clones from.

Pass `--instance-types` pointing at a JSON object of
`name -> {vcpu, memory_mib, template_vmid}`:

```json
{
  "pve.small":  { "vcpu": 2,  "memory_mib": 4096,  "template_vmid": 9000 },
  "pve.medium": { "vcpu": 4,  "memory_mib": 8192,  "template_vmid": 9000 },
  "pve.large":  { "vcpu": 8,  "memory_mib": 16384, "template_vmid": 9000 }
}
```

`vcpu` and `memory_mib` must be positive. A zero or omitted `template_vmid` falls
back to `--template-vmid` (default `9000`). Omit `--instance-types` entirely to
use the built-in catalog: `pve.small` (2 vCPU / 4 GiB), `pve.medium` (4 / 8),
`pve.large` (8 / 16), `pve.xlarge` (16 / 32), all cloning from `--template-vmid`.

### Allocatable vs resources

`resources` (on the offering) is the per-replica *request* shape; `allocatable`
is the instance type's *real hardware* capacity (`cpu`, `memory`), which the
engine compares against demand (density = `floor(allocatable / resources)`). You
never set `allocatable` — the provider derives it from the catalog entry's
`vcpu`/`memory_mib`. Memory renders as `Gi` when it is a whole number of GiB,
else `Mi`, so it round-trips without precision loss. Keep `resources` and
`allocatable` distinct (resources smaller) so density math is meaningful.

## Pricing

Proxmox has no cloud bill, so `price_per_hour` is **synthetic** — derived from the
instance type's hardware: `vcpu × --price-per-vcpu-hour + gib × --price-per-gib-hour`
(defaults `0.0030`/`0.0008`). Pin an explicit per-type price instead with
`--prices type=usd,...`. The value is a relative ranking signal for the engine's
effective-cost formula, not a real invoice, so approximate synthetic pricing is
fine. A type with no override and no catalog entry prices at `0`.

## Clone then bootstrap

The provider deliberately splits **clone** from **cluster join**: a Proxmox
template's first-boot config (cloud-init) is consumed only once, at first boot,
but a slot's target cluster is only known when the shard binds it — and the join
secret is confidential, so it must not ride a first-boot-only, non-confidential
channel. The lifecycle:

1. **Create → clone + start.** Clones the instance type's `template_vmid` onto the
   target node (`zone`) as a full clone in `--proxmox-pool`, sizes it
   (`cores`/`memory` from the catalog), tags it `bigfleet` and
   `bigfleet-<machine-id>`, records the verbatim machine id and the operation id
   in the VM Description, starts it, and **waits until the qemu guest agent is
   reachable** before returning Idle. **Create is idempotent**: a retried clone
   finds the VM already tagged for this machine id and adopts it (re-powering it
   if it was stopped) rather than cloning a second VM — a retried Create converges
   on one VM.
2. **Configure → guest agent.** First powers the VM on if it was stopped out of
   band (`EnsureRunning` — see below), then delivers the opaque `bootstrap_blob`
   over the **qemu guest agent**: it writes the blob to `--bootstrap-path` in the
   guest (agent file-write) and runs `--bootstrap-exec <path>` (agent exec),
   **waiting for the hook to exit**. A non-zero exit becomes `FAILED`, never a
   false Configured.
3. **Drain → guest agent.** Powers the VM on if needed, then runs `kubectl drain`
   over the guest agent (honouring the grace period) and clears the cluster
   binding, leaving the VM running but unbound (Idle).
4. **Delete → destroy.** Stops the VM and destroys it together with its disks
   (purge + destroy unreferenced disks); the slot returns to Speculative.
   **Delete is idempotent**: an already-gone VM succeeds.

### EnsureRunning before Configure and Drain

A VM the kit holds Idle may have been stopped out of band (an operator
power-cycle, an HA event, a maintenance reboot). Because the bootstrap and drain
hooks run over the guest agent, the provider powers the VM on and waits for the
agent **first** on every Configure and Drain. It is a no-op when the VM is already
running with a reachable agent. Without this, the guest-agent call would loop
until the transition timed out and FAILed.

### The template contract

Configure does not bake cluster-join logic into the provider — it delivers an
opaque blob and runs a hook your **template image** ships. The template must:

- Have **`qemu-guest-agent` installed and enabled** (and started at boot). The
  whole delivery path — Create's wait-for-agent, Configure's file-write/exec,
  Drain's exec — depends on it. Without it Create never settles Idle and
  Configure/Drain cannot reach the guest.
- Have **`kubelet` preinstalled** (and anything else cluster-join needs), so the
  generic, cluster-agnostic pre-binding is already in the image. Only the
  per-cluster secret arrives later, over the guest agent.
- Ship the bootstrap hook the provider invokes. By default the provider runs
  `/bin/sh <--bootstrap-path>` — i.e. the blob is written to
  `/run/bigfleet-bootstrap` and executed as a shell script. The blob is opaque to
  the provider; interpret it however your join flow needs (a kubeadm join script,
  a config bundle, secrets) and **exit non-zero on any failure** — a non-zero
  exit is what turns a botched bootstrap into a `FAILED` machine instead of a
  silently-broken node. Override the runner with `--bootstrap-exec` (e.g.
  `--bootstrap-exec /usr/bin/python3` to run the blob as Python, or point it at a
  fixed hook with the path appended).
- For Drain, have `kubectl` available and the Kubernetes node name match
  `hostname` inside the guest (the drain runs
  `kubectl drain "$(hostname)" --ignore-daemonsets --delete-emptydir-data`).

Generic, non-secret pre-binding belongs in the template (the guest agent, the
kubelet, image pulls); only the confidential per-cluster bootstrap rides the
guest agent at Configure time. The blob is never delivered via cloud-init or
user-data — cloud-init is first-boot-only and not a confidential per-Configure
channel.
