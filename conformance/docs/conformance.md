# BigFleet provider conformance program

This is the certification program for BigFleet capacity providers — modelled on
the Kubernetes conformance program: a documented set of **behaviors**, a
black-box suite that exercises them against a live provider, **profiles** a
provider claims, and a single pass/fail certification verdict.

A provider is **BigFleet-certified** when it passes, against one running
endpoint:

1. the **upstream authoritative suite** (the immovable baseline, in the bigfleet
   repo at `test/conformance/`), and
2. this **extension suite** (`conformance/suite`), which goes far beyond it.

```sh
make certify-<provider>          # builds, boots, runs BOTH suites (credential-free)
# or, against an already-running endpoint:
go test -tags=certify ./conformance/suite/... -target=host:port
```

## Relationship to the upstream suite

The bigfleet repo owns the authoritative contract and its conformance suite. **We
never modify it.** `make certify-<provider>` runs it verbatim as the baseline,
then runs the extension suite. The extension suite *deepens* coverage — it does
not fork or re-implement the ~24 upstream tests; where a behavior overlaps, our
version adds cases and asserts stronger invariants under a distinct behavior id.

## What it is (and isn't)

The suite is a **black-box gRPC client**: it dials a provider's `--addr` and uses
only the six wire RPCs. No `providerkit` imports, no process introspection — so
it certifies **any** provider implementing `bigfleet.v1alpha1.CapacityProvider`,
in-tree or out, Go or not. It runs credential-free against the fake-backed
in-repo providers in CI, and against a real provider endpoint in production.

## Behaviors registry

Each test maps to one or more behaviors. (Phases marked *planned* are the
roadmap below; the rest are implemented today.)

| ID | Category | Behavior |
|----|----------|----------|
| **C1** | Lifecycle & invariants | Full + repeated round-trips leave no residue (cluster/shard_metadata/last_error clear at a clean Idle); host/cluster invariants per state. |
| **C2** | Out-of-position **matrix** | Every (RPC × stable-source-state) that is not a legal edge and not an idempotent no-op is rejected with a **non-`FAILED_PRECONDITION`** code and **no partial transition**; RPCs at their target state are idempotent no-ops; unknown id → `NotFound` (Delete may be `Unimplemented`); empty id → `InvalidArgument`. |
| **C3** | Fencing depth | Fence runs **before** not-found and before idempotency; per-`shard_id` mark isolation; exhaustive lexicographic `(epoch, sequence)` ordering incl. new-epoch reset; reads never fence. |
| **C5** | `shard_metadata` stress | Verbatim echo of large / unicode / control-byte / empty-value / many-key maps, stable across repeated Get/List; cleared with the binding on Drain and replaced cleanly on re-Configure. |
| **C8** | Field shape & cost | Every machine: `instance_type`/`zone`/`capacity_type` top-level (never labels); `price_per_hour` ≥ 0 finite; `interruption_probability` ∈ [0,1]; SPOT > 0; host-vs-state and cluster-vs-state invariants. |
| **C9** | List & `since_revision` | Filter by every state + multi-state union; `max_results` bound; revision advances on mutation and a `since` delta includes the mutated machine (capability-gated). |

## Profiles

A provider claims a profile; behaviors that don't apply are skipped with a
reason (detected by the harness `Capabilities` probe), never failed:

- **core** — every provider (C1, C2, C3, C5, C8, C9).
- **cloud** — implements `Delete` (Idle → Speculative).
- **bare-metal** — `Delete` returns `Unimplemented`; those cells skip-as-pass.
- **spot** — exposes SPOT capacity (SPOT interruption-probability rigor applies).
- **scale** — supports `since_revision` (the delta-at-scale behaviors apply).

## How to certify a provider

`make certify-<name>` boots the provider on the fake/in-memory backend and runs
both suites. For a real endpoint (e.g. against AWS), run the provider yourself
and `go test -tags=certify ./conformance/suite/... -target=host:port`. A provider
that cannot stand up credential-free in CI adds an empty
`providers/<name>/.ci-no-conformance` marker (CI skips it; you run it manually).

## Adding a behavior

1. Add the test under `conformance/suite/<category>_test.go` (build-tagged
   `certify`), using the `harness` helpers.
2. Add a row to the registry above with a stable id.
3. Run it against the template and the AWS provider (`make certify-_template`,
   `make certify-aws`) — both must stay green.

## Roadmap (planned phases)

The program is built to grow to full Kubernetes-conformance scale. Next:

- **C4 idempotency under concurrency** — parallel retries, operation_id stability.
- **C7 transition-timeout → Failed** + **C11 fault injection** — a reference
  `faultprovider` (providerkit.Server over a fault Backend) with `--fail-*` knobs.
- **C10 durability / restart recovery** — a runner that boots a provider against
  a `FileStore`, kills and restarts it, and asserts fence marks, the idempotency
  map, and bindings + shard_metadata survive.
- **Scale & soak** — 10k–100k inventory, `since_revision` delta-at-scale, churn
  soak with continuous invariants, throughput, on a nightly lane.
- **Property-based / fuzz** — fencing total-order property, metadata round-trip
  `go test -fuzz`, a state-machine model check.
- **Runner & report** — a `bfconformance` runner emitting JUnit XML + JSON + a
  signed certification report mapped to this registry.
