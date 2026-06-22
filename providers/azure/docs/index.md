---
title: Azure provider
description: Provision Azure VM capacity — pay-as-you-go and Spot — for your BigFleet fleet. Deploy one process per region with the Helm chart and container image on AKS, scaled in and out automatically.
sidebar:
  order: 0
  label: Azure overview
---

The **Azure provider** gives your BigFleet fleet machines to run on. When
BigFleet decides your clusters need more capacity, the provider creates Azure
Virtual Machines; when the fleet scales in, it drains and deletes them. You point
it at your subscription, a resource group, and a subnet, and it provisions
**pay-as-you-go and Spot** capacity automatically — no manual VM management, no
VM scale-set babysitting.

You run **one process per region (location)**, next to BigFleet. Each process
owns a single region's capacity, and BigFleet dials it to request, configure,
drain, and delete machines as demand moves.

## How it behaves

- **Hardened and operable.** It ships as a container image and a Helm chart,
  runs non-root on a distroless, read-only root filesystem, and exposes
  liveness/readiness probes, Prometheus metrics, and structured logs. See
  [Observability](/providers/azure/observability/).
- **Certified.** It passes the full BigFleet provider conformance program —
  [93 certified behaviors](/conformance/) — credential-free on every change, plus
  an extension suite that asserts stronger invariants. See
  [Certification](/providers/azure/certification/).
- **Conservative by default.** A `Create` returns only once the VM is actually
  provisioned, Spot machines always carry a real interruption risk (never a
  falsely-cheap zero), and a failed bootstrap or drain surfaces as a hard failure
  rather than a silently-broken node. Capacity it doesn't own, it never touches.

## What you need

To run it against a real region, have these ready (the
[Credentials](/providers/azure/credentials/) page walks through the identity):

- **An Azure subscription** and the region you want capacity in (one process per
  region).
- **A resource group** the provider creates VMs in, and a **VNet/subnet** for it
  to attach NICs to.
- **A base image** that joins your cluster (an image URN like
  `Canonical:ubuntu-24_04-lts:server:latest`, or your own managed image). The
  provider creates the VM, then delivers a per-cluster bootstrap blob via a
  CustomScript extension and runs a small hook your image ships. The hook
  contract is in [Configuration](/providers/azure/configuration/).
- **A managed identity** with a least-privilege role scoped to the resource
  group, federated to the chart's ServiceAccount via Workload Identity on AKS —
  ready-to-apply Terraform is on the [Credentials](/providers/azure/credentials/)
  page.

## Deploy it

The provider is a published container image plus a Helm chart — you don't build
from source. The path on AKS is:

1. **Create the managed identity** with a role scoped to your resource group, and
   federate it to the chart's ServiceAccount. The
   [Credentials](/providers/azure/credentials/) page has the exact role and
   Terraform.
2. **Install the Helm chart, one release per region**, pointing it at your
   location, subscription, resource group, subnet, image, and your **offerings**
   (the quota of capacity it may provision). Enable durable state on a
   PersistentVolume so bindings survive restarts.

A minimal install is on the [Install & deploy](/providers/azure/install/) page.

## Kick the tyres with no Azure account

The provider ships a credential-free **fake backend** that simulates the VM
lifecycle in memory. With no `--location` it comes up on the fake automatically —
this is exactly how `make certify-azure` runs in CI:

```sh
make build-azure
./bin/azure --seed-count 32 --addr :9000 --metrics-addr :9090
# curl localhost:9090/readyz  -> ready
```

Everything in [Install & deploy](/providers/azure/install/) and
[Configuration](/providers/azure/configuration/) is for a real region.
