---
title: Configuration
description: Every flag of the BigFleet OCI provider, the offerings schema, the bootstrap hook contract, and the flexible-shape OCPU/memory model.
sidebar:
  order: 2
  label: Configuration
---

## Flags

| Flag | Default | Purpose |
|---|---|---|
| `--addr` | `:9000` | gRPC listen address (BigFleet dials this). |
| `--provider` | `oci` | Provider/region label stamped on `HostRef.provider` (e.g. `oci-eu-frankfurt-1`). |
| `--oci-backend` | `auto` | `oci` \| `fake` \| `auto` (auto = oci when `--region` and `--compartment` set, else fake). |
| `--region` | — | OCI region identifier, e.g. `eu-frankfurt-1` (required for the oci backend). |
| `--compartment` | — | Compartment OCID the provider operates in. |
| `--subnet` | — | Subnet OCID for `LaunchInstance`. |
| `--image` | — | Base image OCID for `LaunchInstance`. |
| `--auth` | `auto` | `instance_principal` \| `workload_identity` \| `config_file` \| `auto`. |
| `--offerings` | — | Path to a JSON offerings file (default: a built-in mix sized by `--seed-count`). |
| `--seed-count` | `32` | Number of Speculative slots when using the default offerings. |
| `--ad-a` / `--ad-b` | — | Availability domains for the default offerings. |
| `--prices-file` | — | Override the embedded `prices.yaml` price table. |
| `--bootstrap-hook` | `/opt/bigfleet/bootstrap` | Image path that applies the delivered bootstrap blob. |
| `--base-user-data` | — | Path to the generic pre-binding cloud-init baked in at launch. |
| `--reconcile-interval` | `2m` | Background OCI→inventory reconcile interval (0 = off). |
| `--state` | — | Durable state file (empty = in-memory). |
| `--metrics-addr` | `:9090` | `/metrics`, `/healthz`, `/readyz` (empty = disabled). |
| `--reflection` | `true` | Register gRPC server reflection. |
| `--tls-cert` / `--tls-key` / `--tls-ca` | — | Server TLS; setting `--tls-ca` enables mTLS. |

## Offerings

An offering is one shape of capacity the provider may provision: an OCI **shape**
in an **availability domain** at a **capacity type**, up to `count` slots. Each
open slot is a Speculative machine the shard can actuate.

```json
[
  {
    "shape": "VM.Standard.E5.Flex",
    "availability_domain": "Uocm:EU-FRANKFURT-1-AD-1",
    "capacity_type": "on_demand",
    "count": 10,
    "ocpus": 2,
    "memory_gb": 16,
    "resources": { "cpu": "1", "memory": "2Gi" }
  },
  {
    "shape": "VM.Standard.E5.Flex",
    "availability_domain": "Uocm:EU-FRANKFURT-1-AD-1",
    "capacity_type": "spot",
    "count": 20,
    "ocpus": 2,
    "memory_gb": 16,
    "resources": { "cpu": "1", "memory": "2Gi" }
  },
  {
    "shape": "BM.Standard.E5.192",
    "availability_domain": "Uocm:EU-FRANKFURT-1-AD-2",
    "capacity_type": "bare_metal",
    "count": 2,
    "resources": { "cpu": "8", "memory": "32Gi" }
  }
]
```

- `shape` → `Machine.instance_type` (top-level). The OCI shape name.
- `availability_domain` → `Machine.zone` (top-level). Satisfies
  `topology.kubernetes.io/zone`.
- `capacity_type` → `on_demand` | `spot` (preemptible) | `bare_metal`. Capacity is
  taken from this declared value, not inferred from the shape: a `BM.*` shape
  declared `on_demand` is hourly-billed ON_DEMAND capacity (priced, idle-
  releasable); declare `bare_metal` for a held, price-0 free-pool lane.
- `count` → the number of Speculative slots (the quota the shard may Create).
- `ocpus` / `memory_gb` → **required for flexible shapes** (name ends `.Flex`);
  they size the launch `ShapeConfig` and `Machine.allocatable`. Ignored for fixed
  shapes (which pin their own OCPU/memory).
- `resources` → `Machine.resources`: the **per-replica request shape** the
  offering serves (one Pod's request), operator-declared — distinct from
  `allocatable`. See [Pricing & interruption](/providers/oracle-cloud/pricing-and-interruption/)
  and the note below.

### `resources` vs `allocatable`

`resources` is the per-replica request shape (e.g. `{cpu:"1", memory:"2Gi"}`).
`allocatable` is the machine's full hardware capacity, derived from the shape
(plus the flex OCPU/memory). The shard computes `density = floor(allocatable /
resources)`, so the two **must differ** to pack more than one Pod per machine —
never set them equal.

The OCPU→vCPU convention: x86 shapes expose **2 vCPU per OCPU** (hyperthreading),
Ampere (A1/A2) shapes **1 vCPU per OCPU**. So a `VM.Standard.E5.Flex` with 2
OCPUs reports `allocatable.cpu = 4`; a `VM.Standard.A1.Flex` with 2 OCPUs reports
`cpu = 2`.

## Bootstrap hook contract

OCI cloud-init `user_data` runs only at **first boot**, so it carries the generic
`--base-user-data` baked in at launch. The **cluster-specific** bootstrap blob is
delivered later by `Configure` over the **Oracle Cloud Agent Run Command**: the
provider writes the blob to `<bootstrap-hook>.blob` and runs

```
<bootstrap-hook> <cluster-id>
```

Your base image must ship that executable and run the Oracle Cloud Agent with the
**Run Command** plugin enabled. The hook joins the node to the cluster using the
blob (opaque kubelet-join data — never parsed by the provider). `Drain` runs a
`kubectl cordon`/`drain` via the same Run Command channel, bounded by the grace
period.

> **Blob size.** A Run Command's inline text is capped (~4 KB), so a bootstrap
> blob that fits is delivered in one command; a larger one is streamed to
> `<bootstrap-hook>.blob.b64` in bounded base64 chunks and decoded on-host before
> the hook runs. Very large blobs (beyond a couple dozen chunks) are rejected with
> a clear error — stage those out-of-band and have the hook fetch them. Keep the
> join blob small where you can.

> **Node-name assumption.** The drain script resolves the Kubernetes node to
> cordon/drain from the host's own `hostname -f` (falling back to `hostname`).
> This is correct when the kubelet registers the node under that name (the OKE /
> default convention). If you run the kubelet with a `--hostname-override` or a
> custom DNS scheme so the registered node name differs, ensure the base image's
> `hostname` resolves to the registered node name (e.g. set it in the
> `--base-user-data` cloud-init), or the drain will target the wrong node.
