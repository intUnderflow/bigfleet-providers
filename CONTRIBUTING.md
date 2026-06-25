# Contributing

This repo holds out-of-tree BigFleet capacity providers and the shared
[`providerkit`](providerkit) library they build on. Almost always,
"contributing" means **adding a provider**.

**You don't have to contribute a provider here to use one.** Building your own
out-of-tree provider on `providerkit` and running it privately — for your company,
an internal substrate, or a proprietary cloud — is a fully supported, first-class
path that never goes through this repo. The mandatory, no-opt-out certification
described below applies to providers **merged into this repo**: those are a
certified-conformant starter set, not the only way to run BigFleet capacity.

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

2. **Copy the template and point its module at the new path.** Each provider is
   its own Go module (see *Why each provider is its own module* below), so the
   only setup is renaming the module:

   ```sh
   cp -r providers/_template providers/<name>
   go -C providers/<name> mod edit -module github.com/intUnderflow/bigfleet-providers/providers/<name>
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

5. **Add your dependencies — to your module only.**

   ```sh
   go -C providers/<name> get <your-substrate-sdk>@latest
   go -C providers/<name> mod tidy
   ```

   These land in `providers/<name>/go.mod` + `go.sum` — files no other provider
   shares. Keep your bigfleet pin equal to the root's: `make check-bigfleet-pin`
   (or `make sync-bigfleet` to align).

6. **Wire it up — automatically.** The Makefile and CI discover providers from
   `providers/*`, so `make build-<name>`, `make test-<name>`,
   `make conformance-<name>`, and the CI matrix leg all appear with **no edits to
   any shared file**. Just add an entry to the provider table in the
   [README](README.md).

7. **Get conformance green.**

   ```sh
   make conformance-<name>
   ```

   A passing run is what "BigFleet-compatible" means, and it is mandatory. Every
   provider ships a credential-free fake backend (run it with `--use-fake-backend`),
   so `make certify-<name>` runs on every PR with no cloud account. There is **no
   opt-out**: a provider that cannot certify credential-free is not merged.

## Why each provider is its own module

`providers/<name>/` is a **separate Go module** with its own `go.mod`/`go.sum`;
the shared library is the root module, pulled in via a `replace` directive to
the local checkout. This is deliberate: two people (or two agents) adding two
providers in parallel write to two disjoint directories and never touch a shared
file — there is no single `go.mod` to collide on, and one provider's
`go mod tidy` can never prune another's dependencies. Consequences:

- Run Go commands **inside** the provider module: `go -C providers/<name> build ./...`
  (the root `go ./...` only sees the kit). The `make *-<name>` targets do this.
- A local `go.work` is **not** committed (it would be a shared file again);
  `make gen-workspace` regenerates one for your editor on demand.
- The bigfleet proto pin is duplicated per module and kept consistent by
  `make check-bigfleet-pin` (a CI gate) and `make sync-bigfleet`.

## Before you push

```sh
make build-all          # kit + every provider module compiles
make test-all           # kit + provider unit tests (race detector)
make vet
make lint               # golangci-lint, per module
make check-bigfleet-pin # every module pins the same bigfleet proto version
make conformance-<name>
```

CI only runs the work your change needs (a provider change tests just that
provider; a `providerkit` change tests them all). The single required check is
the `ci-ok` job.

## Style

- Match the surrounding code: naming, comment density, idiom.
- New cross-cutting behaviour goes in `providerkit` with unit tests that pin
  the contract (see `providerkit/*_test.go` for the shape).
- Never edit, fork, or vendor the `bigfleet` repo's proto — consume it from the
  Go module so the contract can't drift.
