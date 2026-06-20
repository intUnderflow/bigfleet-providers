---
title: Install & deploy
description: Deploy the BigFleet OCI provider with the published container image and Helm chart — one release per region, hardened non-root, with durable state.
sidebar:
  order: 1
  label: Install & deploy
---

The OCI provider ships as a **container image** and a **Helm chart**. Run **one
release per region**; never scale a release past `replicas: 1` (the provider is
the single, region-scoped `CapacityProvider` process for its region).

## Prerequisites

- A Kubernetes cluster to run the provider in (OKE or anywhere it can reach the
  OCI API and your offerings).
- The provider's **identity** authorized — a dynamic group + IAM policy
  (Instance Principal / Workload Identity) or an `~/.oci/config` Secret. See
  [Credentials & auth](/providers/oracle-cloud/credentials/).
- A **compartment OCID**, a **subnet OCID**, and a **base image OCID** in the
  target region.
- An **offerings** file describing the capacity the provider may provision (see
  [Configuration](/providers/oracle-cloud/configuration/)).

## 1. The image

Pull the published image (or build it from the repo root with
`providers/oracle-cloud/deploy/Dockerfile` — it builds from the repo root so the
multi-module `replace => ../..` resolves `providerkit`):

```
ghcr.io/intunderflow/bigfleet-oracle-cloud:0.1.0
```

It is `distroless/static:nonroot` (uid 65532, no shell, read-only rootfs) and
exposes the gRPC port (`9000`) and the metrics/health port (`9090`).

## 2. Install the chart

One release per region:

```sh
helm install oci-eu-frankfurt-1 providers/oracle-cloud/deploy/helm \
  --namespace bigfleet --create-namespace \
  --set image.tag=0.1.0 \
  --set region=eu-frankfurt-1 \
  --set provider=oci-eu-frankfurt-1 \
  --set oci.compartment=ocid1.compartment.oc1..bbbb \
  --set oci.subnet=ocid1.subnet.oc1..dddd \
  --set oci.image=ocid1.image.oc1..eeee \
  --set oci.auth=instance_principal \
  --set-file offerings.content=offerings.eu-frankfurt-1.json
```

This renders a hardened **Deployment** (`replicas: 1`, `Recreate`, liveness
`/healthz` + readiness `/readyz`), a **Service** exposing `grpc` (BigFleet dials
this) and `metrics`, a **ServiceAccount**, a **ConfigMap** for the offerings, and
optionally a **PVC** for durable state.

## 3. Durable state (recommended)

Persist fence marks, the idempotency map, inventory, and cluster bindings across
restarts by pointing `--state` at a PersistentVolume:

```sh
--set state.enabled=true \
--set state.persistence.enabled=true \
--set state.persistence.size=1Gi
```

Without it the store is in-memory: a restart re-seeds inventory from OCI via
`Describe` (recovered from the `bigfleet-managed` / `bigfleet-machine-id` freeform
tags), but in-flight transitions surface as `FAILED` for the shard to re-drive.

## 4. Point BigFleet at it

BigFleet shards are the gRPC **client**; configure a shard's `--provider-addr`
(Helm `shard.provider.addr`) at this release's `grpc` Service. The provider never
dials the shard.

## Backend selection

`--oci-backend` is `auto` by default: it uses the **real OCI backend** when both
`--region` and `--compartment` are set, and the **in-memory fake** otherwise
(dev / credential-free certification). Force it with `--oci-backend=oci|fake`.

See [Configuration](/providers/oracle-cloud/configuration/) for every flag and the
offerings schema, and the [deploy README](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/oracle-cloud/deploy)
for the Dockerfile, chart, and Terraform.
