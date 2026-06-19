---
title: AWS EC2 provider
description: Provision EC2 capacity — on-demand, spot, and reserved — for your BigFleet fleet. Deploy one process per region with the Helm chart and container image on EKS, scaled in and out automatically.
sidebar:
  order: 0
  label: AWS overview
---

The **AWS EC2 provider** gives your BigFleet fleet machines to run on. When
BigFleet decides your clusters need more capacity, the provider launches EC2
instances; when the fleet scales in, it drains and terminates them. You point it
at your AWS account, your subnets, and a base AMI, and it provisions
**on-demand, spot, and reserved** capacity automatically — no manual instance
management, no node-group babysitting.

You run **one process per region**, next to BigFleet. Each process owns a single
region's capacity, and BigFleet dials it to request, configure, drain, and
delete machines as demand moves.

## Why you'd trust it in production

- **Production-ready.** It ships as a hardened container image and a Helm chart,
  runs non-root on a distroless, read-only root filesystem, and exposes
  liveness/readiness probes, Prometheus metrics, and structured logs. See
  [Observability](/providers/aws/observability/).
- **Certified.** It passes the full BigFleet provider conformance program —
  [92 certified behaviors](/conformance/) — credential-free on every change, plus
  an extension suite that asserts stronger invariants. See
  [Certification](/providers/aws/certification/).
- **Correct by construction.** A `Create` blocks until the instance is actually
  running, spot machines always carry a real interruption risk (never a
  falsely-cheap zero), and a failed bootstrap or drain surfaces as a hard
  failure rather than a silently-broken node. Capacity it doesn't own, it never
  touches.

## What you need

To run it against a real region, have these ready (the [IAM](/providers/aws/iam/)
page walks through the roles):

- **An AWS account** and the region you want capacity in (one process per region).
- **A VPC with subnets** — one or more, mapped to availability zones — for the
  provider to launch into, plus the security groups your nodes need.
- **A base AMI** that joins your cluster. The provider launches it, then delivers
  a per-cluster bootstrap blob over SSM and runs a small hook your AMI ships; the
  EKS-optimized AMIs already include the SSM agent. The hook contract is in
  [Configuration](/providers/aws/configuration/).
- **Two IAM identities**: a **provider role** the process runs as (least-privilege
  EC2 + SSM, given to the pod via IRSA on EKS), and a **node instance profile**
  the launched instances run as (it needs `AmazonSSMManagedInstanceCore`). Both,
  with ready-to-apply Terraform, are on the [IAM](/providers/aws/iam/) page.

## Deploy it

The provider is a published container image plus a Helm chart — you don't build
from source. The path on EKS is:

1. **Create the provider role** with IRSA, bound to the chart's ServiceAccount,
   and give your node profile SSM. The [IAM](/providers/aws/iam/) page has the
   exact policy and Terraform.
2. **Install the Helm chart, one release per region**, pointing it at your
   region, AMI, subnets, security groups, node instance profile, and your
   **offerings** (the quota of capacity it may provision). Enable durable state on
   a PersistentVolume so bindings survive restarts.

A minimal install:
