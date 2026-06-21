# libvirt (QEMU/KVM) capacity provider

A BigFleet `CapacityProvider` for **libvirt** — QEMU/KVM virtual machines on your
own hosts, no cloud account. It implements only the substrate-specific
[`providerkit.Backend`](../../providerkit) (+ `Deleter`); providerkit wraps it
with the full `bigfleet.v1alpha1.CapacityProvider` contract — fencing,
idempotency, async dispatch, transition timeouts, the `shard_metadata` lifecycle,
the `Machine` field-shape, and `since_revision`. This provider never
re-implements any of that; it only maps the kit's lifecycle calls onto libvirt
and fills in the substrate fields (`instance_type`, `zone`, `capacity_type`,
`price_per_hour`, `interruption_probability`, `resources`, `allocatable`,
`host`).

> **📖 Operator documentation:** the full operator guide — install & deploy,
> configuration, credentials, pricing, observability, security, troubleshooting,
> and certification — sources live in [`docs/`](docs) (published to the site).
> This README is the quick repo-facing reference.

## Running it

```sh
make build-libvirt
./bin/libvirt --provider libvirt-dc1 \
              --connect 'rack1=qemu+libssh://bigfleet@host-a/system?keyfile=/etc/bigfleet/libvirt-ssh/id_ed25519&known_hosts=/etc/bigfleet/libvirt-ssh/known_hosts&known_hosts_verify=normal,rack2=qemu+libssh://bigfleet@host-b/system?keyfile=/etc/bigfleet/libvirt-ssh/id_ed25519&known_hosts=/etc/bigfleet/libvirt-ssh/known_hosts&known_hosts_verify=normal' \
              --image ubuntu-24.04.qcow2 \
              --storage-pool default --network default \
              --offerings ./offerings.json \
              --state /var/lib/bigfleet-libvirt/state.json \
              --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

It uses the **pure-Go** [`go-libvirt`](https://github.com/digitalocean/go-libvirt)
client (no `libvirt-dev` C library), so the binary is static and the image is
CGO-free distroless.

### Backend modes

`--libvirt-backend` selects the substrate client:

- `libvirt` — the real go-libvirt client (requires `--connect` and `--image`).
- `fake` — an in-memory simulator (dev + the credential-free conformance run).
- `auto` (default) — `libvirt` when `--connect` is set, otherwise `fake` (with a loud warning).

So a bare `./bin/libvirt --seed-count 32` (no `--connect`) comes up on the fake
backend — exactly how `make certify-libvirt` runs credential-free, with no
hypervisor.

### Key flags

| flag | meaning |
|---|---|
| `--addr` | gRPC listen address (default `:9000`) |
| `--provider` | label stamped on `HostRef.provider` (e.g. `libvirt-dc1`) |
| `--libvirt-backend` | `libvirt` \| `fake` \| `auto` (default `auto`) |
| `--connect` | a bare URI (`qemu:///system`) for the default zone, or a `zone=uri` list for multi-host |
| `--default-zone` | zone label for a single bare `--connect` URI (default `local`) |
| `--image` | golden base-image volume the overlay backs onto (required for the real backend) |
| `--storage-pool` / `--network` | libvirt pool + network for volumes / the domain NIC |
| `--instance-types` | JSON catalog `name -> {vcpu, memory_mib}` (else built-in `kvm.*`) |
| `--capacity-type` | `on_demand` (Delete implemented) or `bare_metal` (fixed pool) |
| `--base-user-data` | generic pre-binding cloud-init baked in at define |
| `--prices` / `--price-per-vcpu-hour` / `--price-per-gib-hour` | explicit or synthetic pricing |
| `--offerings` / `--seed-count` | offerings JSON file (or a default mix sized by seed-count) |
| `--state` | durable state file; empty = in-memory only |
| `--tls-cert` / `--tls-key` / `--tls-ca` | gRPC TLS / mTLS |

The full flag reference, the offerings schema, and the instance-type catalog are
in [`docs/configuration.md`](docs/configuration.md).

## Authentication

libvirt has **no IAM/role/token model** — the authorisation surface is the
**connection** itself. The provider reaches each host over `qemu:///system` (local
socket), `qemu+libssh://` (an SSH key for a least-privilege `libvirt`-group user;
use the `libssh` scheme, not `ssh`, so the pinned pure-Go client honours the
`known_hosts` host-key-pinning param — `keyfile` works on both), or `qemu+tls://`
(a libvirt client certificate on the host's `tls_allowed_dn_list`).
Store the SSH key / client cert as a Kubernetes Secret; scope the connecting
identity with the polkit rule and per-pool access in
[`deploy/host-setup/`](deploy/host-setup). The provider's own gRPC listener is
secured separately with mTLS. See [`docs/credentials.md`](docs/credentials.md).

## Configure-bootstrap reconciliation (design choice)

A cloud-init NoCloud datasource is consumed by cloud-init **only at first boot**,
but a slot's target cluster is only known when the shard binds it. So the provider
splits launch from cluster-join:

- **Create** creates a copy-on-write overlay from `--image`, attaches a NoCloud
  datasource with the generic `--base-user-data`, defines + starts the domain, and
  settles the machine to Idle (host = `<zone>/<domain>`) once it is running.
- **Configure** delivers the opaque per-cluster `bootstrap_blob` via the **qemu
  guest agent**: it writes the blob to `/opt/bigfleet/bootstrap.blob` and runs
  `/opt/bigfleet/bootstrap <cluster-id>`, waiting for success, then records the
  binding in the domain's libvirt metadata.
- **Drain** cordons/drains the kubelet via the guest agent (honouring the grace
  period) and clears the cluster binding back to Idle.
- **Delete** destroys + undefines the domain and deletes its overlay + cloud-init
  volumes (keeping the golden base image); the slot returns to Speculative.

This keeps the kit's invariant that an Idle machine already carries a real,
reachable host. Inventory and bindings are recoverable from each domain's
`bigfleet` libvirt metadata element, with the persisted `--state` file as the
primary restart path.

## Pricing & interruption

`price_per_hour` is **synthetic** — derived from the instance type's vCPU/RAM
(`--price-per-vcpu-hour` / `--price-per-gib-hour`), or pinned per type with
`--prices` — and is `0` for a `bare_metal` pool. `interruption_probability` is a
**genuine `0.0`**: local KVM has no preemption market, so the provider declares
`ON_DEMAND` (or `BARE_METAL`) for every machine and does not claim the `spot`
conformance profile. A `spot` offering is rejected at startup. See
[`docs/pricing-and-interruption.md`](docs/pricing-and-interruption.md).

## Certification

```sh
make certify-libvirt                     # upstream baseline + extension (credential-free, fake backend)
make report-libvirt PROFILE=core,cloud   # full runner -> JUnit + JSON, VERDICT: CERTIFIED
make test-libvirt                        # unit tests (race)
```

The complete multi-lane run (`PROFILE=core,cloud,fault,durable,scale`) passes all
**92 behaviors across 11 areas**. Everything cross-cutting — fencing, idempotency,
async dispatch, transition timeouts, the `shard_metadata` lifecycle, field-shape —
is handled by `providerkit`. **This provider does not re-implement any of it.**

## Deploy

A distroless, non-root container image; a Helm chart (one release per host-set,
`replicas: 1`, durable state on a PVC); and the libvirt credential Secret +
host-side least-privilege setup live in [`deploy/`](deploy). Build the image from
the **repo root** (the module's `replace => ../..` needs the whole repo):

```sh
docker build -f providers/libvirt/deploy/Dockerfile -t bigfleet-libvirt:latest .
```
