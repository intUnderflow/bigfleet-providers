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
- an **IAM policy** — granting only `InstancesFullAccess` (the Instances backend's
  calls: create/get/list/delete servers, server actions, user-data, server
  types/pricing), plus `BareMetalFullAccess` **only when**
  `enable_elastic_metal=true`, scoped to the one `project_id`;
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

## 3. The agent token

Configure delivers the per-cluster bootstrap blob to the on-host agent, which
authenticates the fetch with a **per-machine token derived from a shared
`--agent-token`** (see [Configuration](/providers/scaleway/configuration/)). Store
that shared token as its own Secret:

```sh
kubectl -n bigfleet create secret generic bigfleet-scaleway-agent \
  --from-literal=agent-token="$(openssl rand -hex 32)"
```

```yaml
agentToken:
  secretName: bigfleet-scaleway-agent
```

Use a dedicated, high-entropy value, not an operator's personal secret, and rotate
it alongside the API key. The base image needs no copy of it — the per-machine
token is derived at Configure time from this shared secret plus the server id.

## 4. Rotate

Both secrets are read by the running process — the API key on every Scaleway API
call (so a rotated Secret is picked up on the **next process start**), the agent
token at startup. To rotate without downtime:

1. Mint a new API key (`tofu taint scaleway_iam_api_key.provider && tofu apply`)
   or a new agent token **before** revoking the old one.
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
| `ApplyBootstrap` (Configure) | Record the binding + release the bootstrap blob to the agent |
| `DrainNode` (Drain) | Cordon/drain + clear the binding |
| `PriceUSD` (Pricing) | `price_per_hour` (catalogue refresh) |
| `DescribeCommercialTypeCapacities` (Catalogue) | `allocatable` (vCPU/memory/GPU) |

The key is **never logged** — neither the access/secret key, the agent token, nor
the opaque bootstrap blob appears in the structured logs. See
[Security](/providers/scaleway/security/) for the full trust model.
