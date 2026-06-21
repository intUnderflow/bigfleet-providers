---
title: Troubleshooting
description: Common failure modes of the BigFleet OCI provider and how to diagnose them.
sidebar:
  order: 7
  label: Troubleshooting
---

## The provider boots into the fake backend unexpectedly

`--oci-backend=auto` resolves to the **fake** in-memory backend unless **both**
`--region` and `--compartment` are set. The startup log line
`using the IN-MEMORY fake OCI backend` confirms it. Set both flags (and
`--subnet`, `--image`) for the real backend, or force `--oci-backend=oci`.

## Machines land in `FAILED` shortly after `Create`/`Configure`

A machine moves to `FAILED` with `last_error` when an actuator errors or a
transition overruns its timeout. Check:

- **`Create` failing:** `LaunchInstance` errors (quota/limits for the shape or AD,
  bad subnet/image OCID, missing `manage instance-family` / `use
  virtual-network-family` / `read instance-images`). The error is in `last_error`
  and `bigfleet_oci_api_calls_total{op="LaunchInstance",outcome="error"}`.
- **`Create` timing out:** the instance didn't reach RUNNING within the Create
  timeout (8m). Look at the instance in the OCI Console for a provisioning error.
- **`Configure` failing:** the Run Command failed — the base image isn't running
  the Oracle Cloud Agent **Run Command plugin**, the bootstrap hook exited
  non-zero, or the principal lacks `use instance-agent-command-family`.
- **`Drain` failing:** `kubectl cordon/drain` returned non-zero on the node (e.g.
  a PDB that can't be satisfied within the grace period).

A `FAILED` machine carries the underlying error in `Get(...).last_error`.

## After a restart, in-flight transitions are `FAILED`

Expected without durable state. The kit cannot replay a backend actuator (notably
the `Configure` bootstrap blob) across a restart, so an interrupted transition
surfaces as `FAILED` (`...transition interrupted by a provider restart; needs
re-drive`) for the shard to re-drive. Enable `--state` on a PersistentVolume so
inventory, bindings, fence marks, and the idempotency map survive — but in-flight
transitions still resolve to `FAILED` by design.

## `Create` rejected with `FAILED_PRECONDITION`

That is **fencing**, not a fault: a shard sent a token not strictly newer than the
high-water mark (a stale/zombie process). The current shard's next request, with a
newer `(epoch, sequence)`, is accepted. No action needed.

## Inventory looks stale / an orphaned instance appears

`Describe`/reconcile recovers inventory from the `bigfleet-managed=true` and
`bigfleet-machine-id` freeform tags. A managed-but-untagged running instance is
surfaced as an orphan under its OCID so it isn't lost. If reconcile is erroring
(`bigfleet_oci_reconcile_total{outcome="error"}`), check API permissions and
throttling; the persisted store is the primary restart path.

## A preemptible machine shows a non-zero `interruption_probability`

That is correct and required — see
[Pricing & interruption](/providers/oracle-cloud/pricing-and-interruption/). A
SPOT machine with `0` would be a correctness bug; the kit rejects such a seed at
startup.

## Prices look wrong

`price_per_hour` comes from the pinned `prices.yaml` (embedded, or `--prices-file`).
It is a relative ranking signal; refresh the table from Oracle's published price
list and re-deploy. Bare-metal always reports `0`.
