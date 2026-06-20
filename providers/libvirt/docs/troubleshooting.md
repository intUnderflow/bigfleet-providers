---
title: Troubleshooting
description: Common failure modes for the BigFleet libvirt provider and how to diagnose them from logs, metrics, and Get/List.
sidebar:
  order: 7
  label: Troubleshooting
---

Most problems show up as a machine landing in `FAILED` (read `last_error` via
`Get`) or a libvirt API error in the logs/metrics. Work from those two signals.

## The provider won't start

| Symptom | Cause | Fix |
|---|---|---|
| `--image (golden base volume) is required for the libvirt backend` | Real backend with no base image. | Pass `--image` (a qcow2 volume in `--storage-pool`). |
| `no --connect host connections configured` | `--libvirt-backend=libvirt` (or `--connect`-implied auto) with no connections. | Set `--connect`, or use `--libvirt-backend=fake` for dev. |
| `connect zone … : …` | A host in `--connect` is unreachable / auth failed. | Check the URI (use the `qemu+libssh://` scheme for SSH, not `qemu+ssh://`, so `keyfile`/`known_hosts` params are honoured), the SSH key/known_hosts (or TLS client cert), and that libvirtd is listening. |
| `both --tls-cert and --tls-key are required` / `--tls-ca set without --tls-cert/--tls-key` | Half-configured gRPC TLS. | Provide cert **and** key (and a CA for mTLS), or none. |
| Comes up on the **fake** backend unexpectedly (log: "using the IN-MEMORY fake libvirt backend") | No `--connect` set, so `auto` resolved to `fake`. | Set `--connect` to opt into the real backend. |
| `offering instance_type … is not in the instance-type catalog` | An offering names a type not in the catalog. | Add it via `--instance-types`, or use a catalog name. |
| `capacity_type "spot" is not meaningful for libvirt` | A `spot` `capacity_type` in offerings. | Use `on_demand` or `bare_metal` — a local host has no preemption market. |

## A machine reaches FAILED

`Get` the machine and read `last_error`:

| `last_error` mentions | Cause | Fix |
|---|---|---|
| `create domain … : look up base image volume` | The `--image` volume isn't in `--storage-pool`. | Stage the golden image in the pool; check the name. |
| `create domain … : create overlay volume` / `start domain` | Storage pool full, or the host can't start the domain (no KVM, bad network). | Check pool free space, `--network` exists, KVM is available on the host. |
| `configure: … guest agent exec` | The guest agent isn't running, or the bootstrap hook exited non-zero. | Ensure the base image runs `qemu-guest-agent` and ships `/opt/bigfleet/bootstrap`; confirm the hook joins the cluster and exits 0. |
| `drain: … guest agent exec` | `kubectl drain` (via the guest agent) failed or timed out. | Confirm `kubectl` is present in the image and the node name resolves; a strict PDB may exceed the grace period. |
| `transition interrupted by a provider restart` | The process was killed mid-transition. | Expected after a kill without graceful drain; the shard re-drives on a fresh slot. Enable `--state` so fence marks/bindings survive. |

A `FAILED` machine is terminal-pending-cleanup: the shard recovers on a different
slot, never in place. Don't re-issue mutations against it.

## Placement / packing looks wrong

| Symptom | Cause | Fix |
|---|---|---|
| Pods won't schedule on an instance type that should fit | `resources` set to the hardware total, forcing density = 1. | `resources` is the **per-replica** request (e.g. `{cpu:"1"}`); leave `allocatable` to the provider (derived from the instance-type catalog). See [Configuration](/providers/libvirt/configuration/). |
| `topology.kubernetes.io/zone` selectors don't match | Zone (host) mismatch. | The provider sets `zone` from the `--connect` host; confirm the offering's `zone` matches a configured connection. |
| A domain lands on the wrong host | `zone` in the offering doesn't match the intended `--connect` zone. | Align the offering `zone` with the `--connect` `zone=uri` key. |

## Cost ranking looks off

| Symptom | Cause | Fix |
|---|---|---|
| All prices are 0 | `--capacity-type bare_metal` (owned hardware prices at 0). | Expected for a bare-metal pool; use `on_demand` for synthetic pricing. |
| Prices look too high/low | Synthetic rates not tuned, or an unintended `--prices` override. | Adjust `--price-per-vcpu-hour` / `--price-per-gib-hour`, or pin `--prices`. |

## Fencing alerts

A spike of `FailedPrecondition` on
`bigfleet_libvirt_grpc_requests_total{code="FailedPrecondition"}` means a **zombie
shard** (an old shard process) is being correctly rejected. This is the provider
doing its job — investigate the shard side (a restart that didn't take over
cleanly), not the provider. `FailedPrecondition` is reserved for fencing; any
other rejection uses a different code.

## Useful commands

```sh
# What state is a machine in, and why?
grpcurl -plaintext -d '{"id":"<machine-id>"}' localhost:9000 \
  bigfleet.v1alpha1.CapacityProvider/Get

# Inventory by state.
grpcurl -plaintext -d '{}' localhost:9000 \
  bigfleet.v1alpha1.CapacityProvider/List

# Cross-check against libvirt on a host.
virsh -c qemu+ssh://bigfleet@host-a/system list --all

# Probes and metrics.
curl localhost:9090/healthz
curl localhost:9090/readyz
curl -s localhost:9090/metrics | grep bigfleet_libvirt_
```
