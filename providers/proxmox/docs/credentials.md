---
title: Credentials
description: The least-privilege Proxmox API token the provider needs, the resource pool and role to scope it, and the mandatory TLS trust (cluster CA or pinned fingerprint).
sidebar:
  order: 3
  label: Credentials
---

Proxmox has no IAM/IRSA model. The authorisation surface is two things: the
**API token** the provider authenticates with (and the role + resource pool that
scope it) and the **TLS trust** that verifies the Proxmox API certificate. There
is no skip-verify path and no static password — the provider authenticates with a
token, sent as the `Authorization: PVEAPIToken=...` header, over a TLS channel it
always verifies.

Two distinct concerns, kept separate:

- The **API token** — `USER@REALM!TOKENID=SECRET`. The provider sends it on every
  API call. Scope it to a dedicated user + custom role + resource pool so it can
  only touch the VMs this provider owns.
- The **TLS trust material** — the cluster CA or a pinned cert fingerprint. The
  TLS channel carries the bootstrap join secret (delivered over the guest agent
  through this same API), so it must be verified.

## The API token

Create a dedicated Proxmox user, a custom role carrying only the privileges the
provider calls, a resource pool to scope it to, and a token for that user — then
grant the role on the pool. The repo ships this as a script,
[`deploy/host-setup/setup-token.sh`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/proxmox/deploy/host-setup/setup-token.sh),
to run as root on a cluster node; the steps it runs are below.

```sh
# 1. A custom role with only the privileges the provider uses (see the table
#    below). Confirm the exact privilege names against your PVE version.
pveum role add BigFleetProvider --privs \
  "VM.Allocate,VM.Clone,VM.Config.Disk,VM.Config.CPU,VM.Config.Memory,VM.Config.Network,VM.Config.Options,VM.PowerMgmt,VM.Monitor,VM.GuestAgent.Audit,VM.GuestAgent.FileSystemWrite,VM.GuestAgent.Unrestricted,VM.Audit,Datastore.AllocateSpace,Datastore.Audit,Pool.Audit,Sys.Audit"

# 2. A dedicated user in the PVE realm.
pveum user add bigfleet@pve --comment "BigFleet capacity provider"

# 3. A resource pool the provider's clones live in (the --proxmox-pool scope).
pveum pool add bigfleet --comment "BigFleet-managed VMs"

# 4. Grant the role to the user on the pool, plus an audit-only binding on /nodes
#    for the node-capacity reads (Cluster.Resources / node status).
pveum acl modify /pool/bigfleet --users bigfleet@pve --roles BigFleetProvider
pveum acl modify /nodes        --users bigfleet@pve --roles PVEAuditor

# 5. An API token for the user, with privilege separation OFF so it inherits the
#    user's ACL above. The command prints the SECRET ONCE — capture it.
pveum user token add bigfleet@pve autoscaler --privsep 0
```

Step 5 prints `full-tokenid` (`bigfleet@pve!autoscaler`) and `value` (the
secret). The token id goes to `--proxmox-token-id`; the secret goes to a file
referenced by `--proxmox-token-file` (preferred — it then never appears in a
process arg list).

:::note
The privilege names above are plausible for **PVE 8.x**; the guest-agent
privileges in particular vary by version (older PVE folds guest-agent access into
`VM.Monitor`). Confirm the exact set against your cluster's `pveum role list` and
the API documentation for your PVE version, and add only what your version
exposes.
:::

### What each privilege is for

| Privilege | Lifecycle call | Why |
|---|---|---|
| `VM.Allocate` | `Create` | Allocate the new (cloned) VMID. |
| `VM.Clone` | `Create` | Clone the source template into a fresh VM. |
| `VM.Config.*` (Disk, CPU, Memory, Options, Network) | `Create` | Size the clone (cores/memory), set its tags and description (the machine-id marker), and apply its config. |
| `VM.PowerMgmt` | `Create` / `Configure` / `Drain` / `Delete` | Start the clone, power a stopped VM back on (`EnsureRunning`), and stop it before destroy. |
| `VM.Monitor` | `Create` / `Configure` / `Drain` | Read VM/agent status; underpins the guest-agent path on some PVE versions. |
| `VM.GuestAgent.*` | `Configure` / `Drain` | Write the bootstrap blob into the guest (file-write) and run the bootstrap/drain hook (exec) over the qemu guest agent. |
| `Datastore.AllocateSpace` | `Create` | Allocate the clone's disk(s) on the target storage. |
| `Datastore.Audit` | List / reconcile | Read datastore state alongside the allocation. |
| `VM.Audit` / `Sys.Audit` / `Pool.Audit` | List / reconcile | Read cluster resources, VM config (to recover the verbatim machine id + binding), and pool membership when rebuilding inventory. |

Scope the role to **`/pool/bigfleet`** (the resource pool the provider clones
into, via `--proxmox-pool`) rather than granting it cluster-wide, so the token can
only act on VMs this provider owns; the node-capacity reads are covered by the
audit-only `PVEAuditor` binding on `/nodes`. The source **template** must also be
readable by the token — either place the template in the same pool or grant the
token `VM.Audit`/`VM.Clone` on the template's path.

### Delivering the token

Pass the id and a file holding the secret:

```sh
./bin/proxmox \
  --proxmox-token-id 'bigfleet@pve!autoscaler' \
  --proxmox-token-file /etc/bigfleet/proxmox-token/token \
  ...
```

On Kubernetes the Helm chart mounts a Secret holding the secret at
`/etc/bigfleet/proxmox-token` and wires `--proxmox-token-file` for you:

```sh
kubectl -n bigfleet create secret generic bigfleet-proxmox-token \
  --from-literal=token='xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx'
```

```yaml
proxmox:
  tokenID: bigfleet@pve!autoscaler
credentials:
  token:
    secretName: bigfleet-proxmox-token
    secretKey: token
```

The inline `--proxmox-token-secret` exists for local testing, but prefer the file
form everywhere else so the secret stays out of `ps` and shell history.

## TLS trust (mandatory)

The provider reaches the Proxmox API over HTTPS, and **TLS verification cannot be
disabled** — there is deliberately no `InsecureSkipVerify` path anywhere in the
provider. The reason is the trust model: the bootstrap join secret is delivered to
the guest over the qemu guest agent through this same TLS-protected,
token-authenticated API. If the TLS channel were not verified, that secret could
ride a man-in-the-middle. So verification material is **required** — startup fails
without it.

A Proxmox cluster's API cert is self-signed by the cluster CA by default. You
trust it one of two ways, and you must set one:

### Option A — the cluster CA bundle (recommended)

Point `--proxmox-ca-file` at the Proxmox cluster CA, `/etc/pve/pve-root-ca.pem`.
The provider verifies the API cert chains to it (standard chain + hostname
verification):

```sh
./bin/proxmox --proxmox-ca-file /etc/pve/pve-root-ca.pem ...
```

On Kubernetes, mount it as a Secret and the chart wires `--proxmox-ca-file`:

```sh
kubectl -n bigfleet create secret generic bigfleet-proxmox-ca \
  --from-file=ca.pem=/etc/pve/pve-root-ca.pem
```

```yaml
credentials:
  ca:
    secretName: bigfleet-proxmox-ca
    secretKey: ca.pem
```

This survives a cert reissue as long as the same CA signs the new cert, so it is
the lower-maintenance option.

### Option B — a pinned certificate fingerprint

If you cannot ship the CA bundle, pin the API cert's SHA-256 fingerprint with
`--proxmox-tls-fingerprint`. The provider then accepts only the exact leaf cert
whose fingerprint matches — an unexpected cert is rejected. This is verification
by pinning, not skipping it.

```sh
# Read the fingerprint from a node:
openssl x509 -in /etc/pve/local/pve-ssl.pem -noout -fingerprint -sha256
# -> SHA256 Fingerprint=AB:CD:...

./bin/proxmox --proxmox-tls-fingerprint 'AB:CD:...' ...
```

The flag accepts the common `:`-separated form or bare hex. Set
`proxmox.tlsFingerprint` in the chart (only when `credentials.ca.secretName` is
unset). A pinned fingerprint must be **re-pinned whenever the cert is reissued**,
so prefer the CA bundle unless you have a reason to pin.

If you set both `--proxmox-ca-file` and `--proxmox-tls-fingerprint`, the provider
requires the cert to both chain to the CA **and** match the fingerprint.

## Quick verification

Once deployed, a `Create → Configure → Drain → Delete` cycle exercises the token's
privileges. A missing privilege surfaces as a failed transition rather than a
silent skip — watch `bigfleet_proxmox_api_calls_total{outcome="error"}` and the
logs (see [Troubleshooting](/providers/proxmox/troubleshooting/)). A `401`/`403`
on the very first `CloneVM` almost always means the token's role is not granted on
the pool, or the privilege set is short one entry. A TLS error at startup
(`read --proxmox-ca-file` / fingerprint mismatch) means the trust material does
not match the API cert the cluster actually presents.

See also [Security](/providers/proxmox/security/) for the trust model rationale
and [Install & deploy](/providers/proxmox/install/) for the chart wiring.
