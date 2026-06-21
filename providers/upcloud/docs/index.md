---
title: UpCloud provider
description: Provision UpCloud cloud-server capacity for your BigFleet fleet. Deploy one process per zone with the Helm chart and container image, scaled in and out automatically.
sidebar:
  order: 0
  label: UpCloud overview
---

The **UpCloud provider** gives your BigFleet fleet machines to run on. When
BigFleet decides your clusters need more capacity, the provider creates UpCloud
cloud servers; when the fleet scales in, it drains and deletes them. You point it
at an UpCloud zone and an OS template, and it provisions capacity
automatically — no manual server management, no node-pool babysitting.

You run **one process per zone** (UpCloud's `fi-hel1`, `fi-hel2`, `de-fra1`,
`nl-ams1`, `uk-lon1`, `us-nyc1`, `us-chi1`, `sg-sin1`, …), next to BigFleet. Each
process owns a single zone's capacity, and BigFleet dials it to request,
configure, drain, and delete machines as demand moves.

## What you need

To run it against a real UpCloud account, have these ready (the
[Credentials](credentials.md) page walks through the API sub-account):

- **An UpCloud account** and the zone you want capacity in (one process per
  zone).
- **A dedicated API sub-account**, created in the UpCloud Control Panel's
  *People* page, scoped to API access only. It authenticates with HTTP Basic auth
  (a username + password read from `UPCLOUD_USERNAME` / `UPCLOUD_PASSWORD`). There
  is **no IAM/role model on UpCloud** — the sub-account is the entire
  authorisation surface. See [Credentials](credentials.md).
- **An OS template** to clone (`--template`, an UpCloud storage UUID, e.g. an
  Ubuntu 24.04 cloud-init template). The provider clones it into each server's OS
  disk at create, with a generic pre-binding cloud-init that installs an
  **on-host bootstrap hook**.
- **An SSH key pair** the provider uses to deliver the per-cluster bootstrap blob
  to a running server over SSH. The flags are in [Install & deploy](install.md)
  and [Configuration](configuration.md).

## How it behaves

- **Hardened and operable.** It ships as a container image and a Helm chart,
  runs non-root on a distroless, read-only root filesystem, and exposes
  liveness/readiness probes, Prometheus metrics, and structured logs. See
  [Observability](observability.md).
- **Certified.** It passes the BigFleet provider conformance program
  credential-free on every change, plus an extension suite that asserts stronger
  invariants. See [Certification](certification.md).
- **Conservative by default.** A `Create` blocks until the server is actually
  `started`, the price you see is a published UpCloud rate converted to USD, and a
  failed bootstrap or drain surfaces as a hard failure rather than a
  silently-broken node. Capacity it doesn't own, it never touches. A `Delete`
  removes the server **and its storage**, so it never leaks the separately-billed
  OS disk.

## How it works (briefly)

The provider implements the substrate-specific half of a provider; a shared
`providerkit` wraps it with the full `bigfleet.v1alpha1.CapacityProvider` gRPC
contract — fencing of stale shards, idempotent retries, async dispatch,
transition timeouts, and field-shape. You operate the substrate side (zones,
templates, offerings, SSH delivery); the contract behaviours are handled for you
and need no tuning.

## Deploy it

The provider is a published container image plus a Helm chart — you don't build
from source. The path is:

1. **Create the API sub-account** in the UpCloud Control Panel's *People* page,
   scope it to API access only, and store its username + password as a Kubernetes
   Secret. The [Credentials](credentials.md) page has the exact steps.
2. **Install the Helm chart, one release per zone**, pointing it at your zone, OS
   template, the SSH key pair, and your **offerings** (the quota of capacity it
   may provision). Enable durable state on a PersistentVolume so bindings survive
   restarts.

From here, see [Install & deploy](install.md) for the full path,
[Configuration](configuration.md) for offerings, pricing, and the
create-then-bootstrap model, and [Credentials](credentials.md) for the API
sub-account and why UpCloud has no IAM/role chain to provision.
