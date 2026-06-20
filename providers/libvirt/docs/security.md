---
title: Security
description: mTLS on the gRPC port, the libvirt connection trust model, the cloud-init / guest-agent bootstrap path, and how the process is exposed for the BigFleet libvirt provider.
sidebar:
  order: 5
  label: Security
---

The libvirt provider sits on the trust boundary between BigFleet's control plane
and your libvirt hosts: it accepts lifecycle RPCs over the network and turns them
into domain define/start/destroy, storage operations, and in-guest commands. This
page covers the four things an operator must get right — the gRPC transport
(mTLS), the libvirt connection identity, the bootstrap trust model, and how the
process is exposed.

## Transport: mTLS on the gRPC port

The `CapacityProvider` gRPC service, the `grpc.health.v1` health service, and
(optionally) reflection all share `--addr` (default `:9000`). Secure it with the
TLS flags:

```sh
./bin/libvirt --provider libvirt-dc1 --connect 'rack1=qemu+ssh://bigfleet@host-a/system' \
              --image ubuntu-24.04.qcow2 \
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
  startup error.
- `--tls-ca` without a cert/key is rejected; a CA only makes sense once the server
  itself has a cert.
- When `--tls-ca` is set, client auth is `RequireAndVerifyClientCert`: a missing or
  untrusted client certificate is refused at the TLS layer, before any RPC handler
  runs.
- The server pins **TLS 1.3** (`MinVersion`). Make sure BigFleet's client and any
  debugging tooling can negotiate 1.3.
- A bad keypair or an unparseable CA bundle fails the process at startup rather
  than degrading silently, so a misconfigured cert can never come up insecure.

This gRPC mTLS is **separate** from the `qemu+tls://` libvirt connection — they are
different certificates for different boundaries (see
[Credentials](/providers/libvirt/credentials/)).

## The libvirt connection identity

Authorisation to libvirt is the **connection** — there is no token. Consequences:

- The connecting identity (an SSH key for a `libvirt`-group user, or a TLS client
  cert on the `tls_allowed_dn_list`) can manage domains on the host. **Scope it**:
  a dedicated non-root user, a polkit rule limited to domain management, and a
  storage pool / network reserved for BigFleet-managed capacity. The
  [host-setup](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/libvirt/deploy/host-setup)
  directory ships the polkit rule and steps.
- Store the SSH key / client cert as a Kubernetes Secret mounted read-only, never
  in args or the image. Full minting / storage / rotation flow is on the
  [Credentials](/providers/libvirt/credentials/) page.
- Pin `known_hosts` (SSH) or the server CA (TLS) so the provider verifies the host
  it connects to — no trust-on-first-use.
- No credential is ever logged.

## The bootstrap trust model

Configure and Drain reach the guest through the **qemu guest agent** channel — no
SSH into the VM, no inbound port on the guest. The model:

- The provider regenerates the domain's cloud-init NoCloud datasource with the
  opaque `bootstrap_blob` as user-data, and runs the image's hook
  (`/opt/bigfleet/bootstrap <cluster-id>`) via `guest-exec`. The blob is delivered
  base64-encoded and decoded in-guest; the provider **never parses it**.
- The cluster binding is recorded in the domain's libvirt metadata only **after**
  the bootstrap hook succeeds, so a failed Configure never leaves a domain tagged
  as bound to a cluster it never joined.
- The guest agent runs over a host-local virtio channel (not the network), so the
  bootstrap material never crosses a network the host doesn't control — there is no
  on-path window to capture the cluster-join secret the way there would be for an
  SSH-to-guest delivery.

For defence in depth, keep the libvirt management network (the `qemu+ssh://` /
`qemu+tls://` path) private to the control plane.

## Exposure

Run the provider with `replicas: 1` per host-set, reachable only by BigFleet:

- Keep the gRPC `--addr` on a `ClusterIP` Service inside the mesh/namespace, not a
  `LoadBalancer`. If you terminate TLS at a mesh sidecar instead of in the
  provider, leave the provider `insecure` but ensure the port is never reachable
  outside the mesh.
- The metrics/health port (`--metrics-addr`) serves no secrets, but scope it to
  your Prometheus and kubelet probes all the same.
- The pod runs non-root (uid 65532) on a read-only root filesystem with all
  capabilities dropped (the chart's hardened defaults match the distroless image).
  The libvirt connection is outbound (SSH/TLS) or a bind-mounted socket — the pod
  needs no extra host privileges.
