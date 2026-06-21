---
title: Credentials & auth
description: Mint a least-privilege Scaleway IAM-application API key, store it as a Kubernetes Secret, mount it, and rotate it — the API-key auth model the SDK reads from SCW_*.
sidebar:
  order: 3
  label: Credentials & auth
---

Scaleway auth is **API-key based**. The provider authenticates with an **access
key + secret key** belonging to an **IAM application**, scoped to one **project**
by an **IAM policy**. There are no roles to assume and no instance profiles — the
key pair is the entire authorisation surface, read by the Scaleway SDK from
`SCW_ACCESS_KEY`, `SCW_SECRET_KEY`, and `SCW_DEFAULT_PROJECT_ID`.

This is the Scaleway equivalent of a hyperscaler's IAM page. The least-privilege
story is an **IAM application + policy + API key**: a machine identity that holds
only the permission sets the provider actually calls, scoped to a single project.

## 1. Mint the API key (least privilege)

The Terraform in
[`deploy/iam/main.tf`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/scaleway/deploy/iam/main.tf)
creates the three pieces, least privilege:

- an **IAM application** — the machine identity the provider runs as;
- an **IAM policy** — granting `InstancesFullAccess` (the Instances backend's
  calls: create/get/list/delete servers, server actions, user-data, server
  types/pricing) **and** `BlockStorageFullAccess` (required so Delete can remove
  the boot Block Storage volume — without it every Delete leaks the boot volume),
  plus `BareMetalFullAccess` **only when** `enable_elastic_metal=true`, all scoped
  to the one `project_id`;
- an **API key** for the application (the access key + secret key outputs).

```sh
cd providers/scaleway/deploy/iam
tofu init      # or: terraform init
tofu apply -var organization_id=<org> -var project_id=<proj> \
  -var name=bigfleet-scaleway-fr-par
# add -var enable_elastic_metal=true for an Elastic Metal deployment
```

Run **one application/key per region** (one provider process per zone). Naming the
application/policy/key per deployment (e.g. `bigfleet-scaleway-fr-par`) lets you
audit and rotate them independently. Keep the project itself scoped to
BigFleet-managed capacity so the key's blast radius is only the servers this
provider owns.

## 2. Store it as a Kubernetes Secret

Never put the keys in values, args, or an image. Store them as an opaque Secret
and let the chart mount them as the `SCW_*` environment variables the SDK reads:

```sh
kubectl -n bigfleet create secret generic bigfleet-scaleway-creds \
  --from-literal=SCW_ACCESS_KEY="$(tofu output -raw access_key)" \
  --from-literal=SCW_SECRET_KEY="$(tofu output -raw secret_key)" \
  --from-literal=SCW_DEFAULT_PROJECT_ID="$(tofu output -raw project_id)"
```

The chart consumes it via `credentials.secretName`:

```yaml
credentials:
  secretName: bigfleet-scaleway-creds
```

The Deployment then sets `SCW_ACCESS_KEY` / `SCW_SECRET_KEY` /
`SCW_DEFAULT_PROJECT_ID` from those Secret keys, and the provider picks them up
(the `--access-key` / `--secret-key` / `--project-id` flags fall back to the env
vars). A ready-to-edit manifest is in
[`deploy/secret/scaleway-creds.example.yaml`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/scaleway/deploy/secret).

If you use an external secrets manager (External Secrets Operator, Vault, …),
point it at the same Secret name/keys — the chart does not care how the Secret is
populated, only that it exists.

## 3. The bootstrap secret and channel TLS cert

Configure delivers the per-cluster bootstrap blob to the on-host agent over the
provider's mutually-authenticated TLS **bootstrap channel** (see
[Configuration](/providers/scaleway/configuration/)). Two pieces of material back
that channel, each stored as its own Secret.

**The HMAC secret.** The provider authorises each agent with a per-machine bearer
token = `base64(HMAC-SHA256(secret, machine_id))`, where `secret` is
`--bootstrap-secret` (env `BIGFLEET_BOOTSTRAP_SECRET`). Pin it so tokens survive a
provider restart; if it is left unset the provider generates a random one and
already-issued tokens stop authenticating after a restart. Store it as a Secret:

```sh
kubectl -n bigfleet create secret generic bigfleet-scaleway-bootstrap \
  --from-literal=bootstrap-secret="$(openssl rand -hex 32)"
```

```yaml
bootstrap:
  secret:
    secretName: bigfleet-scaleway-bootstrap
    secretKey: bootstrap-secret   # exposed as BIGFLEET_BOOTSTRAP_SECRET
```

Use a dedicated, high-entropy value, not an operator's personal secret, and rotate
it alongside the API key. The base image needs no copy of it — the per-machine
token is derived from this secret plus the machine id and baked into the server's
`user_data` at create time.

**The bootstrap channel TLS cert.** The channel serves a secret-bearing blob, so
it is always TLS. Provide a `kubernetes.io/tls` Secret holding the server cert and
key (`tls.crt`/`tls.key`) and, optionally, the CA the agent pins (`ca.crt`;
defaults to the server cert). Its SAN must match `--bootstrap-endpoint`:

```yaml
bootstrap:
  addr: ":9443"
  endpoint: https://scaleway-fr-par.bigfleet.svc:9443
  tls:
    secretName: bigfleet-scaleway-bootstrap-tls   # tls.crt, tls.key, [ca.crt]
```

Ready-to-edit manifests for both Secrets ship in
[`deploy/secret/scaleway-creds.example.yaml`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/scaleway/deploy/secret)
(`bigfleet-scaleway-bootstrap` and the TLS Secret `bigfleet-scaleway-bootstrap-tls`).

## 4. Rotate

All of these are read by the running process — the API key on every Scaleway API
call (so a rotated Secret is picked up on the **next process start**), the
bootstrap secret and the channel TLS cert at startup. To rotate without downtime:

1. Mint a new API key (`tofu taint scaleway_iam_api_key.provider && tofu apply`)
   or a new bootstrap secret / TLS cert **before** revoking the old one.
2. Update the Secret (`kubectl create secret … --dry-run=client -o yaml |
   kubectl apply -f -`, or your secrets operator).
3. Roll the Deployment (`kubectl -n bigfleet rollout restart deploy/…`) so the
   process re-reads the Secret. Because the persisted `--state` file is the
   restart path and transitions run on minute-scale timeouts, a rolling restart is
   safe.
4. Revoke the old key in the Scaleway console (or destroy the tainted key) once
   every provider has rolled.

## What the key is used for

Every Scaleway API call the provider makes, and why (each maps to a lifecycle
step):

| Call | When |
|---|---|
| `CreateServer` | Create (Speculative → Idle) |
| `DeleteServer` | Delete (Idle → Speculative) — Instances only |
| `DescribeManaged` (label-filtered) | Describe / reconcile inventory |
| `ApplyBootstrap` (Configure) | Deliver the blob to the agent over the bootstrap channel, wait for the ack, then tag the binding |
| `DrainNode` (Drain) | Drain via the agent + clear the binding |
| `PriceUSD` (Pricing) | `price_per_hour` (catalogue refresh) |
| `DescribeCommercialTypeCapacities` (Catalogue) | `allocatable` (vCPU/memory/GPU) |

The key is **never logged** — neither the access/secret key, the bootstrap secret
(nor any derived per-machine token), nor the opaque bootstrap blob appears in the
structured logs. See
[Security](/providers/scaleway/security/) for the full trust model.
