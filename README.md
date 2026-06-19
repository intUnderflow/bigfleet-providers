# bigfleet-providers

Out-of-tree **capacity providers** for [BigFleet](https://github.com/intUnderflow/bigfleet), and the shared library every provider is built on.

BigFleet is a fleet-level infrastructure autoscaler: it takes capacity demand from many Kubernetes clusters, diffs it against provisioned inventory, and provisions/reclaims machines through pluggable **`CapacityProvider`** backends. A *provider* is the component that actually creates / configures / drains / deletes machines on a specific substrate (AWS, GCP, libvirt, bare metal, …). BigFleet is not a scheduler — it does not place pods.

## Why this repo exists (out-of-tree, on purpose)

**BigFleet ships zero real providers in its own repo, deliberately.** Kubernetes spent years undoing in-tree CCM/CSI providers; BigFleet does not repeat that. Every real provider lives here, in a repo that is *separate from the main `bigfleet` repo* — and provider work **never modifies the `bigfleet` repo**.

These providers all live together in one **mono-repo** (rather than one repo per provider) so they share:

- **one correctness-critical library** — [`internal/providerkit`](internal/providerkit) — that gets fencing, idempotency, async dispatch, `shard_metadata`, and the `Machine` field shape right *once*, so each provider only writes substrate-specific logic;
- **one conformance harness** — pointed at the canonical suite in the bigfleet repo;
- **one CI pipeline** and one place to read.

Each provider is still an independently buildable binary that could ship on its own cadence.

## The contract (source of truth lives in bigfleet)

A provider is a gRPC **server** implementing `bigfleet.v1alpha1.CapacityProvider`; the BigFleet shard is the **client** that dials your `--addr`. The contract is six RPCs — **no `Watch`**; reconciliation is `List` + `Get`:

| RPC | Lifecycle | Async | Idempotent |
|---|---|---|---|
| `Create` | Speculative → Creating → Idle | yes | yes, on `(machine, Create)` |
| `Configure` | Idle → Configuring → Configured | yes | yes |
| `Drain` | Configured → Draining → Idle | yes | yes |
| `Delete` | Idle → Deleting → Speculative | yes | yes |
| `Get` | read one machine | — | — |
| `List` | read inventory (opaque `revision` cursor) | — | — |

The authoritative definitions are **in the bigfleet repo**, consumed here via the Go module (never re-generated, never vendored — so the wire contract can't drift):

- [`api/proto/bigfleet/v1alpha1/provider.proto`](https://github.com/intUnderflow/bigfleet/blob/main/api/proto/bigfleet/v1alpha1/provider.proto) — the wire contract.
- [`docs/provider-author-guide.md`](https://github.com/intUnderflow/bigfleet/blob/main/docs/provider-author-guide.md) — **read this first.** It is the spine.
- [`test/conformance/`](https://github.com/intUnderflow/bigfleet/tree/main/test/conformance) — the acceptance suite. A passing run is what "BigFleet-compatible" means.

Generated Go types and the server interface come from `github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1`.

## Providers

| Provider | Capacity types | Status |
|---|---|---|
| [`aws`](providers/aws) | on-demand, spot, reserved | EC2 — passes conformance (fake EC2 backend); live demo needs AWS creds |
| [`_template`](providers/_template) | on-demand + spot (example) | copy-me skeleton — passes conformance against an in-memory backend |

More providers (gcp, libvirt, …) are added by copying `_template`; the table grows as they land.

## Repository layout

```
internal/providerkit/    the shared correctness library (the crown jewel)
providers/_template/     copy-me provider skeleton (compiles, passes conformance)
providers/<name>/        a real provider: cp -r providers/_template providers/<name>
hack/run-conformance.sh  boots a provider + runs the bigfleet conformance suite
Makefile                 build-/test-/run-/conformance-<name>, build-all, test-all, lint
.github/workflows/ci.yml provider matrix + the credential-free conformance gate
```

Providers are discovered automatically from `providers/*` — a new one needs no Makefile or CI edits. The `_template` directory is `_`-prefixed so the Go toolchain skips it in `./...`; it is built and conformance-tested explicitly.

## What `providerkit` gives you

You implement the small substrate-specific [`providerkit.Backend`](internal/providerkit/backend.go) (create / configure / drain / describe, plus optional delete) and wrap it in a [`providerkit.Server`](internal/providerkit/server.go). The kit then handles everything that is identical across providers and easy to get subtly wrong:

- **Async dispatch** — lifecycle RPCs return a `TransitionAck` immediately; the backend runs in the background; progress shows up via `Get`/`List`.
- **Idempotency** — same `(machine, operation)` returns the same `operation_id`, persisted across restarts.
- **Transition timeouts** — a transition that overruns lands the machine in `FAILED` with `last_error`.
- **Fencing** — every mutating RPC carries `(shard_id, shard_epoch, sequence_number)`; the kit tracks the per-shard high-water mark, rejects not-strictly-newer tokens with `FAILED_PRECONDITION` **without applying them**, checks the fence **before** the idempotency short-circuit, and persists the marks. `FAILED_PRECONDITION` is used **only** for fencing.
- **`shard_metadata`** — stored verbatim from `Configure`, echoed byte-for-byte on every snapshot, cleared together with the cluster binding when a `Drain` completes. Never interpreted.
- **Field shape** — `instance_type` / `zone` / `capacity_type` stay top-level (never labels-only); `interruption_probability` is validated for every machine and required (> 0) for SPOT.
- **`since_revision`** — incremental `List` plumbing for free.

The persistent [`providerkit.Store`](internal/providerkit/store.go) keeps the fence marks, the idempotency map, and the inventory (with bindings + `shard_metadata`) — so a provider restart loses nothing. Two implementations ship: an in-memory store (tests / ephemeral) and a durable JSON file store.

## Capacity type, pricing & interruption probability

The shard ranks machines with a **locked cost formula**:

```
effective_cost = price_per_hour + interruption_probability × penalty
```

So every provider must declare these honestly:

- **`capacity_type`** — `BARE_METAL`, `RESERVED`, `ON_DEMAND`, or `SPOT`. Drives idle-hold policy (fixed capacity is held forever; spot ~1 minute). Declare it truthfully — the shard's release path only ever `Delete`s `ON_DEMAND`/`SPOT`.
- **`price_per_hour`** — USD; `0` for bare metal (already paid for).
- **`interruption_probability`** — hourly, in `[0, 1]`, provider-declared only (no cluster override). **A SPOT machine with probability `0` is a correctness bug**: it will be picked for high-penalty workloads it should never run. `providerkit` rejects SPOT seeds with a zero probability at startup.

## Deployment shape

- **One process per provider.** Don't co-locate with the shard. Listen on the provider's own gRPC port.
- **mTLS in production**, insecure is acceptable for in-cluster trust. The template supports `--tls-cert` / `--tls-key` / `--tls-ca` (mTLS when the CA is set).
- **One coordinator registry entry per (provider implementation × region).** AWS in `us-east-1` and AWS in `eu-west-1` are two registry entries of the same implementation.

## Running conformance

```sh
# Build + boot a provider, seed Speculative slots, run the bigfleet suite:
make conformance-_template            # the template (in-memory backend)
make conformance-<name> PORT=9100     # any provider

# Reuse an existing bigfleet checkout instead of cloning:
BIGFLEET_SRC=/path/to/bigfleet make conformance-<name>
```

The conformance suite is in the bigfleet repo; `hack/run-conformance.sh` clones the exact version pinned in `go.mod` (into `.cache/`) unless `BIGFLEET_SRC` is set. Equivalently, from a bigfleet checkout: `make conformance TARGET=host:port`.

## Design decisions (kept deliberately simple)

- **Module path** `github.com/intUnderflow/bigfleet-providers`, **Go 1.26.0**, **MIT license** — all matched to the bigfleet module.
- **Store**: a JSON file store (atomic temp-write + rename), not bbolt — the YAGNI-simplest durable option, dependency-free. Providers exposing more than ~10k machines/shard should supply a delta-oriented `Store`; the interface is two methods plus `Close`.
- **Delete is optional** via a separate `providerkit.Deleter` interface — a bare-metal provider omits it and the kit answers `Delete` with `codes.Unimplemented`.

## License

MIT — see [LICENSE](LICENSE).
