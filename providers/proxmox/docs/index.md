---
title: Proxmox VE provider
description: Provision Proxmox VE virtual machines for your BigFleet fleet — on-demand QEMU/KVM VMs on your own cluster. Deploy one process per Proxmox cluster with the Helm chart and container image.
sidebar:
  order: 0
  label: Proxmox overview
---

The **Proxmox VE provider** gives your BigFleet fleet machines to run on, on your
own Proxmox cluster — no cloud account. When BigFleet decides your clusters need
more capacity, the provider clones a VM from a template; when the fleet scales
in, it drains and destroys it. You point it at your Proxmox cluster's API, your
nodes, and a source template, and it provisions VMs automatically — no manual
VMID juggling, no by-hand clones.

You run **one process per Proxmox cluster**, next to BigFleet. Each process owns
a single cluster's capacity, and BigFleet dials it to request, configure, drain,
and delete machines as demand moves. A **machine** is one Proxmox qemu VM (a
single VMID on one cluster node); a BigFleet **zone** is a Proxmox cluster node;
an **instance type** is a catalog entry naming a source template and the clone's
vCPU/memory.

## How it behaves

- **Ships as a deployable.** It is a container image and a Helm chart, runs
  non-root on a distroless, read-only root filesystem, and exposes
  liveness/readiness probes, Prometheus metrics, and structured logs. See
  [Observability](/providers/proxmox/observability/).
- **Certified.** It passes the BigFleet provider conformance program —
  [92 behaviors across 11 areas](/conformance/) — credential-free on every
  change, for the **core** and **cloud** profiles. See
  [Certification](/providers/proxmox/certification/).
- **On-demand only.** Proxmox VMs are not preemptible: every machine is
  `ON_DEMAND` and reports an `interruption_probability` of exactly `0`. There is
  no spot market and no `SPOT` capacity. `Delete` stops and destroys the VM and
  its disks, so the **cloud** profile applies.
- **Verified TLS, always.** The provider reaches the Proxmox API over HTTPS with
  TLS verification that cannot be disabled — anchored on your cluster CA or a
  pinned certificate fingerprint. The cluster-join secret rides that channel, so
  it must be verified. See [Credentials](/providers/proxmox/credentials/) and
  [Security](/providers/proxmox/security/).

## What you need

To run it against a real cluster, have these ready:

- **A Proxmox VE cluster** and its API endpoint (`https://host:8006/api2/json`),
  plus the node names you want capacity on (one process per cluster).
- **A least-privilege API token** (`USER@REALM!TOKENID=SECRET`) scoped to a
  dedicated resource pool. The [Credentials](/providers/proxmox/credentials/)
  page walks through the `pveum` setup.
- **TLS trust material** for the Proxmox API cert: your cluster CA
  (`/etc/pve/pve-root-ca.pem`) or the cert's SHA-256 fingerprint. There is no
  skip-verify option.
- **A prepared template VM** that joins your cluster. It must have
  `qemu-guest-agent` installed and enabled and `kubelet` preinstalled; the
  provider delivers the per-cluster bootstrap over the guest agent at Configure
  time. The template contract is in
  [Configuration](/providers/proxmox/configuration/).

## Deploy it

The provider is a container image plus a Helm chart. The path is:

1. **Create the API token** and a resource pool with a least-privilege role,
   then stage your template. The [Credentials](/providers/proxmox/credentials/)
   page has the `pveum` commands; [Configuration](/providers/proxmox/configuration/)
   has the template contract.
2. **Install the Helm chart, one release per cluster**, pointing it at your API
   URL, token, CA/fingerprint, nodes, template, and your **offerings** (the quota
   of capacity it may provision). Enable durable state on a PersistentVolume so
   bindings survive restarts.

See [Install & deploy](/providers/proxmox/install/) for the full path, and
[Configuration](/providers/proxmox/configuration/) for every flag and the
offerings schema.

If you just want to try it with no Proxmox cluster, the provider's in-memory
**fake** backend stands up with no credentials — a bare run with no
`--proxmox-api-url` comes up on the fake, which is how the credential-free
[certification](/providers/proxmox/certification/) run works.
