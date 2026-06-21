---
title: Configuration
description: Every flag, the offerings JSON schema, the instance-type catalog, the backend modes, and the create-then-bootstrap (cloud-init + guest agent) model for the BigFleet libvirt provider.
sidebar:
  order: 2
  label: Configuration
---

You run one process per host-set, and you configure it entirely with
command-line flags. You give it four things: the libvirt hosts to manage (the
**`--connect`** list, one zone per host), a quota of capacity it may provision
(the **offerings**), an instance-type catalog plus a golden base image, and the
addresses it listens on. Correctness concerns like retry-safe creates and
transition timeouts are handled for you and need no tuning.

This page is the flag reference, the offerings schema, the instance-type catalog,
the backend modes, and the create-then-bootstrap contract your image must
satisfy. For how the provider authenticates to libvirt see
[Credentials](/providers/libvirt/credentials/); for how price is sourced see
[Pricing & interruption](/providers/libvirt/pricing-and-interruption/).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address (CapacityProvider + health + reflection). |
| `--provider` | `libvirt` | Provider label stamped on every `HostRef` (e.g. `libvirt-dc1`). |
| `--libvirt-backend` | `auto` | `libvirt` \| `fake` \| `auto`. `auto` = `libvirt` when `--connect` is set, else `fake`. See [Backend modes](#backend-modes). |
| `--connect` | _(empty)_ | A bare libvirt URI for the default zone, or a comma-separated `zone=uri` list for multi-host. Required for the `libvirt` backend. |
| `--default-zone` | `local` | Zone label for a single bare `--connect` URI (and the seed of the fake backend's two synthetic zones). |
| `--offerings` | _(built-in)_ | Path to a JSON offerings file. Omit to use a built-in mix sized by `--seed-count`. |
| `--seed-count` | `32` | Number of Speculative slots in the default offerings (ignored when `--offerings` is set). |
| `--capacity-type` | `on_demand` | `on_demand` or `bare_metal` for the default offerings. See [Capacity type](#capacity-type-and-delete). |
| `--instance-types` | _(built-in)_ | JSON catalog `name -> {vcpu, memory_mib}`. Omit for built-in `kvm.*` sizes. |
| `--prices` | _(synthetic)_ | Explicit `type=usd` per-hour prices. Omit for synthetic per-vCPU/GiB pricing. |
| `--price-per-vcpu-hour` | `0.0030` | Synthetic USD/hour per vCPU. |
| `--price-per-gib-hour` | `0.0008` | Synthetic USD/hour per GiB RAM. |
| `--state` | _(empty)_ | Durable state file. Empty = in-memory only (state is lost on restart). |
| `--image` | _(empty)_ | Golden base-image volume the overlay disk backs onto. **Required** for the `libvirt` backend. |
| `--storage-pool` | `default` | libvirt storage pool for the overlay + cloud-init volumes. |
| `--network` | `default` | libvirt network the domain NIC attaches to. |
| `--base-user-data` | _(empty)_ | Path to the generic, pre-binding cloud-init baked into the NoCloud datasource at define. |
| `--reconcile-interval` | `2m` | Background libvirt→inventory reconcile interval (`0` = off). |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz`. Empty = disabled. |
| `--reflection` | `true` | Register gRPC server reflection (for `grpcurl`/debugging). |
| `--tls-cert` | _(empty)_ | gRPC server certificate (PEM). With `--tls-key`, enables TLS. |
| `--tls-key` | _(empty)_ | gRPC server private key (PEM). |
| `--tls-ca` | _(empty)_ | gRPC client CA bundle (PEM). Enables mTLS (requires + verifies client certs). |

A minimal production invocation:

```sh
./bin/libvirt \
  --provider libvirt-dc1 \
  --connect 'rack1=qemu+libssh://bigfleet@host-a/system?keyfile=/etc/bigfleet/libvirt-ssh/id_ed25519&known_hosts=/etc/bigfleet/libvirt-ssh/known_hosts&known_hosts_verify=normal,rack2=qemu+libssh://bigfleet@host-b/system?keyfile=/etc/bigfleet/libvirt-ssh/id_ed25519&known_hosts=/etc/bigfleet/libvirt-ssh/known_hosts&known_hosts_verify=normal' \
  --image ubuntu-24.04.qcow2 \
  --storage-pool default --network default \
  --offerings /etc/bigfleet/offerings.json \
  --state /var/lib/bigfleet-libvirt/state.json \
  --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

## Backend modes

`--libvirt-backend` selects the substrate client:

- **`libvirt`** — the real go-libvirt client. Requires `--connect` **and**
  `--image`; startup fails without them. This defines and starts real domains and
  delivers real cloud-init/guest-agent bootstrap.
- **`fake`** — an in-memory simulator. No libvirt host, connection, or network
  needed; no real domains are created. Used for dev and the credential-free
  conformance run. Selecting it logs a loud warning so it is never mistaken for
  production.
- **`auto`** (default) — resolves to `libvirt` when `--connect` is set, otherwise
  `fake`.

So a bare `./bin/libvirt --seed-count 32` (no `--connect`) comes up on the fake
backend — exactly how `make certify-libvirt` runs credential-free — while setting
`--connect` opts you into the real backend.

## Zones map to libvirt hosts

A BigFleet **zone** is a libvirt host. The `--connect` list defines the mapping:

- A single bare URI (`qemu:///system` or `qemu+libssh://user@host/system`) is
  assigned to `--default-zone` — a single-host deployment.
- A `zone=uri` list maps each zone to a specific host
  (`rack1=qemu+libssh://user@a/system?keyfile=…&known_hosts=…&known_hosts_verify=normal,rack2=qemu+libssh://user@b/system?keyfile=…&known_hosts=…&known_hosts_verify=normal`),
  and `Create` places each domain on the host matching the slot's `zone`. Use the
  `qemu+libssh://` scheme for SSH (see [Credentials](/providers/libvirt/credentials/)
  — the pinned pure-Go client accepts the `known_hosts` host-key-pinning param
  only on the `libssh` transport; `keyfile` works on both). The provider holds
  one connection per host; the
  pure-Go go-libvirt client multiplexes concurrent calls over a single connection
  safely, so no per-host lock is taken (which would serialise every op behind a
  slow Configure/Drain).

`zone` is surfaced top-level on every machine, so `topology.kubernetes.io/zone`
selectors are satisfied directly.

## Instance-type catalog

An **instance type** is a named VM flavor (vCPU + memory). It maps to the domain's
hardware **and** to `Machine.allocatable` (the per-machine hardware capacity).
The built-in catalog:

| Type | vCPU | Memory |
|---|---|---|
| `kvm.small` | 2 | 4Gi |
| `kvm.medium` | 4 | 8Gi |
| `kvm.large` | 8 | 16Gi |
| `kvm.xlarge` | 16 | 32Gi |

Override it with `--instance-types` pointing at a JSON object:

```json
{
  "kvm.small":  { "vcpu": 2,  "memory_mib": 4096 },
  "build.2x":   { "vcpu": 8,  "memory_mib": 32768 }
}
```

`allocatable` is derived from this catalog — you never set it. It is distinct
from `resources` (below).

## Offerings

An **offering** is one shape of capacity the provider is allowed to provision: an
instance type, on a host (zone), up to `count` slots. Each open slot is a
**Speculative** `Machine` the shard can actuate into a real VM. The offerings are
the provider's entire quota — it will never create a type/zone combination you
did not list.

Pass a JSON file with `--offerings`. The file is a JSON array of objects:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `instance_type` | string | yes | A name from the instance-type catalog, e.g. `kvm.small`. Rejected at startup if not in the catalog. |
| `zone` | string | yes | The libvirt host (zone) this slot lands on. Zoneless offerings are rejected (the provider is multi-host). |
| `capacity_type` | string | no | `on_demand` (default) or `bare_metal`. `spot` is rejected (a local host has no preemption market). |
| `count` | int | yes | Number of Speculative slots this offering provides. |
| `resources` | map[string]string | no | The per-replica request shape the offering serves (the `Machine.resources`). Distinct from `allocatable`. |
| `labels` | map[string]string | no | Extra matchable labels carried on the slot. |

Example `offerings.json`:

```json
[
  {
    "instance_type": "kvm.small",
    "zone": "rack1",
    "capacity_type": "on_demand",
    "count": 8,
    "resources": { "cpu": "1", "memory": "2Gi" }
  },
  {
    "instance_type": "kvm.large",
    "zone": "rack2",
    "capacity_type": "on_demand",
    "count": 4,
    "resources": { "cpu": "2", "memory": "4Gi" },
    "labels": { "team": "batch" }
  }
]
```

If you omit `--offerings`, the provider synthesizes a representative mix of
small/large slots across two zones, distributing `--seed-count` slots evenly.
That default is for dev and conformance; **real deployments supply
`--offerings`.**

Shrinking an offering (or removing it) does not destroy domains: a managed domain
that still matches a slot keeps owning it (running → Idle; shut off → the slot
stays Speculative until a Create powers it back on), and any managed domain with
no matching offering is surfaced under its machine id — running or shut off —
rather than being lost, so it stays reapable on scale-in.

## Allocatable vs resources

`resources` is the per-replica *request* shape an offering serves; `allocatable`
is the instance type's *real hardware* capacity (`cpu`, `memory`), which the
engine compares against demand (density = `floor(allocatable / resources)`). You
never set `allocatable` — the provider derives it from the instance-type catalog.

:::caution
Never set `resources` to the instance type's hardware total. `resources` is the
per-replica request (e.g. `{cpu:"1", memory:"2Gi"}`); `allocatable` is the VM's
full vCPU/RAM (e.g. `kvm.small` → `{cpu:"2", memory:"4Gi"}`). Setting them equal
forces density = 1 and silently breaks the shard's packing math.
:::

## Capacity type and Delete

`--capacity-type` (and the per-offering `capacity_type`) picks how the pool
behaves:

- **`on_demand`** (default) — a churning pool. The provider implements `Delete`
  (Idle → Speculative = `virDomainDestroy` + `virDomainUndefine` + overlay
  delete), so it claims the **cloud** conformance profile. Use this to model a
  pool that scales in and out, freeing host resources.
- **`bare_metal`** — a fixed free pool. `price_per_hour` is 0 (owned hardware),
  and since M73 the shard never sends `Delete` for bare-metal capacity. Use this
  when the VMs are long-lived and you don't want them destroyed on scale-in.

Pick deliberately; the choice drives the shard's idle-hold policy and whether
`Delete` is ever sent.

## Create then bootstrap

The provider deliberately splits **create** from **cluster join**, because a
cloud-init NoCloud datasource is consumed at first boot but a slot's target
cluster is only known when the shard binds it. The lifecycle:

1. **Create → define + start.** Creates a copy-on-write overlay from `--image`,
   builds a cloud-init NoCloud datasource (the generic `--base-user-data`), renders
   the domain XML (vCPU/memory from the instance type, a virtio NIC on `--network`,
   the overlay disk, a qemu guest-agent channel), then `DomainDefineXML` +
   `DomainCreate`. The operation id makes the domain name stable, so a retried
   Create maps to the same domain instead of defining a second one. The machine
   settles to **Idle** with a populated `host` (`<zone>/<domain>`) once the domain
   is running.
2. **Configure → guest agent.** Delivers the opaque `bootstrap_blob` via the qemu
   guest agent: writes the blob to `/opt/bigfleet/bootstrap.blob` and runs the
   image's hook (`/opt/bigfleet/bootstrap <cluster-id>`) with `guest-exec`,
   polling `guest-exec-status` until it exits. We wait for the hook to
   **succeed**, so a failed bootstrap surfaces as `FAILED`. `cluster` and
   `shard_metadata` are recorded.
3. **Drain → guest agent.** Cordons and drains the kubelet (honouring
   `grace_period_seconds`), then clears the cluster binding — leaving the domain
   running but unbound (Idle). `cluster` and `shard_metadata` are cleared.
4. **Delete → destroy + undefine.** Destroys and undefines the domain and deletes
   its overlay + cloud-init volumes (keeping the golden base image); the slot
   returns to Speculative (host cleared).

### The image hook contract

Your golden base image must satisfy:

- **Run `qemu-guest-agent`.** Configure and Drain reach the guest through the
  guest-agent channel (no SSH into the VM is required). The agent must be enabled
  and started on boot.
- **Ship the bootstrap hook** at `/opt/bigfleet/bootstrap`. On Configure the
  provider writes the decoded bootstrap blob to `/opt/bigfleet/bootstrap.blob` and
  runs `/opt/bigfleet/bootstrap <cluster-id>`; the hook joins the node to the
  cluster and must exit non-zero on failure (so a broken join becomes `FAILED`,
  not a falsely-Idle node). The blob is opaque — the hook consumes it verbatim.
- **Have a kubelet** preinstalled (so "Idle" means a ready-to-join node).

The blob is never parsed or rewritten by the provider — it is the cluster
operator's kubelet join material (cloud-init user-data / a join script / a kubeadm
token), delivered as opaque bytes.
