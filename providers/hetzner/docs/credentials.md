---
title: Credentials & auth
description: Mint a project-scoped, Read & Write Hetzner Cloud API token, store it as a Kubernetes Secret, mount it, and rotate it — the Hetzner analogue of cloud IAM.
sidebar:
  order: 3
  label: Credentials & auth
---

Hetzner Cloud has **no IAM, roles, or instance profiles**. The entire
authorisation surface is a single **project-scoped API token**. That makes auth
simple — one secret, one scope — but it also means the token is powerful: it can
create and delete every server in the project. Treat it accordingly.

This is the Hetzner equivalent of a hyperscaler's IAM page. There is no role
model to provision, so do not look for one — mint a token, scope it correctly,
store it as a Secret, and rotate it.

## 1. Mint the token

In the [Hetzner Cloud Console](https://console.hetzner.cloud/):

1. Select the **project** you want this provider to manage. A token is scoped to
   exactly one project — run one provider process (and one token) per project as
   well as per location.
2. Go to **Security → API Tokens → Generate API Token**.
3. Give it **Read & Write** permission. The provider **creates and deletes
   servers**, sets labels, and reads server types and pricing — read-only is not
   enough.
4. Name it for the deployment (e.g. `bigfleet-hetzner-nbg1`) so you can audit and
   rotate it independently.
5. Copy the token **now** — Hetzner shows it once.

There is no finer-grained scoping than Read & Write on Hetzner Cloud, so the
least-privilege story is: **one token per project, named per deployment, rotated
regularly.** Keep the project itself scoped to BigFleet-managed capacity so the
token's blast radius is only the servers this provider owns.

## 2. Store it as a Kubernetes Secret

Never put the token in values, args, or an image. Store it as an opaque Secret
and let the chart mount it as the `HCLOUD_TOKEN` environment variable:

```sh
kubectl -n bigfleet create secret generic bigfleet-hetzner-token \
  --from-literal=token="$HCLOUD_TOKEN"
```

The chart consumes it via `token.secretName` (key `token`):

```yaml
token:
  secretName: bigfleet-hetzner-token
```

The Deployment then sets `HCLOUD_TOKEN` from that Secret key, and the provider
picks it up (the `--token` flag falls back to `HCLOUD_TOKEN`). A ready-to-edit
manifest is in
[`deploy/secret/hcloud-token.example.yaml`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/hetzner/deploy/secret).

If you use an external secrets manager (External Secrets Operator, Vault, …),
point it at the same Secret name/key — the chart does not care how the Secret is
populated, only that it exists.

## 3. The SSH key

Configure and Drain reach the server **over SSH** (Hetzner Cloud has no in-guest
command API). The provider needs an SSH **private key**, and the base image must
authorise the matching **public key**. Store the private key as its own Secret:

```sh
kubectl -n bigfleet create secret generic bigfleet-hetzner-ssh \
  --from-file=id_ed25519=./id_ed25519
```

```yaml
ssh:
  secretName: bigfleet-hetzner-ssh
  user: root
```

Bake the public key into your image (or deliver it via `--base-user-data`
cloud-init) so the freshly created server accepts the provider's connection. Use
a dedicated key for the provider, not an operator's personal key, and rotate it
alongside the API token.

## 4. Rotate

Both secrets are read by the running process — the token on every Hetzner API
call (so a rotated Secret is picked up on the **next process start**), the SSH
key once at startup. To rotate without downtime:

1. Mint a new token (or generate a new SSH keypair and add the new public key to
   the image) **before** revoking the old one.
2. Update the Secret (`kubectl create secret … --dry-run=client -o yaml |
   kubectl apply -f -`, or your secrets operator).
3. Roll the Deployment (`kubectl -n bigfleet rollout restart deploy/…`) so the
   process re-reads the Secret. Because the persisted `--state` file is the
   restart path and transitions run on minute-scale timeouts, a rolling restart
   is safe.
4. Revoke the old token in the Console once every provider has rolled.

## What the token is used for

Every Hetzner API call the provider makes, and why (each maps to a lifecycle
step):

| Call | When |
|---|---|
| `Server.Create` | Create (Speculative → Idle) |
| `Server.Delete` | Delete (Idle → Speculative) |
| `Server.AllWithOpts` (label-filtered) | Describe / reconcile inventory |
| `Server.Update` (labels) | Configure/Drain record + clear the cluster binding |
| `ServerType.GetByName` (cores/memory + pricing) | `allocatable` and `price_per_hour` |

The token is **never logged** — neither the token nor the opaque bootstrap blob
appears in the structured logs. See [Security](/providers/hetzner/security/) for
the full trust model.
