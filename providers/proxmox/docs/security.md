---
title: Security
description: gRPC mTLS, the mandatory Proxmox API TLS verification, the least-privilege API token, and the guest-agent bootstrap trust model for the BigFleet Proxmox VE provider.
sidebar:
  order: 5
  label: Security
---

The Proxmox provider sits on the trust boundary between BigFleet's control plane
and your Proxmox cluster: it accepts lifecycle RPCs over the network and turns
them into clones, power operations, guest-agent file-writes and execs, and VM
destroys. This page covers the things an operator must get right — the gRPC
transport (mTLS), the **mandatory** Proxmox API TLS verification, the API token
the process holds, the guest-agent bootstrap trust model, and how the process is
exposed.

## Transport: mTLS on the gRPC port

The `CapacityProvider` gRPC service, the `grpc.health.v1` health service, and
(optionally) reflection all share `--addr` (default `:9000`). Secure it with the
TLS flags:

```sh
./bin/proxmox --provider proxmox-dc1 \
          --proxmox-api-url https://pve1:8006/api2/json \
          --proxmox-ca-file /etc/pve/pve-root-ca.pem \
          --tls-cert server.pem --tls-key server-key.pem \
          --tls-ca client-ca.pem
```

The flags compose into three modes (logged at startup as the `security` field):

| Mode | Flags | Behaviour |
|---|---|---|
| `insecure` | none of `--tls-cert`/`--tls-key` | Plaintext. Acceptable only for trusted in-cluster traffic or the fake backend. |
| `TLS` | `--tls-cert` + `--tls-key` | Server presents a cert; clients are not authenticated. |
| `mTLS` | `--tls-cert` + `--tls-key` + `--tls-ca` | Server presents a cert **and** requires a client cert chaining to `--tls-ca`. Use this in production. |

Notes from the implementation, so you do not fight the validation:

- `--tls-cert` and `--tls-key` are required together — supplying only one is a
  startup error (`both --tls-cert and --tls-key are required to enable TLS`).
- `--tls-ca` without a cert/key is rejected (`--tls-ca set without
  --tls-cert/--tls-key`); a CA only makes sense once the server has a cert.
- When `--tls-ca` is set, client auth is `RequireAndVerifyClientCert`: a missing
  or untrusted client certificate is refused at the TLS layer, before any RPC
  handler runs.
- The server pins **TLS 1.3** (`MinVersion`). Make sure BigFleet's client and any
  debugging tooling (`grpcurl`) can negotiate 1.3.
- A bad keypair or an unparseable CA bundle fails the process at startup rather
  than degrading silently, so a misconfigured cert can never come up insecure.

The keypair and CA are read once, at startup, so the process does **not** hot-
reload a rotated cert. To roll a certificate, restart the process after the new
PEM is in place (a Deployment rollout when cert-manager rewrites the Secret).
Because the persisted `--state` file is the restart path, a rolling restart is
safe; drain in-flight transitions first if you want zero `FAILED` churn.

If you terminate TLS at a mesh sidecar (Istio/Linkerd) instead, leave the gRPC
listener `insecure` and let the mesh enforce mTLS between pods — but then the
provider port must never be reachable outside the mesh (see
[network exposure](#network-exposure)).

## The Proxmox API connection is always verified

This is the most important property to internalise: the TLS connection to the
Proxmox API is the **secret channel**. The bootstrap join secret is delivered to
the guest over the qemu guest agent, and the guest-agent file-write/exec ride this
same TLS-protected, token-authenticated API. So the API TLS connection **must** be
verified, and the provider gives you no way to skip it — there is deliberately no
`InsecureSkipVerify` path anywhere in the code.

Verification is anchored on one of two operator-supplied inputs, and **at least
one is required** — startup fails with neither:

- **`--proxmox-ca-file`** — the Proxmox cluster CA (`/etc/pve/pve-root-ca.pem`).
  The API cert is verified to chain to it (standard chain + hostname check). A
  self-signed cluster cert is trusted by trusting its CA, not by skipping
  verification.
- **`--proxmox-tls-fingerprint`** — a pinned SHA-256 fingerprint of the API leaf
  cert. The provider accepts only the exact cert whose fingerprint matches; an
  unexpected cert is rejected. This is verification by pinning.

Set both and the cert must chain to the CA **and** match the fingerprint. The
exact `pveum`/cert steps are on the [Credentials](/providers/proxmox/credentials/)
page; this section is the rationale: an unverified API channel would let a
man-in-the-middle observe or substitute the bootstrap secret, so the provider
refuses to run without trust material.

## Least-privilege API token

The provider authenticates to the Proxmox API with an API token
(`USER@REALM!TOKENID=SECRET`), sent as the `Authorization: PVEAPIToken=...`
header. The full copy-pasteable `pveum` setup lives in
[Credentials](/providers/proxmox/credentials/); this section is the security
rationale.

- **Dedicated user + custom role.** Create a dedicated `bigfleet@pve` user and a
  custom role carrying only the privileges the code calls — VM clone/config/power
  management, guest-agent access, datastore allocation, and the audit reads for
  inventory. Do not reuse `root@pam` or grant `Administrator`.
- **Scope to a resource pool.** Grant the role on a single resource pool
  (`/pool/bigfleet`, via `--proxmox-pool`) so the token can only act on VMs this
  provider clones into that pool — not on other tenants' VMs and not on the
  cluster at large.
- **Least-privilege token, scoped to a pool.** The token's authority comes from a
  dedicated user whose ACL is granted only on the managed resource pool (and
  audit-only on `/nodes`), so it can act on nothing else. The shipped setup
  (`deploy/host-setup/setup-token.sh`) creates the token with `--privsep 0`, so
  it inherits that pool-scoped user ACL. (Do **not** use `--privsep 1` with this
  setup: a privilege-separated token starts with an empty ACL of its own, so
  every API call would 403 — grant the role on the token directly if you want
  privilege separation.)
- **Deliver the secret from a file.** Use `--proxmox-token-file` (the chart mounts
  a Secret) so the secret never appears in a process arg list. The secret is never
  logged.

## The guest-agent bootstrap trust model

Configure and Drain are delivered over the **qemu guest agent** through the
verified, token-authenticated Proxmox API — there is no inbound SSH path to the
VM. The provider writes the blob into the guest (agent file-write) and runs the
in-image hook (agent exec), waiting for it to exit; a non-zero exit becomes a
`FAILED` transition rather than a false Configured/Idle. The trust properties that
matter:

- **The bootstrap blob is opaque to the provider.** On Configure the provider
  writes the `bootstrap_blob` it was handed to `--bootstrap-path`
  (`/run/bigfleet-bootstrap`) in the guest and runs `--bootstrap-exec <path>`. The
  provider never parses, logs, or persists the blob's bytes — the in-image hook is
  the only thing that consumes it. Treat the blob as a credential: it is the
  kubelet join secret, so it joins a node to a cluster.
- **It rides the guest agent, never cloud-init.** Cloud-init/user-data is
  first-boot-only and not a confidential per-Configure channel, so the per-cluster
  secret is never delivered that way. Generic, non-secret pre-binding
  (qemu-guest-agent, kubelet) lives in the template; only the secret rides the
  guest agent at Configure time.
- **The template hook is part of your TCB.** Whatever `--bootstrap-exec` runs
  against `--bootstrap-path` runs inside the guest with the blob as input. Bake the
  hook into the template you control, pin the template (`--template-vmid`), and
  review it the way you would review an init system — a compromised template is a
  compromised node.
- **Drain is real.** Drain runs `kubectl drain` over the guest agent and clears
  the cluster binding; an incomplete drain surfaces as `FAILED`, not a false Idle.
  A node reported drained but still running pods is a correctness and safety
  problem.
- **EnsureRunning before the agent.** Because the agent only answers on a running
  VM, the provider powers a stopped VM on before Configure/Drain. This is a power
  operation on a VM the provider already owns (its own pool), not a broadening of
  scope.

## Network exposure

There is **one process per Proxmox cluster**, and it is meant to be reached only
by BigFleet's control plane, in-cluster:

- **Bind scope.** `--addr` (gRPC) and `--metrics-addr` (`/metrics`, `/healthz`,
  `/readyz`) should both stay on the cluster network. Expose the gRPC port to
  BigFleet via a ClusterIP Service and a `NetworkPolicy` that admits only the
  control plane; never put it behind a public LoadBalancer or Ingress.
- **Reflection.** `--reflection` is on by default for `grpcurl`-based debugging.
  It advertises the service schema to any client that can already reach the port,
  so under mTLS it is low-risk; if the port is reachable more broadly, set
  `--reflection=false`.
- **The metrics port carries no secrets** but does expose operational detail (RPC
  volumes, Proxmox API outcomes, panic counts — see
  [Observability](/providers/proxmox/observability/)). Scope it to your Prometheus
  scraper. `/healthz` and `/readyz` are unauthenticated and are fine to leave open
  to the kubelet.
- **The provider reaches the Proxmox API outbound** on `:8006`. Keep that path on
  a trusted management network; the API token + verified TLS are the controls on
  it.

## Do not commit credentials

- Never bake the API token secret into the image, the template, or a committed
  manifest. Mount it from a Secret and pass `--proxmox-token-file`.
- Keep gRPC TLS private keys (`--tls-key`) and the bootstrap blob out of version
  control. Mount keys from a Secret (or cert-manager); BigFleet supplies the blob
  at Configure time — it is never stored in this provider's config.
- The durable `--state` file holds inventory and bindings, not credentials, but
  treat it as sensitive operational data and keep it off shared/world-readable
  volumes.

See also [Install & deploy](/providers/proxmox/install/) for the pod spec and
[Credentials](/providers/proxmox/credentials/) for the token and TLS setup.
