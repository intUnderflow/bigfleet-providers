---
title: Oracle Cloud (OCI) provider
description: Provision OCI Compute capacity — on-demand, preemptible (spot), and bare metal — for your BigFleet fleet. Deploy one process per region with the Helm chart and container image, scaled in and out automatically.
sidebar:
  order: 0
  label: OCI overview
---

The **Oracle Cloud Infrastructure (OCI) provider** gives your BigFleet fleet
machines to run on. When BigFleet decides your clusters need more capacity, the
provider launches OCI compute instances; when the fleet scales in, it drains and
terminates them. You point it at your tenancy's compartment, a subnet, and a base
image, and it provisions **on-demand, preemptible (spot), and bare-metal**
capacity automatically — no manual instance management, no node-pool babysitting.

You run **one process per region**, next to BigFleet. Each process owns a single
region + compartment's capacity, and BigFleet dials it to request, configure,
drain, and delete machines as demand moves.

## How it behaves

- **Hardened and operable.** It ships as a container image and a Helm chart, runs
  non-root on a distroless, read-only root filesystem, and exposes
  liveness/readiness probes, Prometheus metrics, and structured logs. See
  [Observability](/providers/oracle-cloud/observability/).
- **Certified.** It passes the full BigFleet provider conformance program —
  [92 certified behaviors](/conformance/) — credential-free on every change, plus
  an extension suite that asserts stronger invariants. See
  [Certification](/providers/oracle-cloud/certification/).
- **Conservative by default.** A `Create` blocks until the instance is actually
  RUNNING, preemptible machines always carry a real interruption risk (never a
  falsely-cheap zero), and a failed bootstrap or drain surfaces as a hard failure
  rather than a silently-broken node. Capacity it doesn't own, it doesn't touch.

## What you need

To run it against a real region, have these ready (the
[Credentials & auth](/providers/oracle-cloud/credentials/) page walks through the
identity setup):

- **An OCI tenancy**, a **compartment**, and the **region** you want capacity in
  (one process per region).
- **A subnet** (in a VCN) for the provider to launch instances into.
- **A base image** (OCID) that joins your cluster and runs the **Oracle Cloud
  Agent** with the Run Command plugin enabled. The provider launches it, then
  delivers a per-cluster bootstrap blob over Run Command and runs a small hook
  your image ships. The hook contract is in
  [Configuration](/providers/oracle-cloud/configuration/).
- **An identity** the provider runs as: an **Instance Principal** (on an OCI
  instance), **OKE Workload Identity** (as an OKE pod), or a **config-file /
  API-key**. A **dynamic group + IAM policy** grants it least-privilege Compute
  permissions scoped to one compartment — with ready-to-apply Terraform on the
  [Credentials & auth](/providers/oracle-cloud/credentials/) page.

## Deploy it

The provider is a published container image plus a Helm chart — you don't build
from source. The path is:

1. **Authorize the provider** — provision the dynamic group + IAM policy for the
   Instance-Principal / Workload-Identity path (or mount an `~/.oci/config`
   Secret). See [Credentials & auth](/providers/oracle-cloud/credentials/).
2. **Install the Helm chart, one release per region**, pointing it at your region,
   compartment, subnet, base image, and your **offerings** (the quota of capacity
   it may provision). Enable durable state on a PersistentVolume so bindings
   survive restarts.

See [Install & deploy](/providers/oracle-cloud/install/) for the full walkthrough.
