---
title: BigFleet Providers
description: Capacity providers you deploy alongside BigFleet to provision and reclaim the machines that run your fleet — production-ready and conformance-certified.
template: splash
hero:
  tagline: Capacity providers you deploy alongside BigFleet to provision and reclaim the machines under your fleet — production-ready and conformance-certified.
  actions:
    - text: Get started with AWS
      link: /providers/aws/
      icon: right-arrow
      variant: primary
    - text: Why you can trust it
      link: /conformance/
      icon: document
      variant: secondary
    - text: GitHub
      link: https://github.com/intUnderflow/bigfleet-providers
      icon: external
      variant: minimal
---

## What you get

You already run [BigFleet](https://bigfleet.lucy.sh) to autoscale your fleet. BigFleet decides *which machines* your Kubernetes clusters need — but it doesn't touch your cloud account. A **capacity provider** is the piece that does: you deploy it next to BigFleet, point it at your substrate, and it provisions, configures, drains, and reclaims real machines automatically as your fleet's demand moves.

You run it as a container, ship it with a Helm chart, give it scoped credentials, and BigFleet dials it. From then on, machines appear and disappear to match your fleet — no glue scripts, no manual capacity ops.

## Providers

### AWS EC2 — available and certified

The **[AWS EC2 provider](/providers/aws/)** is a complete, production-ready provider: a container image and a Helm chart you deploy onto EKS (or anywhere it can reach AWS).

- **On-demand, spot, and reserved** capacity. Spot machines always carry a real, non-zero interruption probability, forecast from the Spot Instance Advisor and raised the moment AWS signals an interruption.
- **Deploy it the way you already work** — pull the container image, install the [Helm chart](/providers/aws/install/) one release per region, and authenticate with IRSA. No static keys, no hardcoded credentials.
- **You provide the substrate** — an AWS account, a base AMI, your subnets and security groups, and a [least-privilege IAM role](/providers/aws/iam/) (Terraform included). The provider does the rest.
- **Built to be operated** — Prometheus metrics, `/healthz` and `/readyz` probes, structured logs, optional mTLS, and durable state on a PersistentVolume so it recovers cleanly on restart.

Start with the **[AWS overview](/providers/aws/)**, then **[Install & deploy](/providers/aws/install/)**.

**More substrates are coming.** GCP, libvirt, bare metal, and others are added the same way and held to the same bar.

## Why you can trust it

Every provider here is certified by *passing the same suite* — no exceptions, no self-attestation. The AWS provider clears the upstream authoritative baseline **plus** an extension suite, certified against **92 conformance behaviors across 11 areas** with no failures. That covers the things you'd otherwise have to take on faith: a failed bootstrap or drain becomes a visible failure rather than a silent false "ready"; launches are idempotent; inventory stays consistent.

See the **[conformance program](/conformance/)** for what's checked, and the AWS provider's **[certification page](/providers/aws/certification/)** for how to reproduce the verdict yourself.

## How it works (in one line)

BigFleet is the client; each provider is a gRPC server it dials to create, configure, drain, and delete machines. You don't write any of that — you deploy a provider and configure it.

## Building a provider?

If you're adding support for a new substrate rather than operating one, the authoring path lives a layer down — every provider is built on a shared library that gets fencing, idempotency, and the machine contract right once, so you only write substrate-specific logic.

- The [provider author guide](https://bigfleet.lucy.sh/provider-author-guide/) — the spine for new providers.
- [CONTRIBUTING.md](https://github.com/intUnderflow/bigfleet-providers/blob/main/CONTRIBUTING.md) — the step-by-step recipe.
- The [conformance program](/conformance/) — the behavior registry your provider must pass.
