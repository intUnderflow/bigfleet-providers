# Deploying the UpCloud provider

Production deploy artifacts for the BigFleet UpCloud capacity provider: a
container image, a Helm chart, and the credential (API sub-account + SSH key)
Secret wiring.

The provider follows a **one-process-per-zone** model. Each process owns a single
UpCloud zone (`--zone`, e.g. `fi-hel1`), holds zone-scoped inventory/state, and is
the single `CapacityProvider` for that zone. To cover several zones, deploy the
chart once per zone with a distinct release name, zone, and offerings file — never
scale a single release past `replicas: 1`.

> **No IAM, no Terraform.** Unlike a hyperscaler provider (AWS ships a
> least-privilege IAM policy + Terraform), UpCloud has no IAM/role system. The
> authorisation surface is a dedicated **API sub-account** (username + password
> for HTTP Basic auth), so this directory ships a **Secret** (`secret/`) instead
> of an `iam/` Terraform module. There is no role to provision — the sub-account
> is the provider's only cloud identity.

## 1. Build the image

The `providers/upcloud` Go module uses a `replace ... => ../..` to resolve the
shared `providerkit` (repo-root) module from the local checkout, so the build
needs the **whole repository** in context. Build from the repo root:

```sh
# from the repository root
docker build -f providers/upcloud/deploy/Dockerfile -t ghcr.io/intunderflow/bigfleet-upcloud:0.1.0 .
docker push ghcr.io/intunderflow/bigfleet-upcloud:0.1.0
```

The multi-stage build compiles with `go -C providers/upcloud build -o /out/upcloud .`
and ships a `distroless/static:nonroot` final image (uid 65532, no shell). It
exposes the gRPC port (`9000`) and the metrics/health port (`9090`). The bootstrap
blob is delivered **outbound** over SSH (port 22 to the server), so there is no
extra inbound port.

## 2. Create the credential Secrets

Create a **dedicated, API-scoped sub-account** in the UpCloud Control Panel
(**People → add a user**, enable API access, disable Control Panel/console login)
— do not use your main account credentials. Store its username/password, the SSH
delivery key, and (optionally) the gRPC TLS material as Secrets:

```sh
kubectl -n bigfleet create secret generic bigfleet-upcloud-credentials \
  --from-literal=username="$UPCLOUD_USERNAME" \
  --from-literal=password="$UPCLOUD_PASSWORD"

ssh-keygen -t ed25519 -N '' -f bigfleet-upcloud
kubectl -n bigfleet create secret generic bigfleet-upcloud-ssh \
  --from-file=id=./bigfleet-upcloud

kubectl -n bigfleet create secret generic bigfleet-upcloud-tls \
  --from-file=tls.crt=./provider.crt \
  --from-file=tls.key=./provider.key \
  --from-file=ca.crt=./client-ca.crt
```

A ready-to-edit manifest for all three is in
[`secret/upcloud-credentials.example.yaml`](secret/upcloud-credentials.example.yaml).
The chart mounts the credentials as the `UPCLOUD_USERNAME` / `UPCLOUD_PASSWORD`
env vars and the SSH private key at `/etc/bigfleet/ssh/id`; pass the matching
public key with `--set ssh.publicKey=...` so it is injected into every server at
create. Full guidance — scoping, rotation, never-logged — is on the
[Credentials & auth](../docs/credentials.md) page.

## 3. Install the chart

Write an offerings file (see the provider README / docs for the schema), pick an
OS template UUID to clone (an Ubuntu 24.04 cloud-init template), then install one
release per zone:

```sh
helm install upcloud-fi-hel1 providers/upcloud/deploy/helm \
  --namespace bigfleet --create-namespace \
  --set image.repository=ghcr.io/intunderflow/bigfleet-upcloud \
  --set image.tag=0.1.0 \
  --set zone=fi-hel1 \
  --set zoneB=de-fra1 \
  --set provider=upcloud-fi-hel1 \
  --set upcloud.template=<ubuntu-24.04-template-uuid> \
  --set credentials.secretName=bigfleet-upcloud-credentials \
  --set ssh.privateKeySecretName=bigfleet-upcloud-ssh \
  --set ssh.publicKey="$(cat bigfleet-upcloud.pub)" \
  --set-file offerings.content=offerings.fi-hel1.json
```

The chart renders:

- a **Deployment** (`replicas: 1`, `Recreate`) with HTTP probes
  `livenessProbe: /healthz` and `readinessProbe: /readyz` on the metrics port,
  hardened to match the non-root, read-only-rootfs image, with `UPCLOUD_USERNAME`
  / `UPCLOUD_PASSWORD` sourced from the credentials Secret and the SSH key mounted
  read-only (mode 0400);
- a **Service** exposing the `grpc` port (BigFleet dials this) and a `metrics`
  port carrying `prometheus.io/scrape` annotations;
- a **ServiceAccount**;
- a **ConfigMap** for the offerings (and optional base user-data);
- an optional **PVC** for durable `--state` (fence marks, idempotency map,
  inventory, bindings, and pinned SSH host keys).

### Common extras

```sh
# Durable state on a PersistentVolume (recommended in production):
--set state.enabled=true \
--set state.persistence.enabled=true \
--set state.persistence.size=1Gi

# mTLS for the gRPC listener (Secret with tls.crt, tls.key, ca.crt):
--set tls.enabled=true --set tls.mtls=true --set tls.secretName=bigfleet-upcloud-tls
```

### Credential-free smoke test

With no credentials and no zone, the binary comes up on the **fake** backend (no
real servers) — the same path `make conformance-upcloud` uses. Useful to confirm
the image and chart render and serve:

```sh
helm install upcloud-smoke providers/upcloud/deploy/helm \
  --set upcloudBackend=fake --set zone="" --set provider=upcloud-smoke
```

## Endpoints

- gRPC `CapacityProvider` + `grpc.health.v1` + reflection on `--addr` (`:9000`).
- HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) on
  `--metrics-addr` (`:9090`).

Metrics are namespaced `bigfleet_upcloud_*` on an isolated registry (UpCloud API
calls, gRPC requests, reconcile runs), plus the standard Go/process collectors.
