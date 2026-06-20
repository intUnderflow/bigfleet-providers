---
title: Credentials & auth
description: Mint a scoped DigitalOcean Personal Access Token, store it as a Kubernetes Secret, mount it, and rotate it — the DigitalOcean analogue of cloud IAM.
sidebar:
  order: 3
  label: Credentials & auth
---

DigitalOcean has **no IAM, role chain, or instance profiles**. The entire
authorisation surface is a single **Personal Access Token (PAT)**. That makes
auth simple — one secret, one identity — but it also means the token is powerful:
scoped to write on Droplets, it can create and delete every Droplet it is allowed
to touch. Treat it accordingly.

This is the DigitalOcean equivalent of a hyperscaler's IAM page. There is **no
role model to provision**, so do not look for one — mint a token, scope it to the
minimum, store it as a Secret, and rotate it.

## Single identity, vs AWS's two

If you are coming from the AWS provider, the model is simpler here, and the
difference matters:

- **AWS runs two identities.** A *provider role* the process runs as (least-
  privilege EC2 + SSM, via IRSA), **and** a separate *node instance profile* the
  launched instances run as (it needs `AmazonSSMManagedInstanceCore`).
- **DigitalOcean runs one.** The provider's **PAT is its only cloud identity**.
  There is no separate node role: the on-host agent authenticates to the provider
  with a per-machine bearer token the provider mints (see
  [Configuration](configuration.md)), not with a DigitalOcean credential. So you
  provision exactly one secret here.

Do not look for a node identity to attach — there isn't one.

## 1. Mint the token

In the [DigitalOcean control panel](https://cloud.digitalocean.com/account/api/tokens)
(**API → Tokens → Generate New Token**), or with `doctl`:

1. Give it a name tied to the deployment (e.g. `bigfleet-digitalocean-nyc3`) so
   you can audit and rotate it independently.
2. Scope it to the **minimum** the provider needs: **read + write on Droplets**,
   plus the **Sizes** and **Tags** catalogue the provider reads (for
   `allocatable`, `price_per_hour`, and the inventory tags). **Do not** grant
   account or billing scope — the provider never touches them.
3. Copy the token **now** — DigitalOcean shows it once.

The provider's calls (each maps to a lifecycle step) are: `Droplets.Create`,
`Droplets.Delete`, `Droplets.ListByTag` / `ListByName` / `Get`, `Tags.Create` /
`TagResources` / `UntagResources`, and `Sizes.List`. A read-only token cannot
create or delete Droplets, so it is not enough; scope **read + write on
Droplets** and keep everything else off.

```sh
# With doctl, the token is what you authenticate doctl itself with; generate a
# scoped one in the control panel and store it (don't reuse a personal token).
doctl auth init   # uses the token; confirms it can list Droplets
```

## 2. Store it as a Kubernetes Secret

Never put the token in values, args, or an image. Store it as an opaque Secret
and let the chart mount it as the `DIGITALOCEAN_TOKEN` environment variable:

```sh
kubectl -n bigfleet create secret generic bigfleet-digitalocean-token \
  --from-literal=token="$DIGITALOCEAN_TOKEN"
```

The chart consumes it via `token.secretName` (key `token`):

```yaml
token:
  secretName: bigfleet-digitalocean-token
```

The Deployment then sets `DIGITALOCEAN_TOKEN` from that Secret key, and the
provider picks it up (the `--token` flag falls back to `DIGITALOCEAN_TOKEN`). A
ready-to-edit manifest is in
[`deploy/secret/digitalocean-token.example.yaml`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/digitalocean/deploy/secret).

If you use an external secrets manager (External Secrets Operator, Vault, …),
point it at the same Secret name/key — the chart does not care how the Secret is
populated, only that it exists.

## 3. The bootstrap secret (not a cloud credential)

The provider also holds an HMAC **bootstrap secret** (`--bootstrap-secret` /
`BIGFLEET_BOOTSTRAP_SECRET`). This is **not** a DigitalOcean credential — it
mints the per-machine bearer tokens the on-host agent uses to fetch its
cluster-join blob over the TLS channel. Pin it (don't rely on the random
default), because a random secret invalidates already-issued agent tokens on a
provider restart. Store it as its own Secret (or alongside the bootstrap TLS
material), and treat it like the cluster-join secret it protects. The trust model
is on the [Security](security.md) page.

## 4. Rotate

Both secrets are read by the running process — the token on every DigitalOcean
API call (so a rotated Secret is picked up on the **next process start**), the
bootstrap secret at startup. To rotate without downtime:

1. Mint a new token (or new bootstrap secret) **before** revoking the old one.
2. Update the Secret (`kubectl create secret … --dry-run=client -o yaml |
   kubectl apply -f -`, or your secrets operator).
3. Roll the Deployment (`kubectl -n bigfleet rollout restart deploy/…`) so the
   process re-reads the Secret. Because the persisted `--state` file is the
   restart path and transitions run on minute-scale timeouts, a rolling restart
   is safe.
4. Revoke the old token in the control panel once every provider has rolled.

## What the token is used for

Every DigitalOcean API call the provider makes, and why (each maps to a lifecycle
step):

| Call | When |
|---|---|
| `Droplets.Create` | Create (Speculative → Idle) |
| `Droplets.Delete` | Delete (Idle → Speculative) |
| `Droplets.ListByTag` / `Get` | Describe / reconcile inventory; resolve a host |
| `Tags.Create` / `TagResources` / `UntagResources` | Configure/Drain record + clear the cluster binding |
| `Sizes.List` | `allocatable` (vCPU/memory) and `price_per_hour` |

The token is **never logged** — neither the token nor the opaque bootstrap blob
appears in the structured logs. See [Security](security.md) for the full trust
model.
