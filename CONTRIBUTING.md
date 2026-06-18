# Contributing

This repo holds out-of-tree BigFleet capacity providers and the shared
[`providerkit`](internal/providerkit) library they build on. Almost always,
"contributing" means **adding a provider**.

## The one rule

**The cross-cutting contract logic lives in `providerkit`, and providers must
not re-implement it.** Fencing, idempotency, async dispatch, transition
timeouts, the `shard_metadata` store/echo/clear lifecycle, and `Machine`
field-shape are the kit's job. A provider that re-implements any of them is
how the bugs the author guide warns about get back in (a zombie shard that
gets a cached `operation_id`; `FAILED_PRECONDITION` used for a non-fencing
error; `instance_type` buried in labels; `shard_metadata` that outlives its
binding). If you find yourself writing fencing or idempotency code in a
provider, stop — fix or extend `providerkit` instead.

Your provider writes **only** substrate logic: how to create / configure /
drain / (optionally) delete an instance on your backend, and how to describe
its inventory.

## Add a provider

1. **Read the source of truth first.** The bigfleet
   [`provider-author-guide.md`](https://github.com/intUnderflow/bigfleet/blob/main/docs/provider-author-guide.md)
   and [`provider.proto`](https://github.com/intUnderflow/bigfleet/blob/main/api/proto/bigfleet/v1alpha1/provider.proto).

2. **Copy the template.**

   ```sh
   cp -r providers/_template providers/<name>
   ```

3. **Implement the `Backend`.** Fill in the `TODO(provider-author)` methods in
   `providers/<name>/backend.go` against your substrate's API:
   - `Describe` — enumerate your inventory (quota slots as `Speculative`, any
     existing hosts as `Idle`). Substrate truth only.
   - `CreateInstance` / `ConfigureInstance` / `DrainInstance` — the actuators.
     Return an error to drive a machine to `FAILED`; honour `ctx` (the kit
     cancels it on transition timeout).
   - `DeleteInstance` (optional) — implement it for cloud teardown, or **delete
     the method entirely** for a bare-metal free pool (the kit then returns
     `codes.Unimplemented`, which is correct for fixed capacity).

4. **Declare your fields honestly.** On every machine your `Describe` returns:
   - `capacity_type` — `BARE_METAL` / `RESERVED` / `ON_DEMAND` / `SPOT`.
   - `price_per_hour` — USD (0 for bare metal).
   - `interruption_probability` — in `[0, 1]`; **a real value (> 0) for SPOT**.
     `effective_cost = price + interruption_probability × penalty`, so a SPOT
     machine with probability 0 will win workloads it should never run. The
     kit rejects an invalid seed (missing `instance_type`/`capacity_type`,
     out-of-bounds cost, SPOT-with-zero-probability) at startup.
   - Keep `instance_type` / `zone` top-level — never labels-only.

5. **Wire it up — automatically.** The Makefile and CI discover providers from
   `providers/*`, so `make build-<name>`, `make test-<name>`,
   `make conformance-<name>`, and the CI matrix leg all appear with no edits.
   Just add an entry to the provider table in the [README](README.md).

6. **Get conformance green.**

   ```sh
   make conformance-<name>
   ```

   A passing run is what "BigFleet-compatible" means. For a credentialed cloud
   provider, add a CI conformance step gated on a secret and skipped cleanly
   when it is unset — never fail CI for missing cloud creds (see the example in
   `.github/workflows/ci.yml`).

## Before you push

```sh
make build-all      # everything compiles (incl. the template)
make test-all       # kit + provider unit tests (race detector)
make vet
make lint           # golangci-lint
make conformance-<name>
```

## Style

- Match the surrounding code: naming, comment density, idiom.
- New cross-cutting behaviour goes in `providerkit` with unit tests that pin
  the contract (see `internal/providerkit/*_test.go` for the shape).
- Never edit, fork, or vendor the `bigfleet` repo's proto — consume it from the
  Go module so the contract can't drift.
