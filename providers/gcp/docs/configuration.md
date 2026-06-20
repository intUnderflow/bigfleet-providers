---
title: Configuration
description: Every flag, the offerings JSON schema, the backend modes, and the create-then-bootstrap (startup-script) model for the BigFleet GCP provider.
sidebar:
  order: 2
  label: Configuration
---

You run one process per GCP region, and you configure it entirely with
command-line flags. You give it three things: a quota of capacity it may
provision for your fleet (the **offerings**), a project + region + boot image to
create instances, and the addresses it listens on. Correctness concerns like
retry-safe creates and transition timeouts are handled for you and need no
tuning.

This page is the flag reference, the offerings schema, the backend modes, and
the create-then-bootstrap contract your image must satisfy. For the credentials
the flags imply see [Credentials](/providers/gcp/credentials/); for how price
and interruption are sourced see
[Pricing & interruption](/providers/gcp/pricing-and-interruption/).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address (CapacityProvider + health + reflection). |
| `--provider` | `gcp` | Provider/region label stamped on every `HostRef` (e.g. `gcp-us-central1`). |
| `--gcp-backend` | `auto` | `gcp` \| `fake` \| `auto`. `auto` = `gcp` when `--region` is set, else `fake`. See [Backend modes](#backend-modes). |
| `--project` | _(empty)_ | GCP project id. **Required** for the `gcp` backend. |
| `--region` | _(empty)_ | GCP region, e.g. `us-central1`. **Required** for the `gcp` backend. |
| `--offerings` | _(built-in)_ | Path to a JSON offerings file. Omit to use a built-in mix sized by `--seed-count`. |
| `--seed-count` | `32` | Number of Speculative slots in the default offerings (ignored when `--offerings` is set). |
| `--zone-a` | `<region>-a` | First zone for the default offerings. |
| `--zone-b` | `<region>-b` | Second zone for the default offerings. |
| `--state` | _(empty)_ | Durable state file. Empty = in-memory only (state is lost on restart). |
| `--image` | `…/debian-cloud/…/debian-12` | Boot disk source image for `Instances.Insert`. |
| `--network` | `global/networks/default` | VPC network for the instance NIC. |
| `--subnetwork` | _(empty)_ | Subnetwork for the NIC (default: the network's auto subnet). |
| `--disk-size-gb` | `20` | Boot disk size in GiB. |
| `--instance-service-account` | _(empty)_ | Service account the **launched instances** run as (default: project default). |
| `--base-startup-script` | _(empty)_ | Path to the generic, pre-binding startup script baked in at Insert. See [the image contract](#the-image-hook-contract). |
| `--reconcile-interval` | `2m` | Background GCE→inventory reconcile interval (`0` = off). |
| `--metrics-addr` | `:9090` | Address for `/metrics`, `/healthz`, `/readyz`. Empty = disabled. |
| `--reflection` | `true` | Register gRPC server reflection (for `grpcurl`/debugging). |
| `--tls-cert` | _(empty)_ | Server certificate (PEM). With `--tls-key`, enables TLS. |
| `--tls-key` | _(empty)_ | Server private key (PEM). |
| `--tls-ca` | _(empty)_ | Client CA bundle (PEM). Enables mTLS (requires + verifies client certs). |

A minimal production invocation:

```sh
./bin/gcp \
  --provider gcp-us-central1 \
  --project my-gcp-project \
  --region us-central1 \
  --image projects/debian-cloud/global/images/family/debian-12 \
  --instance-service-account bigfleet-node@my-gcp-project.iam.gserviceaccount.com \
  --offerings /etc/bigfleet/offerings.json \
  --state /var/lib/bigfleet-gcp/state.json \
  --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

Credentials are not a flag: the provider authenticates to GCE via **Application
Default Credentials** (Workload Identity on GKE, or `GOOGLE_APPLICATION_CREDENTIALS`
off-GKE). See [Credentials](/providers/gcp/credentials/).

## Backend modes

`--gcp-backend` selects the substrate client:

- **`gcp`** — the real GCE client backed by `cloud.google.com/go/compute`.
  Requires `--project` **and** `--region`; startup fails without them. This is
  what creates real instances, writes startup-script metadata, and resets.
- **`fake`** — an in-memory simulator. No GCP project, credentials, or network
  needed; no real instances are created. Used for dev and the credential-free
  certification run. Selecting it logs a loud warning so it is never mistaken for
  production.
- **`auto`** (default) — resolves to `gcp` when `--region` is set, otherwise
  `fake`.

So a bare `./bin/gcp --seed-count 32` (no region) comes up on the fake backend —
exactly how `make certify-gcp` runs credential-free — while setting
`--project`/`--region` opts you into the real backend.

## Offerings

An **offering** is one shape of capacity the provider is allowed to provision: a
machine type, in a zone, at a capacity type, up to `count` slots. Each open slot
is a **Speculative** `Machine` the shard can actuate (the cloud analogue of a
free pool). The offerings are the provider's entire quota — it will never create
a combination you did not list.

Pass a JSON file with `--offerings`. The file is a JSON array of objects:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `machine_type` | string | yes | GCE machine type, e.g. `n2-standard-8`, `c3-highmem-22`. |
| `zone` | string | yes | GCE zone, e.g. `us-central1-a`. Zoneless offerings are rejected at startup (the provider is multi-zone). |
| `capacity_type` | string | no | `on_demand` (default), `spot`, or `reserved`. `bare_metal` is rejected (GCE creates VMs). |
| `count` | int | yes | Number of Speculative slots this offering provides. |
| `resources` | map[string]string | no | The per-replica request shape the offering serves (the `Machine.resources`). Distinct from `allocatable`, which is derived from the machine type. |
| `labels` | map[string]string | no | Extra labels carried on the slot. Accelerator families (`a2*`/`a3*`/`g2*`) also get an automatic `bigfleet.io/accelerator` label. |

Example `offerings.json`:

```json
[
  {
    "machine_type": "n2-standard-8",
    "zone": "us-central1-a",
    "capacity_type": "on_demand",
    "count": 8,
    "resources": { "cpu": "2", "memory": "4Gi" }
  },
  {
    "machine_type": "c2-standard-8",
    "zone": "us-central1-b",
    "capacity_type": "spot",
    "count": 16,
    "resources": { "cpu": "2", "memory": "4Gi" }
  },
  {
    "machine_type": "g2-standard-4",
    "zone": "us-central1-a",
    "capacity_type": "on_demand",
    "count": 2,
    "resources": { "cpu": "4", "memory": "16Gi" },
    "labels": { "team": "ml" }
  }
]
```

The `g2-standard-4` offering carries both `team` and the automatic
`bigfleet.io/accelerator=nvidia-l4` label.

If you omit `--offerings`, the provider synthesizes a representative mix of
on-demand and **spot** `n2`/`c2` slots across `--zone-a`/`--zone-b`, distributing
`--seed-count` slots evenly. That default is for dev and certification (it
includes Spot so the SPOT invariant fires); **real deployments supply
`--offerings`.**

Shrinking an offering (or removing it) does not delete live instances: a
labelled, running instance keeps owning its slot, and any labelled instance with
no matching offering is surfaced as Idle under its machine id rather than lost.

## Allocatable (machine-type capacity)

`resources` (above) is the per-replica *request* shape an offering serves;
`allocatable` is the machine type's *real hardware* capacity (`cpu`, `memory`),
which the engine compares against demand (density = `floor(allocatable /
resources)`). You never set `allocatable` — the provider derives it from the
machine type.

It is resolved **authoritatively from GCE**: at startup the provider reads each
offered type's `guest_cpus`/`memory_mb` from the MachineTypes API and caches
them. A **pinned fallback table** of common families (e2/n2/n2d/c2/c3/m1/a2/g2)
seeds the cache, so the fake backend, credential-free certification, and a
MachineTypes API outage all still produce correct `allocatable`. A type that is
neither offered-and-resolved nor pinned yields no `allocatable`, which the engine
treats as `allocatable == resources`.

:::caution
Never set `resources` to the machine-type hardware total. `resources` is the
per-replica request (e.g. `{cpu:"2", memory:"4Gi"}`); `allocatable` is the box's
full vCPU/RAM (e.g. `n2-standard-8` → `{cpu:"8", memory:"32Gi"}`). Setting them
equal forces density = 1 and silently breaks the shard's packing math.
:::

## Create then bootstrap

The provider deliberately splits **create** from **cluster join**, because a GCE
instance consumes its `startup-script` only at boot but a slot's target cluster
is only known when the shard binds it. The lifecycle:

1. **Create → `Instances.Insert`.** Creates the instance from `--image` with
   `--base-startup-script` as the boot script (a generic, cluster-agnostic node
   bootstrap), in the chosen zone, with the BigFleet labels (`bigfleet-managed`,
   `bigfleet-machine-id`, `bigfleet-capacity`). Spot offerings set
   `scheduling.provisioningModel = SPOT`. The operation id makes the instance
   **name** stable, so a retried Create maps to the same instance instead of
   creating a second one. **Create blocks until the instance is actually
   `RUNNING`** before returning Idle, so the immediately-following Configure never
   races a still-booting host.
2. **Configure → `SetMetadata` + `Reset`.** Overwrites the instance's
   `startup-script` metadata with the opaque `bootstrap_blob` (preserving other
   metadata items) and resets the instance so the script runs on the next boot and
   the node joins `cluster_id`'s cluster. The instance is then labelled
   `bigfleet-cluster=<id>` — only **after** the blob applied, so a failed Configure
   never leaves an instance mislabelled. The blob is opaque — never parsed.
3. **Drain → `SetMetadata`.** Strips the delivered `startup-script` metadata (so
   the node will not rejoin on a future boot) and clears the cluster label —
   leaving the instance running but unbound (Idle). BigFleet has already cordoned
   and drained the pods at the k8s layer (honouring `grace_period_seconds`); this
   is the machine-side cleanup. `cluster` and `shard_metadata` are cleared.
4. **Delete → `Instances.Delete`.** Deletes the instance; the slot returns to
   Speculative (host cleared).

### The image hook contract

Your boot image must satisfy one thing: **a `startup-script` it runs on boot
that consumes the delivered bootstrap and joins the cluster.** Two equivalent
shapes:

- **Direct startup-script (default).** Configure writes the `bootstrap_blob`
  *verbatim* as the `startup-script` metadata value and resets; the image's
  startup-script runner executes it. The blob is the kubelet join script.
- **Indirect (baked image).** Bake an image whose own startup logic fetches a
  metadata key from the metadata server on every boot; have your generic
  `--base-startup-script` read it. Configure then only writes the metadata and
  resets.

Either way the blob is opaque and the provider never parses it. On Drain the
`startup-script` is removed so a future boot does not rejoin.
