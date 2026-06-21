---
title: Credentials & auth
description: Create a dedicated UpCloud API sub-account, store its username + password as a Kubernetes Secret, mount it, and rotate it — the UpCloud analogue of cloud IAM.
sidebar:
  order: 3
  label: Credentials & auth
---

UpCloud has **no IAM, role chain, or instance profiles**. The entire
authorisation surface is an account's **API credentials** — a username and
password sent over **HTTP Basic auth**. That makes auth simple, but it also means
the credentials are powerful: they can create and delete every server the account
can touch. Treat them accordingly.

This is the UpCloud equivalent of a hyperscaler's IAM page. There is **no role
model to provision** — so do not look for one (there are no role ARNs, no
policies, no instance profiles). Instead, you create a **dedicated API
sub-account**, scope it to API access only, store its username + password as a
Secret, and rotate it.

## Use a dedicated API sub-account — not the main account

Do **not** use your main UpCloud account login for the provider. Create a
dedicated **sub-account** scoped to API access only, so the provider's blast
radius is bounded and you can rotate or revoke it independently of your console
login.

1. In the UpCloud Control Panel, open **People** (the sub-account / team-member
   page).
2. **Add a new sub-account** with a name tied to the deployment (e.g.
   `bigfleet-upcloud-fi-hel1`) so you can audit and rotate it on its own.
3. Enable **API access** for it, and grant it permission to manage **servers and
   storage** (the provider creates/deletes servers, clones template storage, and
   modifies labels). Leave billing/account administration **off** — the provider
   never touches them.
4. Set a strong password. The username + password are the credentials.

There is **no role ARN, policy document, or node identity** to attach — UpCloud
does not have that model. The servers the provider launches do not authenticate
back to UpCloud at all: the provider reaches them over SSH (see
[Security](security.md)), using a key pair you supply, not an UpCloud credential.

The provider authenticates with the UpCloud API over **HTTP Basic auth**
(`github.com/UpCloudLtd/upcloud-go-api/v8`), reading the credentials from
`UPCLOUD_USERNAME` and `UPCLOUD_PASSWORD`.

## Store the credentials as a Kubernetes Secret

Never put the username/password in values, args, or an image. Store them as an
opaque Secret and let the chart mount them as the `UPCLOUD_USERNAME` /
`UPCLOUD_PASSWORD` environment variables:

```sh
kubectl -n bigfleet create secret generic bigfleet-upcloud-credentials \
  --from-literal=username="$UPCLOUD_USERNAME" \
  --from-literal=password="$UPCLOUD_PASSWORD"
```

The chart consumes it via `credentials.secretName` (keys `username`, `password`):

```yaml
credentials:
  secretName: bigfleet-upcloud-credentials
```

The Deployment then sets `UPCLOUD_USERNAME` and `UPCLOUD_PASSWORD` from those
Secret keys, and the provider picks them up (the `--username`/`--password` flags
fall back to the env vars).

If you use an external secrets manager (External Secrets Operator, Vault, …),
point it at the same Secret name/keys — the chart does not care how the Secret is
populated, only that it exists.

## The SSH key (not an UpCloud credential)

The provider also holds an **SSH key pair** (`--ssh-key` / `--ssh-pubkey`). This
is **not** an UpCloud credential — it is how the provider delivers the per-cluster
bootstrap blob to a running server over SSH. The public key is injected into each
server at create; the private key authenticates the provider's SSH session. Store
the private key as its own Secret. The trust model is on the
[Security](security.md) page.

## Rotate

The credentials are read by the running process on every UpCloud API call (so a
rotated Secret is picked up on the **next process start**). To rotate without
downtime:

1. Set a new password on the API sub-account (or mint a second sub-account)
   **before** disabling the old one.
2. Update the Secret (`kubectl create secret … --dry-run=client -o yaml |
   kubectl apply -f -`, or your secrets operator).
3. Roll the Deployment (`kubectl -n bigfleet rollout restart deploy/…`) so the
   process re-reads the Secret. Because the persisted `--state` file is the
   restart path and transitions run on minute-scale timeouts, a rolling restart is
   safe.
4. Disable the old password / sub-account once every provider has rolled.

## Never logged

Neither the username, the password, nor the opaque bootstrap blob is ever written
to the structured logs. Use a distinct, named API sub-account per deployment so it
can be rotated and audited independently. See [Security](security.md) for the full
trust model.

## What the credentials are used for

Every UpCloud API call the provider makes, and why (each maps to a lifecycle
step):

| Call | When |
|---|---|
| `CreateServer` | Create (Speculative → Idle) |
| `StopServer` + `DeleteServerAndStorages` | Delete (Idle → Speculative) — stops, then deletes the server **and** its storage |
| `GetServersWithFilters` / `GetServerDetails` | Describe / reconcile inventory; resolve a host |
| `StartServer` | EnsureRunning (power a stopped server back on before Configure/Drain) |
| `ModifyServer` | Configure/Drain record + clear the cluster binding label |
| `GetPlans` | `allocatable` (cores/memory) for offered plans |
