---
title: Credentials & auth
description: Mint a project-scoped Latitude.sh API token, pair it with the project id/slug, store both as Kubernetes Secrets, mount them, and rotate them — the Latitude analogue of cloud IAM.
sidebar:
  order: 3
  label: Credentials & auth
---

Latitude.sh has **no IAM, roles, or instance profiles**. The entire
authorisation surface is a single **project-scoped API token** plus the
**project id/slug** that scopes every server operation. That makes auth simple —
one token, one project — but it also means the token is powerful: it can deploy
and deprovision every server in the project. Treat it accordingly.

This is the Latitude equivalent of a hyperscaler's IAM page. There is no role
model to provision, so do not look for one — mint a token, scope it to a project,
store both as Secrets, and rotate them.

## 1. Mint the token

In the [Latitude.sh dashboard](https://latitude.sh/):

1. Go to **Settings → API Keys → Create** (the dashboard's API-key area).
2. Name it for the deployment (e.g. `bigfleet-latitude-ash`) so you can audit and
   rotate it independently.
3. Copy the token **now** and store it straight into your secrets manager —
   treat it as a live credential that can deploy and delete hardware.

The provider **deploys and deprovisions servers**, registers an SSH key, creates
and deletes per-server `UserData` resources, powers servers on, and reads plans
and pricing — so the token needs full project access; a read-only key is not
enough. Keep the project itself scoped to BigFleet-managed capacity so the
token's blast radius is only the servers this provider owns.

## 2. Scope it to a project

Every server operation the provider makes is scoped to a **project** (id or
slug), passed via `--project` or `LATITUDESH_PROJECT`. The real (`latitude`)
backend will not start without it. Run **one provider process — and one
project — per site**, and keep the project dedicated to BigFleet capacity so the
inventory the provider reconciles is exactly the servers it manages.

## 3. Store them as Kubernetes Secrets

Never put the token in values, args, or an image. Store it as an opaque Secret
and let the chart mount it as the `LATITUDESH_API_TOKEN` environment variable:

```sh
kubectl -n bigfleet create secret generic bigfleet-latitude-token \
  --from-literal=token="$LATITUDESH_API_TOKEN"
```

The chart consumes it via `token.secretName` (key `token`):

```yaml
token:
  secretName: bigfleet-latitude-token
```

The Deployment then sets `LATITUDESH_API_TOKEN` from that Secret key, and the
provider picks it up (the `--token` flag falls back to `LATITUDESH_API_TOKEN`).
The project id/slug can be set inline (`project.value`) or, if you prefer to keep
it out of values, from its own Secret (`project.secretName` / `project.secretKey`,
surfaced as `LATITUDESH_PROJECT`). A ready-to-edit manifest is in
[`deploy/secret/latitude-token.example.yaml`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/latitude/deploy/secret).

If you use an external secrets manager (External Secrets Operator, Vault, …),
point it at the same Secret name/key — the chart does not care how the Secret is
populated, only that it exists.

## 4. The SSH key

Configure and Drain reach the server **over SSH** (Latitude.sh has no in-guest
command API). The provider needs an SSH **private key**, registers the matching
**public key** with Latitude, and authorises it on every server it deploys. Store
the private key as its own Secret:

```sh
kubectl -n bigfleet create secret generic bigfleet-latitude-ssh \
  --from-file=id_ed25519=./id_ed25519
```

```yaml
ssh:
  secretName: bigfleet-latitude-ssh
  user: root
```

You do not bake the public key into the image yourself — the provider registers
it with Latitude and attaches it at deploy. (Separately, the provider injects a
generated SSH **host** key via first-boot user-data so it can verify the host;
that is covered in [Security](/providers/latitude/security/).) Use a dedicated
key for the provider, not an operator's personal key, and rotate it alongside the
API token.

## 5. Rotate

Both secrets are read by the running process — the token on every Latitude API
call (so a rotated Secret is picked up on the **next process start**), the SSH
key once at startup. To rotate without downtime:

1. Mint a new token (or generate a new SSH keypair) **before** revoking the old
   one.
2. Update the Secret (`kubectl create secret … --dry-run=client -o yaml |
   kubectl apply -f -`, or your secrets operator).
3. Roll the Deployment (`kubectl -n bigfleet rollout restart deploy/…`) so the
   process re-reads the Secret. Because the persisted `--state` file is the
   restart path and transitions run on minute-scale timeouts, a rolling restart
   is safe.
4. Revoke the old token in the dashboard once every provider has rolled.

## What the credentials are used for

Every Latitude.sh API call the provider makes, and why (each maps to a lifecycle
step):

| Call | When |
|---|---|
| `Servers.Create` (+ `UserData.Create`, `SSHKeys` register) | Create (Speculative → Idle) |
| `Servers.Delete` (+ `UserData.Delete`) | Delete (Idle → Speculative) |
| `Servers.List` (project-filtered) | Describe / reconcile inventory |
| `Servers.Get` / `Servers.RunAction` (power-on) | Configure/Drain EnsureRunning |
| SSH (bootstrap delivery, drain) | Configure / Drain |
| `Plans.List` (pricing + specs) | `price_per_hour` and `allocatable` |

The token is **never logged** — neither the token nor the opaque bootstrap blob
appears in the structured logs. See [Security](/providers/latitude/security/) for
the full trust model.
