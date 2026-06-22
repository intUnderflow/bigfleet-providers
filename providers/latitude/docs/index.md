---
title: Latitude.sh provider
description: Provision Latitude.sh on-demand bare-metal capacity for your BigFleet fleet. Deploy one process per site with the Helm chart and container image, scaled in and out automatically.
sidebar:
  order: 0
  label: Latitude.sh overview
---

The **Latitude.sh provider** gives your BigFleet fleet machines to run on. When
BigFleet decides your clusters need more capacity, the provider deploys physical
Latitude.sh servers; when the fleet scales in, it drains and deprovisions them.
You point it at a Latitude.sh project and an OS image, and it provisions
on-demand bare-metal capacity automatically — no manual server management, no
node-pool babysitting.

You run **one process per site** (Latitude's `ASH`, `NYC`, `LON`, `FRA`, `SGP`,
`SYD`, `TYO`, …), next to BigFleet. Each process owns a single site's capacity,
and BigFleet dials it to request, configure, drain, and delete machines as demand
moves.

## How it behaves

- **Hardened and operable.** It ships as a container image and a Helm chart,
  runs non-root on a distroless, read-only root filesystem, and exposes
  liveness/readiness probes, Prometheus metrics, and structured logs. See
  [Observability](/providers/latitude/observability/).
- **Certified.** It passes the full BigFleet provider conformance program —
  [93 certified behaviors](/conformance/) — credential-free on every change, plus
  an extension suite that asserts stronger invariants. See
  [Certification](/providers/latitude/certification/).
- **Conservative by default.** A `Create` blocks until the bare-metal server is
  actually powered on, the price you see is the real published Latitude.sh hourly
  rate in USD, and a failed bootstrap or drain surfaces as a hard failure rather
  than a silently-broken node. Capacity it doesn't own, it never touches.

## On-demand bare metal, with a real Delete

Latitude.sh is an **on-demand bare-metal cloud**: the provider deploys a physical
box on demand (`POST /servers`) and deprovisions it on demand (`DELETE
/servers/{id}` releases the hardware). That real teardown is the reason the
provider declares **`capacity_type = ON_DEMAND`, not `BARE_METAL`**.

The distinction matters and is load-bearing: since BigFleet M73 the shard only
emits `Delete` for `ON_DEMAND`/`SPOT` capacity. Declaring `BARE_METAL` would stop
the shard ever reclaiming a deployed server — leaking a paid-for physical box
forever. So this provider:

- declares `capacity_type = ON_DEMAND` for every machine (a real `Delete`
  deprovisions the box),
- sets `interruption_probability = 0.0` — a genuine, provider-declared zero
  (Latitude bare metal is not a preemptible/spot market, so the provider never
  reclaims a running server out from under a workload),
- claims the **core** and **cloud** conformance profiles, and **not** `spot` or
  `bare-metal`.

[Pricing & interruption](/providers/latitude/pricing-and-interruption/) explains
why that zero is the *correct* value, and the provider rejects a `spot` or
`bare_metal` `capacity_type` in an offering at startup rather than mis-declaring
the lifecycle.

## What you need

To run it against a real project, have these ready (the
[Credentials](/providers/latitude/credentials/) page walks through the token):

- **A Latitude.sh project** and the site you want capacity in (one process per
  site).
- **A project-scoped API token** plus the **project id/slug** — the provider
  deploys and deprovisions servers. See
  [Credentials](/providers/latitude/credentials/).
- **An OS slug** the deployed server boots (default `ubuntu_22_04_x64_lts`) that
  joins your cluster. The provider deploys a server with a generic pre-binding
  cloud-init, then delivers a per-cluster bootstrap blob over SSH and runs a small
  hook your image ships. The hook contract is in
  [Configuration](/providers/latitude/configuration/).
- **An SSH key** the provider uses to deliver the per-cluster bootstrap and to
  cordon/drain (Latitude.sh has no in-guest command API). The deployed OS must
  authorise it.

There is **no IAM/role model on Latitude.sh** — a single project-scoped API token
plus the project id/slug is the entire authorisation surface, so there are no
roles, policies, or instance profiles to provision. That is the one structural
difference from a hyperscaler provider.

## Deploy it

The provider is a published container image plus a Helm chart — you don't build
from source. The path is:

1. **Mint a project-scoped API token** in the Latitude.sh dashboard and store it
   as a Kubernetes Secret (with the project id/slug). The
   [Credentials](/providers/latitude/credentials/) page has the exact steps.
2. **Install the Helm chart, one release per site**, pointing it at your site,
   OS slug, SSH key, project, and your **offerings** (the quota of capacity it
   may provision). Enable durable state on a PersistentVolume so bindings survive
   restarts.

From here, see [Install & deploy](/providers/latitude/install/) for the full
path, [Configuration](/providers/latitude/configuration/) for offerings and the
bootstrap model, and [Pricing](/providers/latitude/pricing-and-interruption/) for
how price is sourced and why interruption probability is a genuine zero.
