---
title: Configuration
description: Every flag, the offerings JSON schema, the backend modes, and the create-then-bootstrap (in-band SSH) model for the BigFleet GCP provider.
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
| `--ssh-key` | _(empty)_ | SSH private key for in-band Configure/Drain delivery. Without it, Configure cannot deliver the bootstrap blob. |
| `--ssh-user` | `bigfleet` | SSH user for Configure/Drain (authorised on the instance via `ssh-keys` metadata). |
| `--bootstrap-hook` | `/opt/bigfleet/bootstrap` | Image path that consumes the delivered bootstrap blob and joins the cluster. See [the image contract](#the-image-hook-contract). |
| `--use-external-ip` | `false` | Reach instances over an ephemeral external IP for SSH (default: internal IP, provider in the same VPC). |
| `--reconcile-interval` | `2m` | Background GCE→inventory reconcile interval (`0` = off; also observes Spot preemptions). |
| `--price-refresh` | `45m` | Live on-demand price refresh interval from the Cloud Billing Catalog (`0` = off, pinned table only). Read off the `List` hot path. |
| `--pricing-api-key` | _(empty)_ | Cloud Billing Catalog API key for price refresh (default: Application Default Credentials). |
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
  --ssh-key /etc/bigfleet/ssh/id_ed25519 \
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
  what creates real instances and delivers the bootstrap in-band over SSH.
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

The provider deliberately splits **create** from **cluster join**, because a
slot's target cluster is only known when the shard binds it. The
cluster-specific bootstrap is delivered **in-band over SSH** to the
already-running host — no reboot, and the join secret is never persisted in
instance metadata (matching the certified AWS/Hetzner providers). The lifecycle:

1. **Create → `Instances.Insert`.** Creates the instance from `--image` with
   `--base-startup-script` as the boot script (a generic, cluster-agnostic node
   bootstrap), in the chosen zone, with the `bigfleet-managed` and
   `bigfleet-capacity` labels and the machine id recorded in `bigfleet-machine-id`
   instance **metadata** (the id is too long for a 63-char label value). It also
   authorises the provider's `--ssh-key` via `ssh-keys` metadata and injects a
   pinned SSH host key (cloud-init `user-data`) for later host verification. Spot
   offerings set `scheduling.provisioningModel = SPOT`. The operation id makes the
   instance **name** stable, so a retried Create maps to the same instance.
   **Create blocks until the instance is actually `RUNNING`** before returning
   Idle, so the immediately-following Configure never races a still-booting host.
2. **Configure → SSH (no reboot).** Connects to the running host over SSH (as
   `--ssh-user`, verifying the pinned host key), writes the opaque `bootstrap_blob`
   to `<bootstrap-hook>.blob` with `umask 077`, and runs `<bootstrap-hook>
   <cluster-id>`, waiting for it to **succeed** (a failed hook surfaces as
   `FAILED`). Only after success is the cluster id recorded in `bigfleet-cluster`
   metadata — so a failed Configure never records a binding it never made. The blob
   is opaque (never parsed) and is **not** persisted in metadata.
3. **Drain → SSH (no reboot).** Cordons and drains the kubelet over SSH (`kubectl
   cordon`/`drain`, honouring `grace_period_seconds`), then clears the
   `bigfleet-cluster` metadata — leaving the instance running but unbound (Idle).
   `cluster` and `shard_metadata` are cleared.
4. **Delete → `Instances.Delete`.** Deletes the instance; the slot returns to
   Speculative (host cleared).

### The image hook contract

Your boot image must satisfy two things:

- **Authorise `--ssh-key`.** The provider connects as `--ssh-user` (default
  `bigfleet`) using the private key you pass; its public key is authorised on the
  instance via `ssh-keys` metadata (the guest agent provisions it; the provider
  also sets `enable-oslogin=false` so metadata SSH keys are honoured). For
  pre-pinned host keys (no trust-on-first-use window) use a **cloud-init-enabled
  image** (e.g. Ubuntu) so the injected `user-data` host key takes effect; on
  images without cloud-init the provider trust-on-first-uses and pins the observed
  host key.
- **Ship the bootstrap hook** at `--bootstrap-hook` (default
  `/opt/bigfleet/bootstrap`). On Configure the provider writes the decoded blob to
  `<hook>.blob` and runs `<hook> <cluster-id>`; the hook joins the node to the
  cluster and must exit non-zero on failure (so a broken join becomes `FAILED`,
  not a falsely-Idle node). The blob is opaque — the hook consumes it verbatim.

If you run without `--ssh-key`, Configure cannot deliver the blob and the machine
ends up `FAILED`; Drain degrades to clearing the binding metadata only. For a real
deployment, always set `--ssh-key`. The provider also needs network reachability
to the instance (same VPC for the default internal IP, or `--use-external-ip`).
