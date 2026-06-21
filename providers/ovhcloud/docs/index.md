---
title: OVHcloud Public Cloud provider
description: Provision OVHcloud Public Cloud (OpenStack) capacity for your BigFleet fleet. Deploy one process per region with the Helm chart and container image; it grows and shrinks your fleet's capacity automatically.
sidebar:
  order: 0
  label: OVHcloud overview
---

The **OVHcloud Public Cloud provider** gives your BigFleet fleet machines to run
on. When BigFleet decides your clusters need more capacity, the provider creates
OVH Public Cloud instances; when the fleet scales in, it drains and deletes them.
You point it at an OVH Public Cloud project (an OpenStack tenant) and a base
image, and it provisions capacity automatically — no manual instance management,
no node-pool babysitting.

You run **one process per region** (OVH's `GRA`, `SBG`, `BHS`, `WAW`, `DE`, `UK`,
`US-EAST`, …), next to BigFleet. Each process owns a single region's capacity,
and BigFleet dials it to request, configure, drain, and delete machines as demand
moves.

OVH Public Cloud is **OpenStack** under the hood, so the provider speaks the Nova
compute API (via gophercloud) — the same contract any OpenStack cloud exposes.

## How it behaves

- **Hardened and operable.** It ships as a container image and a Helm chart,
  runs non-root on a distroless, read-only root filesystem, and exposes
  liveness/readiness probes, Prometheus metrics, and structured logs. See
  [Observability](/providers/ovhcloud/observability/).
- **Certified.** It passes the full BigFleet provider conformance program —
  [92 certified behaviors](/conformance/) — credential-free on every change, plus
  an extension suite that asserts stronger invariants. See
  [Certification](/providers/ovhcloud/certification/).
- **Correct by construction.** A `Create` blocks until the instance is actually
  `ACTIVE`, the secret-bearing bootstrap is delivered over an authenticated,
  host-key-verified channel, and a failed bootstrap or drain surfaces as a hard
  failure rather than a silently-broken node. Capacity it doesn't own, it never
  touches.

## What you need

To run it against a real project, have these ready (the
[Credentials](/providers/ovhcloud/credentials/) page walks through the OpenStack
user):

- **An OVH Public Cloud project** and the region you want capacity in (one
  process per region).
- **An OpenStack user** scoped to that project (`OS_AUTH_URL`, `OS_USERNAME`,
  `OS_PASSWORD`, project id, `OS_REGION_NAME`, Keystone v3), with the project
  `member` role. See [Credentials](/providers/ovhcloud/credentials/).
- **A base image** that joins your cluster. The provider boots an instance from
  it with a generic pre-binding cloud-init, then delivers a per-cluster bootstrap
  blob over SSH and runs a small hook your image ships. The hook contract is in
  [Configuration](/providers/ovhcloud/configuration/).
- **An OpenStack keypair + SSH key** the provider uses to deliver the per-cluster
  bootstrap and to cordon/drain. The keypair injects the public half at create;
  the provider holds the private half.

OVH Public Cloud uses an **OpenStack user**, not an AWS-style IAM role — a
project-scoped Keystone user with the project `member` role is the entire
authorisation surface. That is the one structural difference from a hyperscaler
provider, and the [Credentials](/providers/ovhcloud/credentials/) page is its
analogue of an IAM page.

## Deploy it

The provider is a published container image plus a Helm chart — you don't build
from source. The path is:

1. **Create a project-scoped OpenStack user** (the
   `deploy/openstack/create-scoped-user.sh` helper does this and the keypair) and
   store its OS_* credentials as a Kubernetes Secret. The
   [Credentials](/providers/ovhcloud/credentials/) page has the exact steps.
2. **Install the Helm chart, one release per region**, pointing it at your
   region, base image, keypair, SSH key, and your **offerings** (the quota of
   capacity it may provision). Enable durable state on a PersistentVolume so
   bindings survive restarts.

From here, see [Install & deploy](/providers/ovhcloud/install/) for the full path,
[Configuration](/providers/ovhcloud/configuration/) for offerings and the
bootstrap model, and [Pricing](/providers/ovhcloud/pricing-and-interruption/) for
the EUR→USD conversion and why interruption probability is a genuine zero.
