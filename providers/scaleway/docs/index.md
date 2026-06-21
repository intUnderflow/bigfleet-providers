---
title: Scaleway provider
description: Provision Scaleway capacity for your BigFleet fleet. Deploy one process per zone with the Helm chart and container image, scaled in and out automatically.
sidebar:
  order: 0
  label: Scaleway overview
---

The **Scaleway provider** gives your BigFleet fleet machines to run on. When
BigFleet decides your clusters need more capacity, the provider creates Scaleway
servers; when the fleet scales in, it drains and deletes them. You point it at a
Scaleway project, a zone, and a base image, and it provisions capacity
automatically — no manual server management, no node-pool babysitting.

It serves **two substrates**, one per process, selected by `--substrate`:

- **Instances → `ON_DEMAND`** (`--substrate=instances`) — cloud VMs the provider
  can tear down. Implements `Delete`.
- **Elastic Metal → `BARE_METAL`** (`--substrate=elastic-metal`) — physical
  servers returned to a free pool. `Delete` is `Unimplemented`. **The real
  Elastic Metal backend is not yet built:** `--substrate=elastic-metal` with real
  credentials fails fast at startup; it runs on the in-memory fake only (which is
  what the bare-metal conformance profile certifies). Use `instances` for any real
  deployment.

You run **one process per zone** (Scaleway's `fr-par-1`, `nl-ams-1`,
`pl-waw-1`, …), and **one substrate per process**, next to BigFleet. Each process
owns a single zone's capacity for a single substrate, and BigFleet dials it to
request, configure, drain, and delete machines as demand moves.

## How it behaves

- **Hardened and operable.** It ships as a hardened container image and a Helm chart,
  runs non-root on a distroless, read-only root filesystem, and exposes
  liveness/readiness probes, Prometheus metrics, and structured logs. See
  [Observability](/providers/scaleway/observability/).
- **Certified.** It passes the full BigFleet provider conformance program —
  [92 certified behaviors](/conformance/) — credential-free on every change, plus
  an extension suite that asserts stronger invariants. See
  [Certification](/providers/scaleway/certification/).
- **Correct by construction.** A `Create` blocks until the server is actually
  running, the price you see is the real published Scaleway rate converted to USD,
  and a failed bootstrap or drain surfaces as a hard failure rather than a
  silently-broken node. Capacity it doesn't own, it never touches. These
  invariants come from `providerkit`, the shared kit every BigFleet provider wraps.

## What you need

To run it against a real project, have these ready (the
[Credentials](/providers/scaleway/credentials/) page walks through the API key):

- **A Scaleway project** and the zone you want capacity in (one process per zone,
  one substrate per process).
- **An IAM-application API key** — an access key + secret key scoped to that one
  project (`InstancesFullAccess` + `BlockStorageFullAccess` — the latter is
  required so Delete can remove the boot volume; plus `BareMetalFullAccess` for
  Elastic Metal). See [Credentials](/providers/scaleway/credentials/).
- **A base image** that joins your cluster (e.g. `ubuntu_jammy`). The provider
  creates a server from it with a generic pre-binding `user_data`, which installs
  a small on-host agent; at Configure that agent dials the provider's
  mutually-authenticated TLS bootstrap channel, receives the per-cluster bootstrap
  blob, and applies it. The model is in
  [Configuration](/providers/scaleway/configuration/).

Scaleway auth is **API-key based** — an IAM application, a least-privilege policy,
and an API key — not the role/instance-profile model of a hyperscaler. There are
no roles to assume; the access/secret key pair is the entire authorisation surface.

## Deploy it

The provider is a published container image plus a Helm chart — you don't build
from source. The path is:

1. **Create the least-privilege API key** (the Terraform in `deploy/iam/` mints
   the IAM application, policy, and key) and store the access/secret key + project
   id as a Kubernetes Secret. The [Credentials](/providers/scaleway/credentials/)
   page has the exact steps.
2. **Install the Helm chart, one release per zone**, pointing it at your zone,
   substrate, base image, and your **offerings** (the quota of capacity it may
   provision). Enable durable state on a PersistentVolume so bindings survive
   restarts.

From here, see [Install & deploy](/providers/scaleway/install/) for the full path,
[Configuration](/providers/scaleway/configuration/) for offerings and the
bootstrap model, and [Pricing](/providers/scaleway/pricing-and-interruption/) for
the EUR→USD conversion and why interruption probability is a genuine zero.
