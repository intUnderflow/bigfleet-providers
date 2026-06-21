# oracle-cloud — BigFleet CapacityProvider for Oracle Cloud Infrastructure (OCI)

Out-of-tree BigFleet capacity provider for **OCI Compute**. It provisions
**on-demand, preemptible (spot), and bare-metal** capacity for a BigFleet fleet:
when BigFleet needs machines it launches OCI instances; when the fleet scales in
it drains and terminates them. One process per OCI region/compartment; BigFleet
shards dial it.

This provider implements only the substrate-specific
[`providerkit.Backend`](../../providerkit) (create / configure / drain / delete /
describe). Fencing, idempotency, async dispatch, transition timeouts, the
`shard_metadata` lifecycle, and the `Machine` field-shape are all `providerkit`'s
job — this binary never re-implements them.

| | |
|---|---|
| **Substrate** | OCI Compute (`oci-go-sdk/v65`) |
| **Capacity types** | on-demand, spot (Preemptible Instances), bare metal |
| **Create** | `LaunchInstance` (waits for RUNNING) |
| **Configure / Drain** | Oracle Cloud Agent **Run Command** (IAM-authenticated) |
| **Delete** | `TerminateInstance` |
| **Auth** | Instance Principals, OKE Workload Identity, or `~/.oci/config` |
| **Status** | **CERTIFIED** — 92/92 behaviors (fake backend, credential-free) |

## Quick start (credential-free)

```sh
# Build + run against the in-memory fake backend (no OCI tenancy needed):
make run-oracle-cloud PORT=9000

# Certify (upstream baseline + extension suite, credential-free):
make certify-oracle-cloud

# Full multi-lane report → VERDICT: CERTIFIED:
make report-oracle-cloud PROFILE=core,cloud,spot,fault,durable,scale
```

With no `--region`/`--compartment`, `--oci-backend=auto` selects the **fake**
backend, so everything above runs without credentials.

## Run against real OCI

```sh
./bin/oracle-cloud \
  --addr :9000 \
  --provider oci-eu-frankfurt-1 \
  --region eu-frankfurt-1 \
  --compartment ocid1.compartment.oc1..bbbb \
  --subnet ocid1.subnet.oc1..dddd \
  --image ocid1.image.oc1..eeee \
  --auth instance_principal \
  --offerings ./offerings.json \
  --state /var/lib/bigfleet-oracle-cloud/state.json
```

## How it maps to OCI

- **shape** → `Machine.instance_type` (e.g. `VM.Standard.E5.Flex`,
  `BM.Standard.E5.192`, `VM.GPU.A10.1`); **availability domain** → `Machine.zone`.
- **Create** launches the instance with a generic base cloud-init and tags it with
  the BigFleet machine id (freeform tag), waiting for RUNNING before reporting
  IDLE.
- **Configure** delivers the opaque, secret-bearing bootstrap blob to the running
  instance over the Oracle Cloud Agent **Run Command** (OCI cloud-init `user_data`
  is first-boot only, so it's never used for post-create delivery).
- **Flexible shapes** carry operator-declared OCPUs/memory in the offering; the
  OCPU→vCPU convention (x86 = 2 vCPU/OCPU, Ampere = 1) feeds `allocatable`.
- **Preemptible** machines always declare a non-zero `interruption_probability`
  (forecast prior, raised on observed preemption) — never a falsely-cheap zero.
- **price_per_hour** comes from a pinned, embedded `prices.yaml`; it is `0` only for `capacity_type=bare_metal` (held capacity), while a `BM.*` shape declared `on_demand` is priced at its real hourly rate.

## Layout

```
backend.go        providerkit.Backend (+ Deleter): Describe / Create / Configure / Drain / Delete
main.go           flags, TLS, listen, providerkit.New + Register
oci.go            the ociClient substrate interface + types
ocireal.go        production client (oci-go-sdk: Compute + Run Command)
ocifake.go        in-memory fake backend for credential-free certify
offering.go       offerings: shape/AD/capacity, default mix, labels
instancetypes.go  shape → allocatable (OCPU→vCPU, GPU)
pricing.go        prices.yaml loader + lookup (go:embed)
prices.yaml       pinned price table
interruption.go   preemptible interruption forecaster/observer
metrics.go        Prometheus instrumentation + client decorator
serve.go          gRPC server (interceptors, health, reflection) + /metrics,/healthz,/readyz
docs/             operator-first documentation
deploy/           Dockerfile + Helm chart + IAM Terraform + credential Secret
```

## Documentation

See [`docs/`](docs): overview, install & deploy, configuration, credentials &
auth, pricing & interruption, observability, security, troubleshooting, and
certification. Deploy artifacts are in [`deploy/`](deploy).
