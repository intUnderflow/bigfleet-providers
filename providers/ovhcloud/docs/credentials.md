---
title: Credentials & auth
description: Create a project-scoped OpenStack user for OVH Public Cloud, store its OS_* credentials as a Kubernetes Secret, mount them, and rotate them — the OVH analogue of cloud IAM.
sidebar:
  order: 3
  label: Credentials & auth
---

OVH Public Cloud is **OpenStack**, so authentication is a **Keystone v3 user**
scoped to one Public Cloud project — not an AWS-style IAM role graph. This is the
OVH equivalent of a hyperscaler's IAM page: instead of a role + policy + trust
relationship, you create a dedicated project-scoped user, grant it the project
`member` role (Compute + Network), store its `OS_*` credentials as a Secret, and
rotate it.

There are two secrets: the **OpenStack user** (creates/deletes instances) and the
**SSH key** (delivers the per-cluster bootstrap and cordons/drains).

## 1. Create the OpenStack user

You can create the user in the OVH Manager (**Public Cloud → Users & Roles → add
a user**, which yields a downloadable OpenStack RC file), or with the
`openstack` CLI as a project admin. The repo ships a helper that does the user
**and** the SSH keypair:

```sh
# authenticated as a project admin (openrc sourced for the target project):
providers/ovhcloud/deploy/openstack/create-scoped-user.sh bigfleet-gra <PROJECT_ID> GRA
```

It creates the user, grants **only** the project `member` role, generates an
ed25519 keypair, registers the public half in OpenStack, and prints the
`kubectl create secret` commands.

### Least privilege on OVH Public Cloud

OVH maps Public Cloud access to the OpenStack project `member` role; it does not
expose fine-grained Keystone policy editing. So "least privilege" here means:

- **One dedicated user per project**, used by nothing else — its blast radius is
  exactly that one project's instances and networks.
- **The `member` role only** (Compute create/delete, Network attach). Do not grant
  it admin or attach it to other projects.
- **One project per BigFleet deployment**, kept to BigFleet-managed capacity, so
  the user can only ever touch instances this provider owns.

The provider lists instances and filters them by the `bigfleet-managed=true`
metadata it stamps, so it never acts on anything it did not create — even within
the project.

## 2. Store the credentials as a Kubernetes Secret

Never put credentials in values, args, or an image. Store the `OS_*` variables in
an opaque Secret; the chart injects every key as an environment variable
(`envFrom`), and gophercloud reads them via the standard `AuthOptionsFromEnv`:

```sh
kubectl -n bigfleet create secret generic bigfleet-ovh-gra-os \
  --from-literal=OS_AUTH_URL=https://auth.cloud.ovh.net/v3 \
  --from-literal=OS_IDENTITY_API_VERSION=3 \
  --from-literal=OS_USERNAME=user-xxxxxxxx \
  --from-literal=OS_PASSWORD="$OS_PASSWORD" \
  --from-literal=OS_PROJECT_ID="$OS_PROJECT_ID" \
  --from-literal=OS_USER_DOMAIN_NAME=Default \
  --from-literal=OS_PROJECT_DOMAIN_NAME=Default \
  --from-literal=OS_REGION_NAME=GRA
```

The chart consumes it via `openstack.secretName`:

```yaml
openstack:
  secretName: bigfleet-ovh-gra-os
```

A ready-to-edit manifest is in
[`deploy/secret/openstack-credentials.example.yaml`](https://github.com/intUnderflow/bigfleet-providers/tree/main/providers/ovhcloud/deploy/secret).
The keys **must** be the `OS_*` variable names (the chart mounts the whole Secret
with `envFrom`). `OS_REGION_NAME` must match the release's `--region`.

If you use an external secrets manager (External Secrets Operator, Vault, …),
point it at the same Secret name — the chart does not care how the Secret is
populated, only that it exists.

## 3. The SSH key + OpenStack keypair

Configure and Drain reach the instance **over SSH** to deliver the secret-bearing
bootstrap blob and to cordon/drain. Two halves:

- The **OpenStack keypair** (`--key-name` / `ovh.keyName`) injects the **public**
  key into every instance at create, so the freshly booted instance authorises
  the provider.
- The **SSH private key** the provider holds, stored as its own Secret:

```sh
kubectl -n bigfleet create secret generic bigfleet-ovh-ssh \
  --from-file=id_ed25519=./id_ed25519
```

```yaml
ssh:
  secretName: bigfleet-ovh-ssh
  user: ubuntu
```

Use a dedicated key for the provider, not an operator's personal key, and rotate
it alongside the OpenStack user. The provider also pins each instance's **SSH host
key** at create and verifies it on every connection (see
[Security](/providers/ovhcloud/security/)), so the bootstrap channel is both
authenticated and confidential.

## 4. Rotate

The credentials are read by the running process — the OS_* env at startup (and
re-authenticated as the Keystone token expires), the SSH key once at startup. To
rotate without downtime:

1. Create the new user/password (or a new SSH keypair) **before** disabling the
   old one.
2. Update the Secret (`kubectl create secret … --dry-run=client -o yaml |
   kubectl apply -f -`, or your secrets operator).
3. Roll the Deployment (`kubectl -n bigfleet rollout restart deploy/…`) so the
   process re-reads the Secret. Because the persisted `--state` file is the
   restart path and transitions run on minute-scale timeouts, a rolling restart
   is safe.
4. Disable/delete the old user (or remove the old public key) once every provider
   has rolled.

## What the credentials are used for

Every OpenStack API call the provider makes, and why (each maps to a lifecycle
step):

| Call | When |
|---|---|
| `servers.Create` (Nova) | Create (Speculative → Idle) |
| `servers.Delete` (Nova) | Delete (Idle → Speculative) |
| `servers.List` (metadata-filtered) | Describe / reconcile inventory |
| `servers.CreateMetadatum` / `DeleteMetadatum` | Configure/Drain record + clear the cluster binding |
| `flavors.ListDetail` | `allocatable` (vCPU/memory) + flavor id resolution |
| `networks.List` | resolve a `--network` name to a UUID (once) |
| SSH (key auth) | Configure/Drain bootstrap delivery + cordon/drain |

Neither the OpenStack password, the SSH key, nor the opaque bootstrap blob is ever
logged. See [Security](/providers/ovhcloud/security/) for the full trust model.
