# Deploying the DigitalOcean provider

Production deploy artifacts for the BigFleet DigitalOcean capacity provider: a
container image, a Helm chart, and the credential (PAT + bootstrap-channel)
Secret wiring.

The provider follows a **one-process-per-region** model. Each process owns a
single DigitalOcean region (`--region`, e.g. `nyc3`), holds region-scoped
inventory/state, and is the single `CapacityProvider` for that region. To cover
several regions, deploy the chart once per region with a distinct release name,
region, and offerings file — never scale a single release past `replicas: 1`.

> **No IAM, no Terraform.** Unlike a hyperscaler provider (AWS ships a
> least-privilege IAM policy + Terraform), DigitalOcean has no IAM/role system.
> The entire authorisation surface is a single Personal Access Token (PAT), so
> this directory ships a **Secret** (`secret/`) instead of an `iam/` Terraform
> module. There is no role to provision — the PAT is the provider's only cloud
> identity.

## 1. Build the image

The `providers/digitalocean` Go module uses a `replace ... => ../..` to resolve
the shared `providerkit` (repo-root) module from the local checkout, so the build
needs the **whole repository** in context. Build from the repo root:

```sh
# from the repository root
docker build -f providers/digitalocean/deploy/Dockerfile -t ghcr.io/intunderflow/bigfleet-digitalocean:0.1.0 .
docker push ghcr.io/intunderflow/bigfleet-digitalocean:0.1.0
```

The multi-stage build compiles with `go -C providers/digitalocean build -o /out/digitalocean .`
and ships a `distroless/static:nonroot` final image (uid 65532, no shell). It
exposes the gRPC port (`9000`), the metrics/health port (`9090`), and the on-host
agent bootstrap channel (`9443`).

## 2. Create the credential Secrets

Mint a **read + write Droplets** Personal Access Token in the DigitalOcean
control panel (**API → Tokens → Generate New Token**), or with `doctl`. Scope it
to the minimum — read+write Droplets (the provider also reads Sizes and writes
Droplet Tags); **no account/billing scope**. Store it, the bootstrap-channel
HMAC secret, and the bootstrap-channel TLS material as Secrets:

```sh
kubectl -n bigfleet create secret generic bigfleet-digitalocean-token \
  --from-literal=token="$DIGITALOCEAN_TOKEN"

kubectl -n bigfleet create secret generic bigfleet-digitalocean-bootstrap \
  --from-literal=secret="$(openssl rand -hex 32)"

kubectl -n bigfleet create secret generic bigfleet-digitalocean-bootstrap-tls \
  --from-file=tls.crt=./bootstrap.crt \
  --from-file=tls.key=./bootstrap.key \
  --from-file=ca.crt=./bootstrap-ca.crt
```

A ready-to-edit manifest for all three is in
[`secret/digitalocean-token.example.yaml`](secret/digitalocean-token.example.yaml).
The chart mounts the token as the `DIGITALOCEAN_TOKEN` env var, the HMAC secret
as `BIGFLEET_BOOTSTRAP_SECRET`, and the TLS material at
`/etc/bigfleet/bootstrap-tls`. The base image must ship the on-host agent that
the generic Create-time user-data configures. Full guidance — scoping, rotation,
never-logged — is on the [Credentials & auth](../docs/credentials.md) page.

## 3. Install the chart

Write an offerings file (see the provider README / docs for the schema), then
install one release per region:

```sh
helm install digitalocean-nyc3 providers/digitalocean/deploy/helm \
  --namespace bigfleet --create-namespace \
  --set image.repository=ghcr.io/intunderflow/bigfleet-digitalocean \
  --set image.tag=0.1.0 \
  --set region=nyc3 \
  --set regionB=sfo3 \
  --set provider=digitalocean-nyc3 \
  --set digitalocean.image=ubuntu-24-04-x64 \
  --set token.secretName=bigfleet-digitalocean-token \
  --set bootstrap.enabled=true \
  --set bootstrap.endpoint=https://do-provider.example.com:9443 \
  --set bootstrap.tlsSecretName=bigfleet-digitalocean-bootstrap-tls \
  --set bootstrap.secretName=bigfleet-digitalocean-bootstrap \
  --set-file offerings.content=offerings.nyc3.json
```

The chart renders:

- a **Deployment** (`replicas: 1`, `Recreate`) with HTTP probes
  `livenessProbe: /healthz` and `readinessProbe: /readyz` on the metrics port,
  hardened to match the non-root, read-only-rootfs image, with
  `DIGITALOCEAN_TOKEN` sourced from the token Secret, `BIGFLEET_BOOTSTRAP_SECRET`
  from the bootstrap Secret, and the bootstrap TLS material mounted read-only
  (mode 0400);
- a **Service** exposing the `grpc` port (BigFleet dials this) and a `metrics`
  port carrying `prometheus.io/scrape` annotations;
- a **ServiceAccount**;
- a **ConfigMap** for the offerings (and optional base user-data);
- an optional **PVC** for durable `--state`.

### Common extras

```sh
# Durable state on a PersistentVolume (recommended in production):
--set state.enabled=true \
--set state.persistence.enabled=true \
--set state.persistence.size=1Gi

# mTLS for the gRPC listener (Secret with tls.crt, tls.key, ca.crt):
--set tls.enabled=true --set tls.mtls=true --set tls.secretName=bigfleet-digitalocean-tls
```

### Credential-free smoke test

With no token and no region, the binary comes up on the **fake** backend (no real
Droplets) — the same path `make conformance-digitalocean` uses. Useful to confirm
the image and chart render and serve:

```sh
helm install digitalocean-smoke providers/digitalocean/deploy/helm \
  --set doBackend=fake --set region="" --set provider=digitalocean-smoke
```

## Endpoints

- gRPC `CapacityProvider` + `grpc.health.v1` + reflection on `--addr` (`:9000`).
- HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) on
  `--metrics-addr` (`:9090`).
- HTTPS on-host agent bootstrap channel on `--bootstrap-addr` (`:9443`), real
  backend only — the Droplets must be able to reach `--bootstrap-endpoint`.

Metrics are namespaced `bigfleet_digitalocean_*` on an isolated registry
(DigitalOcean API calls, gRPC requests, reconcile + price-refresh runs), plus the
standard Go/process collectors.
