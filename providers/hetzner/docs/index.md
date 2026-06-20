---
title: Hetzner Cloud provider
description: Provision Hetzner Cloud capacity for your BigFleet fleet. Deploy one process per location with the Helm chart and container image, scaled in and out automatically.
sidebar:
  order: 0
  label: Hetzner overview
---

The **Hetzner Cloud provider** gives your BigFleet fleet machines to run on. When
BigFleet decides your clusters need more capacity, the provider creates Hetzner
Cloud servers; when the fleet scales in, it drains and deletes them. You point it
at a Hetzner Cloud project and a base image, and it provisions capacity
automatically — no manual server management, no node-pool babysitting.

You run **one process per location** (Hetzner's `nbg1`, `fsn1`, `hel1`, `ash`,
`hil`, …), next to BigFleet. Each process owns a single location's capacity, and
BigFleet dials it to request, configure, drain, and delete machines as demand
moves.

## Why you'd trust it in production

- **Production-ready.** It ships as a hardened container image and a Helm chart,
  runs non-root on a distroless, read-only root filesystem, and exposes
  liveness/readiness probes, Prometheus metrics, and structured logs. See
  [Observability](/providers/hetzner/observability/).
- **Certified.** It passes the full BigFleet provider conformance program —
  [92 certified behaviors](/conformance/) — credential-free on every change, plus
  an extension suite that asserts stronger invariants. See
  [Certification](/providers/hetzner/certification/).
- **Correct by construction.** A `Create` blocks until the server is actually
  running, the price you see is the real published Hetzner rate in USD, and a
  failed bootstrap or drain surfaces as a hard failure rather than a
  silently-broken node. Capacity it doesn't own, it never touches.

## What you need

To run it against a real project, have these ready (the
[Credentials](/providers/hetzner/credentials/) page walks through the token):

- **A Hetzner Cloud project** and the location you want capacity in (one process
  per location).
- **A project-scoped API token** with **Read & Write** (the provider creates and
  deletes servers). See [Credentials](/providers/hetzner/credentials/).
- **A base image** that joins your cluster. The provider creates a server from
  it with a generic pre-binding cloud-init, then delivers a per-cluster bootstrap
  blob over SSH and runs a small hook your image ships. The hook contract is in
  [Configuration](/providers/hetzner/configuration/).
- **An SSH key** the provider uses to deliver the per-cluster bootstrap and to
  cordon/drain (Hetzner Cloud has no in-guest command API). The base image must
  authorise it.

There is **no IAM/role model on Hetzner** — a single project-scoped API token is
the entire authorisation surface, so there are no roles, policies, or instance
profiles to provision. That is the one structural difference from a hyperscaler
provider.

## Deploy it

The provider is a published container image plus a Helm chart — you don't build
from source. The path is:

1. **Mint a project-scoped, Read & Write API token** in the Hetzner Cloud
   Console and store it as a Kubernetes Secret. The
   [Credentials](/providers/hetzner/credentials/) page has the exact steps.
2. **Install the Helm chart, one release per location**, pointing it at your
   location, base image, SSH key, and your **offerings** (the quota of capacity
   it may provision). Enable durable state on a PersistentVolume so bindings
   survive restarts.

From here, see [Install & deploy](/providers/hetzner/install/) for the full path,
[Configuration](/providers/hetzner/configuration/) for offerings and the
bootstrap model, and [Pricing](/providers/hetzner/pricing-and-interruption/) for
the EUR→USD conversion and why interruption probability is a genuine zero.
