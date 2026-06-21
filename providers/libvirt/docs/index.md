---
title: libvirt provider
description: Provision QEMU/KVM capacity for your BigFleet fleet on your own libvirt hosts. Deploy one process per host-set with the Helm chart and container image, scaled in and out automatically — no cloud account.
sidebar:
  order: 0
  label: libvirt overview
---

The **libvirt provider** gives your BigFleet fleet machines to run on — **your
own QEMU/KVM hosts**, no cloud account required. When BigFleet decides your
clusters need more capacity, the provider defines and starts libvirt domains
(VMs) from a golden base image; when the fleet scales in, it drains and destroys
them. You point it at one or more libvirt hosts and a base cloud image, and it
provisions capacity automatically — "clone and watch it provision".

You run **one process per deployment** — a set of libvirt hosts, with one
BigFleet **zone** per host — next to BigFleet. Each process owns those hosts'
capacity, and BigFleet dials it to request, configure, drain, and delete machines
as demand moves.

## How it behaves

- **Hardened and operable.** It ships as a hardened container image and a Helm chart,
  runs non-root on a distroless, read-only root filesystem, and exposes
  liveness/readiness probes, Prometheus metrics, and structured logs. See
  [Observability](/providers/libvirt/observability/).
- **Certified.** It passes the full BigFleet provider conformance program —
  [92 certified behaviors](/conformance/) — credential-free on every change, plus
  an extension suite that asserts stronger invariants. See
  [Certification](/providers/libvirt/certification/).
- **Conservative by default.** A `Create` settles to Idle only once the domain is
  actually running, a failed bootstrap or drain surfaces as a hard failure rather
  than a silently-broken node, and capacity it doesn't own it never touches. The
  whole BigFleet contract — fencing, idempotency, async transitions, restart
  recovery — is handled by the shared `providerkit` library, so the provider only
  speaks libvirt.
- **No cloud, pure-Go.** It uses the pure-Go libvirt client, so the image is
  static and CGO-free, and the only thing you provide is libvirt hosts and a base
  image.

## What you need

To run it against real hosts, have these ready (the
[Credentials](/providers/libvirt/credentials/) page walks through the connection):

- **One or more libvirt hosts** (`qemu:///system`, or remote `qemu+libssh://` /
  `qemu+tls://`), each with a storage pool and a network the provider can use.
- **A least-privilege identity** the provider connects as — an SSH key for a
  dedicated `libvirt`-group user, or a libvirt TLS client certificate. There is
  **no API token**; the libvirt connection is the entire auth surface. See
  [Credentials](/providers/libvirt/credentials/).
- **A golden base image** (a cloud image with cloud-init and a kubelet) in the
  storage pool. The provider creates a copy-on-write overlay from it, attaches a
  cloud-init NoCloud datasource, then delivers a per-cluster bootstrap blob and
  runs a small in-image hook. The hook contract is in
  [Configuration](/providers/libvirt/configuration/).

## Deploy it

The provider is a published container image plus a Helm chart — you don't build
from source. The path is:

1. **Prepare each host** — a dedicated least-privilege libvirt user (or TLS
   client allow-list), a storage pool, a network, and the golden base image. The
   [host-side setup](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/libvirt/deploy/host-setup)
   has the polkit rule and the steps.
2. **Store the connection credential as a Secret** (an SSH key or a TLS client
   cert). The [Credentials](/providers/libvirt/credentials/) page has the exact
   steps.
3. **Install the Helm chart, one release per host-set**, pointing it at your
   `--connect` URIs, base image, and your **offerings** (the quota of capacity it
   may provision). Enable durable state on a PersistentVolume so bindings survive
   restarts.

From here, see [Install & deploy](/providers/libvirt/install/) for the full path,
[Configuration](/providers/libvirt/configuration/) for offerings and the
create-then-bootstrap model, and
[Pricing & interruption](/providers/libvirt/pricing-and-interruption/) for the
synthetic cost model and why interruption probability is a genuine zero.

---

### For provider authors

Everything above is what an operator needs. If you are *extending* the provider:
it implements only the small substrate-specific `providerkit.Backend` (create /
configure / drain / delete / describe against libvirt). The cross-cutting
BigFleet contract — the six gRPC RPCs, fencing (`FAILED_PRECONDITION` +
fence-before-idempotency), idempotency on `(machine_id, target_state)`, async
dispatch, transition-timeout→`FAILED`, the `shard_metadata` store/echo/clear
lifecycle, `Machine` field-shape, and `since_revision` — all live in
`providerkit` and must not be re-implemented here. See the repo
[`CONTRIBUTING.md`](https://github.com/intUnderflow/bigfleet-providers/blob/main/CONTRIBUTING.md).
