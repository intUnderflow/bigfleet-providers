# Proxmox VE capacity provider

A BigFleet `CapacityProvider` for **Proxmox VE** — QEMU/KVM VMs on a Proxmox VE
cluster, driven through the Proxmox REST API, with no public cloud account. It
implements only the substrate-specific [`providerkit.Backend`](../../providerkit)
(+ `Deleter`); providerkit wraps it with the full
`bigfleet.v1alpha1.CapacityProvider` contract — fencing, idempotency, async
dispatch, transition timeouts, the `shard_metadata` lifecycle, the `Machine`
field-shape, and `since_revision`. This provider never re-implements any of that;
it only maps the kit's lifecycle calls onto Proxmox and fills in the substrate
fields (`instance_type`, `zone`, `capacity_type`, `price_per_hour`,
`interruption_probability`, `resources`, `allocatable`, `host`).

A "machine" is a Proxmox qemu VM (one VMID on one cluster node). `zone` is the
Proxmox cluster node; `host` is `<node>/<vmid>`; `instance_type` is an
offering-catalog entry naming a source template VMID plus the clone's
vCPU/memory.

> **📖 Operator documentation:** the full operator guide — install & deploy,
> configuration, credentials, observability, security, troubleshooting, and
> certification — lives in [`docs/`](docs) (published to the site). This README is
> the quick repo-facing reference.

## Running it

```sh
make build-proxmox
./bin/proxmox --provider proxmox-dc1 \
              --proxmox-api-url https://pve1.dc1.example:8006/api2/json \
              --proxmox-token-id 'bigfleet@pve!autoscaler' \
              --proxmox-token-file /etc/bigfleet/proxmox-token/token \
              --proxmox-ca-file /etc/pve/pve-root-ca.pem \
              --nodes pve-1,pve-2,pve-3 \
              --template-vmid 9000 --proxmox-pool bigfleet \
              --offerings ./offerings.json \
              --state /var/lib/bigfleet-proxmox/state.json \
              --tls-cert server.pem --tls-key server-key.pem --tls-ca client-ca.pem
```

It uses the **pure-Go** [`go-proxmox`](https://github.com/luthermonson/go-proxmox)
client (no C library), so the binary is static and the image is CGO-free
distroless.

### Backend modes

`--proxmox-backend` selects the substrate client:

- `proxmox` — the real go-proxmox client (requires `--proxmox-api-url`, a token, and `--nodes`).
- `fake` — an in-memory simulator (dev + the credential-free certification run).
- `auto` (default) — `proxmox` when `--proxmox-api-url` is set, otherwise `fake` (with a loud warning).

So a bare `./bin/proxmox --seed-count 32` (no `--proxmox-api-url`) comes up on the
fake backend — exactly how `make certify-proxmox` runs credential-free, with no
Proxmox cluster.

### Key flags

| flag | meaning |
|---|---|
| `--addr` | gRPC listen address (default `:9000`) |
| `--provider` | label stamped on `HostRef.provider` (e.g. `proxmox-dc1`) |
| `--proxmox-backend` | `proxmox` \| `fake` \| `auto` (default `auto`) |
| `--proxmox-api-url` | Proxmox API URL, e.g. `https://host:8006/api2/json` (real backend) |
| `--proxmox-token-id` | API token id `USER@REALM!TOKENID` |
| `--proxmox-token-secret` / `--proxmox-token-file` | the token secret (prefer the file form) |
| `--proxmox-ca-file` / `--proxmox-tls-fingerprint` | TLS trust for the API cert (mandatory; pick one) |
| `--proxmox-pool` | resource pool clones are placed in (least-privilege scope) |
| `--nodes` | comma-separated cluster node names, each a BigFleet zone (real backend) |
| `--instance-types` | JSON catalog `name -> {vcpu, memory_mib, template_vmid}` (else built-in `pve.*`) |
| `--template-vmid` | default template VMID the built-in catalog clones from |
| `--offerings` | JSON offerings file (else a default mix sized by `--seed-count`) |
| `--bootstrap-path` / `--bootstrap-exec` | in-guest path + argv for the guest-agent bootstrap delivery |
| `--prices` / `--price-per-vcpu-hour` / `--price-per-gib-hour` | pricing |
| `--state` | durable state file (fence marks, idempotency map, bindings, inventory) |
| `--metrics-addr` | `/metrics`, `/healthz`, `/readyz` (default `:9090`) |
| `--tls-cert` / `--tls-key` / `--tls-ca` | TLS/mTLS for the gRPC listener BigFleet dials |

## Capacity model

Proxmox VMs are clone-on-demand and destroy-on-`Delete`, so every machine is
`capacity_type = ON_DEMAND` with `interruption_probability = 0` (not preemptible;
no SPOT). `Delete` is implemented (stop + destroy/purge the VM **and its disks**),
so the provider honors the **`core,cloud`** conformance profiles.

The cluster-join `bootstrap_blob` is an opaque secret delivered to an
already-running VM over the **qemu guest agent**, through the TLS-verified,
token-authenticated Proxmox API — never via cloud-init. Before `Configure` and
`Drain` the provider powers the VM on first (`EnsureRunning`), since a VM the kit
holds Idle may have been stopped out of band. `Create` is idempotent (a retried
clone adopts the VM already tagged for the machine id rather than cloning twice).

## Certification

```sh
make certify-proxmox                     # upstream baseline + extension suite (credential-free)
make report-proxmox PROFILE=core,cloud   # -> VERDICT: CERTIFIED
make test-proxmox                        # unit tests (race)
```

## Deploy

Container image + Helm chart + the Proxmox token/role/pool setup live in
[`deploy/`](deploy). Build the image from the **repo root**:

```sh
docker build -f providers/proxmox/deploy/Dockerfile -t bigfleet-proxmox:dev .
```
