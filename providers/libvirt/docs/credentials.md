---
title: Credentials & auth
description: How the libvirt provider connects to libvirtd — qemu:///system, qemu+libssh:// (SSH key), qemu+tls:// (client cert) — and how shards authenticate to its gRPC listener. The libvirt analogue of cloud IAM.
sidebar:
  order: 3
  label: Credentials & auth
---

libvirt has **no IAM, no roles, no instance profiles, and no API token**. The
entire authorisation surface is the **libvirt connection itself**: how the
provider's pod reaches each host's `libvirtd`, and the least-privilege identity it
connects as. This is the libvirt equivalent of a hyperscaler's IAM page — there is
no role model to provision, so don't look for one. Instead you choose a transport,
scope a connecting identity, and (separately) secure the provider's own gRPC port.

There are **two distinct trust boundaries**, and they are easy to confuse:

1. **Provider → libvirtd** (this page, §1–3) — how the provider authenticates to
   your hosts.
2. **Shard → provider's gRPC listener** (§4) — how BigFleet authenticates to the
   provider.

## 1. `qemu+libssh://` — SSH transport (the common multi-host model)

The provider connects to each host over SSH (libvirt's pure-Go `libssh`
transport) and talks to the local libvirt socket. You provide an SSH **private
key**; each host authorises the matching **public key** for a dedicated,
least-privilege user.

:::note
Use the **`qemu+libssh://`** scheme, not `qemu+ssh://`. The provider's pinned
pure-Go go-libvirt client only honours the explicit `keyfile` and `known_hosts`
URI parameters on the `libssh` transport — on the plain `ssh` transport it
rejects them ("option invalid with ssh transport, use libssh transport
instead"). `libssh` is also the right fit for a distroless pod: it takes the key
and known-hosts paths you mount explicitly rather than reading a system SSH
config that isn't there. Both transports use the same CGO-free Go SSH dialer, so
the static image is unaffected.
:::

On each host (full steps in
[`deploy/host-setup/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/libvirt/deploy/host-setup)):

- Create a dedicated unprivileged user (e.g. `bigfleet`) in the `libvirt` group —
  **not** root.
- Authorise the provider's public key in that user's `authorized_keys`, ideally
  with a `from="<mgmt-cidr>"` restriction.
- Install the polkit rule that lets that user manage domains through the system
  libvirtd API and nothing else, scoped to the provider's storage pool/network.

Store the private key (and pinned `known_hosts`) as a Secret:

```sh
kubectl -n bigfleet create secret generic bigfleet-libvirt-ssh \
  --from-file=id_ed25519=./id_ed25519 \
  --from-file=known_hosts=<(ssh-keyscan host-a host-b)
```

```yaml
credentials:
  ssh:
    secretName: bigfleet-libvirt-ssh
```

Reference the key and known-hosts file in each `--connect` URI's query parameters
so the transport uses the mounted key and strictly verifies the host:

```
rack1=qemu+libssh://bigfleet@host-a/system?keyfile=/etc/bigfleet/libvirt-ssh/id_ed25519&known_hosts=/etc/bigfleet/libvirt-ssh/known_hosts&known_hosts_verify=normal
```

`known_hosts_verify=normal` (the default) means strict verification against the
pinned `known_hosts` file — an unknown host or a changed host key aborts the
connection. Use a **dedicated** key for the provider, not an operator's personal
key, and pin `known_hosts` so the transport is not trust-on-first-use
(`known_hosts_verify=auto` would trust-on-first-use). The provider **rejects at
startup** any SSH `--connect` URI that disables host-key verification
(`known_hosts_verify=ignore` or `no_verify`), so a misconfiguration can't quietly
open a MITM window on the cluster-join material.

## 2. `qemu+tls://` — libvirt native TLS

The provider connects to libvirtd's TLS port (16514) with a libvirt **client
certificate**. Each host's libvirtd is configured with a `tls_allowed_dn_list`
scoped to that client's DN — the least-privilege boundary.

Issue a libvirt CA, per-host server certs, and one client cert/key for the
provider, then store the client material as a Secret:

```sh
kubectl -n bigfleet create secret generic bigfleet-libvirt-tls \
  --from-file=clientcert.pem=./clientcert.pem \
  --from-file=clientkey.pem=./clientkey.pem \
  --from-file=cacert.pem=./cacert.pem
```

```yaml
credentials:
  tls:
    secretName: bigfleet-libvirt-tls   # mounted where libvirt looks: /etc/pki/libvirt
```

`--connect` URIs are then `rack1=qemu+tls://host-a/system`. The host-side
`tls_allowed_dn_list` and the polkit rule together scope what the client may do.
See [`deploy/host-setup/`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/libvirt/deploy/host-setup)
for the `certtool` and `libvirtd.conf` steps.

## 3. `qemu:///system` — local socket (single host)

A single-host, in-cluster deployment can bind-mount the host's libvirt socket into
the pod (`credentials.hostSocket.enabled=true`, `--connect qemu:///system`). The
pod's uid must map to a host `libvirt`-group user, and the polkit rule still scopes
API access. Simplest, but couples the provider to one host — prefer SSH/TLS for
multi-host.

## 4. How shards authenticate to the provider (gRPC)

Separately from the libvirt connection, secure the provider's own gRPC listener so
only authorised BigFleet shards connect:

- **Production: mTLS.** Provide a server cert/key and a client CA so the provider
  requires and verifies a client certificate. Wire it with `tls.enabled=true`,
  `tls.mtls=true`, and a Secret carrying `tls.crt`, `tls.key`, `ca.crt`.
- **In-cluster trust / local demo: insecure.** Acceptable only when the gRPC port
  is reachable only by BigFleet inside the mesh/namespace.

This is covered in full on [Security](/providers/libvirt/security/) and
[Install & deploy](/providers/libvirt/install/#mtls). Don't confuse it with the
`qemu+tls://` libvirt connection above — they are different certificates for
different boundaries.

## Rotation

- **SSH key / TLS client cert** is read when the provider connects (at startup and
  on reconnect). To rotate: add the new key/cert to the host allow-list, update
  the Secret, and roll the Deployment so the process reconnects with the new
  credential, then remove the old one from the hosts.
- **gRPC server cert** is read once at startup; restart the process after the new
  PEM is in place.

Because the persisted `--state` file is the restart path and transitions run on
minute-scale timeouts, a rolling restart is safe.

## What the connection is used for

Every libvirt operation the provider makes, and the lifecycle step it serves:

| libvirt operation | When |
|---|---|
| `StorageVolCreateXML` (overlay) + `DomainDefineXML` + `DomainCreate` | Create (Speculative → Idle) |
| `QEMUDomainAgentCommand` (guest-exec) + `DomainSetMetadata` | Configure / Drain (bootstrap + binding) |
| `DomainDestroy` + `DomainUndefineFlags` + `StorageVolDelete` | Delete (Idle → Speculative) |
| `ConnectListAllDomains` + `DomainGetMetadata` + `DomainGetState` | Describe / reconcile inventory |

Neither any credential nor the opaque bootstrap blob appears in the structured
logs. See [Security](/providers/libvirt/security/) for the full trust model.
