---
title: Security
description: The security posture of the BigFleet OCI provider — least-privilege IAM, mTLS, a hardened non-root image, and a confidential bootstrap channel.
sidebar:
  order: 6
  label: Security
---

## Identity & least privilege

The provider runs as an OCI principal (Instance Principal, OKE Workload Identity,
or a config-file API key) authorized by a **dynamic group + IAM policy scoped to
one compartment**. The policy grants only what the code calls — `manage
instance-family`, `use volume-family`, `use virtual-network-family`, `read
instance-images`, `use instance-agent-command-family` — and nothing tenancy-wide.
See [Credentials & auth](/providers/oracle-cloud/credentials/).

## Transport: mTLS

BigFleet shards dial the provider's gRPC port. In production, enable **mTLS** so
only authorized shards connect:

```sh
--set tls.enabled=true --set tls.mtls=true --set tls.secretName=bigfleet-oci-tls
```

With `--tls-ca` set, the server **requires and verifies** client certificates
(TLS 1.3 minimum). Plain TLS (`--tls-cert`/`--tls-key` only) or insecure is
acceptable only for trusted in-cluster traffic; insecure is the certify/dev
default.

## Confidential bootstrap delivery

The `Configure` bootstrap blob carries **cluster-join secrets**. The provider
delivers it over the **Oracle Cloud Agent Run Command**, which is authenticated by
**OCI IAM** (the provider's principal) — an authenticated, confidential channel,
the control-plane analogue of AWS SSM `SendCommand`. The provider:

- never writes secrets via instance metadata (`user_data` is first-boot only and
  not confidential for post-create delivery);
- records the `bigfleet-cluster` binding tag **only after** the bootstrap succeeds,
  so a failed Configure never leaves an instance mistagged as joined;
- treats the blob as **opaque** — it is never parsed or logged.

> **Persisted command record.** Like AWS SSM, OCI persists the Run Command record
> (the command text and its captured output) at-rest, scoped to the compartment.
> The bootstrap blob is embedded (base64) in the command text, so it lives in that
> record until OCI ages it out — this is inherent to in-guest command delivery and
> is why the channel is IAM-scoped to the provider's principal. Two operator
> responsibilities follow: keep the compartment's command records access-controlled
> (the same dynamic group / policy that grants `instance-agent-command-family`),
> and ensure your bootstrap **hook never echoes the secret to stdout** — the
> provider writes the blob to a file (not stdout), but the hook's own stdout is
> captured into the command record. Rotate/short-TTL the join material if your
> threat model requires it.

## Outbound: live price list

The price refresher fetches the **public** OCI price list over HTTPS
(`apexapps.oracle.com/.../cetools`, no credentials, read-only) on a timer. It is
unauthenticated and carries no secrets — only product part numbers and USD rates
flow back. If egress is locked down, allow this host (or point `--price-list-url`
at a mirror); a blocked or failing fetch is non-fatal — the provider falls back to
the `prices.yaml` seed and surfaces staleness via metrics. The fake backend uses a
network-free source, so the certify/dev path makes no outbound call.

## Hardened runtime

The image is `gcr.io/distroless/static:nonroot`: no shell, no package manager,
uid 65532. The Helm chart enforces a matching pod security context —
`runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, all
capabilities dropped, `seccompProfile: RuntimeDefault`. Mounted Secrets
(config-file credentials, TLS) are read-only.

## Blast radius

One process owns one region/compartment. It only ever touches instances it
created — `Describe`/reconcile filter on the `bigfleet-managed=true` freeform tag —
so capacity it doesn't own, it never modifies or deletes. `FAILED_PRECONDITION`
is reserved by the kit for fencing, so a stale/zombie shard can never apply a
mutation.
