---
title: DigitalOcean provider
description: Provision DigitalOcean Droplet capacity for your BigFleet fleet. Deploy one process per region with the Helm chart and container image, scaled in and out automatically.
sidebar:
  order: 0
  label: DigitalOcean overview
---

The **DigitalOcean provider** gives your BigFleet fleet machines to run on. When
BigFleet decides your clusters need more capacity, the provider creates
DigitalOcean Droplets; when the fleet scales in, it drains and deletes them. You
point it at a DigitalOcean region and a base image, and it provisions capacity
automatically — no manual Droplet management, no node-pool babysitting.

You run **one process per region** (DigitalOcean's `nyc3`, `sfo3`, `fra1`,
`lon1`, `sgp1`, …), next to BigFleet. Each process owns a single region's
capacity, and BigFleet dials it to request, configure, drain, and delete
machines as demand moves.

## Why you'd trust it in production

- **Production-ready.** It ships as a hardened container image and a Helm chart,
  runs non-root on a distroless, read-only root filesystem, and exposes
  liveness/readiness probes, Prometheus metrics, and structured logs. See
  [Observability](observability.md).
- **Certified.** It passes the full BigFleet provider conformance program —
  [92 certified behaviors](https://github.com/intUnderflow/bigfleet-providers/tree/main/conformance/docs/conformance.md) —
  credential-free on every change, plus an extension suite that asserts stronger
  invariants. See [Certification](certification.md).
- **Correct by construction.** A `Create` blocks until the Droplet is actually
  `active`, the price you see is the real published DigitalOcean rate in USD, and
  a failed bootstrap or drain surfaces as a hard failure rather than a
  silently-broken node. Capacity it doesn't own, it never touches.

## What you need

To run it against a real DigitalOcean account, have these ready (the
[Credentials](credentials.md) page walks through the token):

- **A DigitalOcean account** and the region you want capacity in (one process
  per region).
- **A Personal Access Token (PAT)** scoped to **read + write on Droplets** (plus
  the Sizes/Tags catalogue the provider reads). See [Credentials](credentials.md).
- **A base image** that joins your cluster. The provider creates a Droplet from
  it with a generic pre-binding cloud-init that installs an **on-host agent**,
  then delivers a per-cluster bootstrap blob to that agent over a
  mutually-authenticated TLS channel. The agent contract is in
  [Configuration](configuration.md).
- **A bootstrap channel** the provider serves over TLS (its own server
  certificate) and that the Droplets can reach. The on-host agent fetches its
  cluster-join blob from it, pinning the provider's CA. The flags are in
  [Install & deploy](install.md) and [Configuration](configuration.md).

There is **no IAM/role chain on DigitalOcean** — a single PAT is the entire
authorisation surface, so there are no roles, policies, or instance profiles to
provision, and **no separate node identity**. That is the one structural
difference from a hyperscaler provider (AWS, for example, runs two identities:
a provider role and a node instance profile). The
[Credentials](credentials.md) page draws that contrast out.

## Deploy it

The provider is a published container image plus a Helm chart — you don't build
from source. The path is:

1. **Mint a PAT scoped to read + write on Droplets** in the DigitalOcean control
   panel (or with `doctl`) and store it as a Kubernetes Secret. The
   [Credentials](credentials.md) page has the exact steps.
2. **Install the Helm chart, one release per region**, pointing it at your
   region, base image, the bootstrap channel, and your **offerings** (the quota
   of capacity it may provision). Enable durable state on a PersistentVolume so
   bindings survive restarts.

From here, see [Install & deploy](install.md) for the full path,
[Configuration](configuration.md) for offerings and the bootstrap model, and
[Credentials](credentials.md) for the token and why DigitalOcean's single-token
model differs from AWS's two-identity IAM.
