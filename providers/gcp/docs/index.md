---
title: GCP (GCE) provider
description: Provision Google Compute Engine capacity for your BigFleet fleet. Deploy one process per region with the Helm chart and container image, scaled in and out automatically.
sidebar:
  order: 0
  label: GCP overview
---

The **GCP provider** gives your BigFleet fleet machines to run on, backed by
**Google Compute Engine (GCE)**. When BigFleet decides your clusters need more
capacity, the provider creates GCE instances; when the fleet scales in, it
drains and deletes them. You point it at a GCP project and a region, and it
provisions capacity automatically — no manual VM management, no node-pool
babysitting.

You run **one process per region** (`us-central1`, `europe-west1`, …), next to
BigFleet. Each process owns a single region's capacity across its zones, and
BigFleet dials it to request, configure, drain, and delete machines as demand
moves.

## Why you'd trust it in production

- **Production-ready.** It ships as a hardened container image and a Helm chart,
  runs non-root on a distroless, read-only root filesystem, and exposes
  liveness/readiness probes, Prometheus metrics, and structured logs. See
  [Observability](/providers/gcp/observability/).
- **Certified.** It passes the full BigFleet provider conformance program —
  [92 certified behaviors](/conformance/) — credential-free on every change, plus
  an extension suite that asserts stronger invariants. See
  [Certification](/providers/gcp/certification/).
- **Correct by construction.** A `Create` blocks until the instance is actually
  `RUNNING`, on-demand and **Spot** capacity are both priced and the Spot
  interruption risk is declared honestly (never a falsely-cheap zero), and a
  failed bootstrap or drain surfaces as a hard failure rather than a
  silently-broken node. Capacity it doesn't own, it never touches.

## What you need

To run it against a real project, have these ready (the
[Credentials](/providers/gcp/credentials/) page walks through the service
account):

- **A GCP project** and the region you want capacity in (one process per region).
- **A provider service account** with least-privilege Compute permissions
  (`roles/compute.instanceAdmin.v1`), obtained via **Workload Identity** on GKE
  (no key files) or a key-file Secret off-GKE. See
  [Credentials](/providers/gcp/credentials/).
- **A boot image** that joins your cluster. The provider creates an instance from
  it with a generic pre-binding `startup-script`, then on Configure overwrites
  `startup-script` with a per-cluster bootstrap blob and resets the instance so
  it joins. The model is in [Configuration](/providers/gcp/configuration/).
- **Your offerings** — the quota of `(machine_type, zone, capacity_type)` shapes
  it may provision.

There are **two identities**, kept separate: the **provider** service account
(the process, which calls `instances.insert/delete/reset` and metadata) and the
service account the **launched instances** run as (`--instance-service-account`).
See [Credentials](/providers/gcp/credentials/).

## Deploy it

The provider is a published container image plus a Helm chart — you don't build
from source. The path is:

1. **Create the provider service account + role binding + Workload-Identity
   binding** (Terraform under
   [`deploy/sa/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/gcp/deploy/sa)).
   The [Credentials](/providers/gcp/credentials/) page has the exact steps.
2. **Install the Helm chart, one release per region**, pointing it at your
   project, region, boot image, and your **offerings**. Enable durable state on a
   PersistentVolume so bindings survive restarts.

From here, see [Install & deploy](/providers/gcp/install/) for the full path,
[Configuration](/providers/gcp/configuration/) for offerings and the bootstrap
model, and [Pricing & interruption](/providers/gcp/pricing-and-interruption/)
for how on-demand and Spot prices and interruption probability are sourced.
