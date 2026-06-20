# Deploying the Oracle Cloud (OCI) provider

Production deploy artifacts for the BigFleet OCI Compute capacity provider: a
container image, a Helm chart, the IAM (dynamic group + policy) Terraform, and the
config-file credential Secret wiring.

The provider follows a **one-process-per-region** model. Each process owns a
single OCI region + compartment (`--region`, `--compartment`), holds region-scoped
inventory/state, and is the single `CapacityProvider` for that region. To cover
several regions, deploy the chart once per region with a distinct release name,
region, compartment, and offerings file — never scale a single release past
`replicas: 1`.

## 1. Build the image

The `providers/oracle-cloud` Go module uses a `replace ... => ../..` to resolve
the shared `providerkit` (repo-root) module from the local checkout, so the build
needs the **whole repository** in context. Build from the repo root:

```sh
# from the repository root
docker build -f providers/oracle-cloud/deploy/Dockerfile -t ghcr.io/intunderflow/bigfleet-oracle-cloud:0.1.0 .
docker push ghcr.io/intunderflow/bigfleet-oracle-cloud:0.1.0
```

The multi-stage build compiles with `go -C providers/oracle-cloud build -o /out/oracle-cloud .`
and ships a `distroless/static:nonroot` final image (uid 65532, no shell). The
pinned price table (`prices.yaml`) is embedded into the binary, so no extra files
ship. It exposes the gRPC port (`9000`) and the metrics/health port (`9090`).

## 2. Authorize the provider

The provider authenticates with **standard OCI auth** — nothing hardcoded. Pick
one:

- **Instance Principals** (production, provider on an OCI instance) or **OKE
  Workload Identity** (provider as an OKE pod). Provision the **dynamic group +
  IAM policy** with the Terraform in [`iam/`](iam):

  ```sh
  cd providers/oracle-cloud/deploy/iam
  tofu init && tofu apply \
    -var tenancy_ocid=ocid1.tenancy.oc1..aaaa \
    -var compartment_ocid=ocid1.compartment.oc1..bbbb \
    -var name=bigfleet-oci-eu-frankfurt-1 \
    -var trust_mode=instance_principal \
    -var provider_instance_ocid=ocid1.instance.oc1..cccc
  ```

  The policy grants, scoped to one compartment: `manage instance-family`,
  `use volume-family`, `use virtual-network-family`, `read instance-images`, and
  `use instance-agent-command-family` (Run Command). The raw statements are in
  [`iam/policy.txt`](iam/policy.txt) if you provision IAM by hand.

- **Config-file / API-key** (non-OKE clusters that cannot use the above). Mount a
  `~/.oci/config` + signing key as a Secret — see
  [`secret/oci-config.example.yaml`](secret/oci-config.example.yaml) — and set
  `oci.auth=config_file`, `credentials.useConfigFile=true`,
  `credentials.secretName=...`.

Full guidance — scoping, rotation, never-logged — is on the
[Credentials & auth](../docs/credentials.md) page.

## 3. Install the chart

Write an offerings file (see the provider README / docs for the schema), then
install one release per region:

```sh
helm install oci-eu-frankfurt-1 providers/oracle-cloud/deploy/helm \
  --namespace bigfleet --create-namespace \
  --set image.repository=ghcr.io/intunderflow/bigfleet-oracle-cloud \
  --set image.tag=0.1.0 \
  --set region=eu-frankfurt-1 \
  --set provider=oci-eu-frankfurt-1 \
  --set oci.compartment=ocid1.compartment.oc1..bbbb \
  --set oci.subnet=ocid1.subnet.oc1..dddd \
  --set oci.image=ocid1.image.oc1..eeee \
  --set oci.auth=instance_principal \
  --set-file offerings.content=offerings.eu-frankfurt-1.json
```

The chart renders:

- a **Deployment** (`replicas: 1`, `Recreate`) with HTTP probes
  `livenessProbe: /healthz` and `readinessProbe: /readyz` on the metrics port,
  hardened to match the non-root, read-only-rootfs image;
- a **Service** exposing the `grpc` port (BigFleet dials this) and a `metrics`
  port carrying `prometheus.io/scrape` annotations;
- a **ServiceAccount** (run the provider pod under it for Workload Identity);
- a **ConfigMap** for the offerings (and optional base user-data / price table);
- an optional **PVC** for durable `--state`.

### Common extras

```sh
# Durable state on a PersistentVolume (recommended in production):
--set state.enabled=true \
--set state.persistence.enabled=true \
--set state.persistence.size=1Gi

# OKE Workload Identity instead of Instance Principals:
--set oci.auth=workload_identity

# Config-file credentials (non-OKE):
--set oci.auth=config_file \
--set credentials.useConfigFile=true \
--set credentials.secretName=bigfleet-oci-config

# mTLS for the gRPC listener (Secret with tls.crt, tls.key, ca.crt):
--set tls.enabled=true --set tls.mtls=true --set tls.secretName=bigfleet-oci-tls
```

## Endpoints

- gRPC `CapacityProvider` + `grpc.health.v1` + reflection on `--addr` (`:9000`).
- HTTP `/metrics`, `/healthz` (liveness), `/readyz` (readiness) on
  `--metrics-addr` (`:9090`).

Metrics are namespaced `bigfleet_oci_*` on an isolated registry (OCI Compute API
calls, gRPC requests, reconcile runs), plus the standard Go/process collectors.

> The Helm chart and Terraform here were authored against the certified AWS and
> Hetzner provider artifacts; `helm` / `tofu` were not available in the build
> sandbox to render them. Run `helm template` and `tofu validate` in your
> environment before a production rollout.
