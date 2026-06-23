---
title: Conformance program
description: The BigFleet provider conformance program — 93 certified behaviors across 11 areas, profiles, and the bfconformance runner.
---

# BigFleet provider conformance program

:::note[Operators: you can stop here]
If you are running a provider, all you need to know is that **"certified" means
it passed every one of the 93 behaviors registered below** — the full
correctness, fault, durability, and scale bar. You do not need to read the
registry; the trust signal is the verdict. The rest of this page is the
developer-facing catalog for people **building or extending** providers.
:::

A certification program for BigFleet capacity providers, modelled on the
Kubernetes conformance program: a **frozen, machine-readable registry** of
behaviors, a **black-box suite** that exercises them against a live provider over
the six wire RPCs, **profiles** a provider claims, runner-orchestrated **fault /
durability / scale lanes**, and a single pass/fail **verdict** with JUnit + JSON
reports.

A provider is **BigFleet-certified** for a profile when it passes, against one
running endpoint:

1. the **upstream authoritative baseline** (the immovable suite in the bigfleet
   repo at `test/conformance/`), run verbatim, and
2. this **extension suite** (`conformance/suite`), which deepens it far beyond
   the baseline and adds entirely new areas.

## The behavior registry

The registry (`conformance/internal/registry`) is the single source of truth: a
frozen, append-only list of **leaf behaviors**, each with a stable id (never
reused — a behavior is retired by deprecation, never deletion), a one-line
wire-observable assertion, the profiles that require it, a capability gate, and
its implementation phase. Every test binds itself to a behavior id (the runner
parses the markers for coverage accounting), and a well-formedness test pins the
registry's invariants so the catalog can't silently rot.

## Profiles

A provider claims one or more profiles; the runner certifies exactly the
behaviors those profiles require and records the rest as skip-as-pass.

| Profile | What it asserts |
|---|---|
| `core` | The black-box contract every provider must satisfy (lifecycle, transition matrix, fencing, concurrency, metadata, field-shape, list/revision, property). |
| `cloud` | Adds the `Delete` lifecycle (terminate → Speculative). |
| `spot` | Adds spot interruption-probability semantics. |
| `bare-metal` | Free-pool semantics (Delete is `Unimplemented`). |
| `fault` | Failure / timeout / late-completion handling, via the reference faultprovider. |
| `durable` | Restart recovery: fence marks, idempotency, bindings, inventory survive a kill+restart against a `--state` FileStore. |
| `scale` | Scale & soak: large inventory, since_revision at scale, churn-soak, latency budgets, parallel throughput. |

## The runner — `bfconformance`

`conformance/cmd/bfconformance` builds and boots the provider credential-free,
runs every applicable lane, maps results onto the registry, and emits a
certification report:

- **baseline lane** — the upstream suite, run verbatim (never modified).
- **extension lane** — the black-box phase-2 suites (`-tags=certify`).
- **fault lane** (`fault` profile) — boots the reference **faultprovider**, which
  injects substrate faults on command over the wire, and runs the fault-tagged
  suite against it.
- **durable lane** (`durable` profile) — owns the provider lifecycle: boots with
  `--state`, drives state, **kills + restarts**, and asserts survival.
- **scale lane** (`scale` profile) — boots a large-seed provider and runs the
  scale-tagged suite.

It writes `report.json` (per-behavior status keyed to the registry) and
`junit.xml` (both lanes), with a profile-aware verdict: **CERTIFIED** when all
profile-required behaviors pass and the baseline is clean, **FAILED** on any
required failure, **INCOMPLETE** when a required behavior has no implementing test.

```sh
make certify-<provider>          # upstream baseline + extension (credential-free)
make report-<provider>           # the runner: JUnit + JSON report (PROFILE=core)
go -C conformance run ./cmd/bfconformance -provider <name> \
   -profile core,cloud,fault,durable,scale -out ./report
```

## Governance

Behavior ids are **frozen and append-only**. A new behavior gets the next free id
in its area and a test that binds to it; an obsolete behavior is marked
deprecated, never deleted or renumbered. Each behavior must be a strictly
stronger assertion than the upstream baseline — the extension *deepens*, it never
re-runs or forks the ~24 baseline tests.

## Relationship to the upstream suite

The bigfleet repo owns the authoritative contract and its conformance suite. **We
never modify it.** The baseline lane runs it verbatim; the extension only adds
cases and asserts stronger invariants under distinct behavior ids.

## The behaviors

Total: **93 behaviors** across **11 areas**.


### Lifecycle & State Machine

| ID | Profiles | Behavior |
|---|---|---|
| `B101` | core | A full Speculative->Idle->Configured->Idle round-trip repeated four times leaves cluster, shard_metadata, and last_error all empty at every return to Idle |
| `B103` | core | During a Configure, any mid-flight state observed is CONFIGURING (never another transitional), and the machine settles in Configured |
| `B104` | core | During a Drain, any mid-flight state observed is DRAINING (never another transitional), and the machine settles back in Idle |
| `B105` | core | The de-duplicated ordered state trace of a Create-then-Configure-then-Drain cycle visits only states adjacent on the four legal edges, never skipping a stable state |
| `B106` | core | cluster is non-empty whenever the machine rests in Configured and is empty once a Drain has settled to Idle; if a Configuring window is observed, cluster is already non-empty in it |
| `B107` | core | At every resting stable state, host is nil for Speculative and set for Idle/Configured, with no transitional state ever surfaced as the settled Get result |
| `B108` | core | After a settled mutation, Consistently-polling the machine over a stability window shows it never spontaneously re-enters a transitional state |
| `B109` | core,cloud | During a Delete, any mid-flight state observed is DELETING (never another transitional), and the machine settles back in Speculative _(cap: Delete)_ |

### Transition Matrix / Errors

| ID | Profiles | Behavior |
|---|---|---|
| `B201` | core | Create on a Configured machine is rejected with a non-FAILED_PRECONDITION code and leaves the machine in Configured (no partial transition) |
| `B202` | core | Configure on a Speculative machine is rejected with a non-FAILED_PRECONDITION code and leaves the machine in Speculative |
| `B203` | core | Drain on an Idle machine never uses FAILED_PRECONDITION and leaves the machine in Idle (out-of-position rejection, or an idempotent no-op when the kit holds Drain op history for the machine) |
| `B204` | core,cloud | Delete out of position (Speculative/Configured) never uses FAILED_PRECONDITION and leaves the source state unchanged — an out-of-position rejection, Unimplemented, or (Speculative being Delete's own target) an idempotent no-op |
| `B205` | core | Re-Configure on an already-Configured machine and re-Create on an already-Idle machine are idempotent no-ops that succeed and leave the target state unchanged |
| `B206` | core | An idempotent no-op-at-target call returns the same non-empty operation_id as the original transition into that target state |
| `B207` | core | Create/Configure/Drain on an unknown machine_id return NotFound, never FAILED_PRECONDITION and never a silent create |
| `B208` | core | Get/Create/Configure/Drain with an empty machine_id return InvalidArgument |
| `B209` | core | A Configure carrying more shard_metadata keys/bytes than the provider accepts is either echoed verbatim or rejected with InvalidArgument (never FAILED_PRECONDITION) with no partial transition |
| `B210` | core | A Drain with a negative grace_period_seconds is rejected with InvalidArgument and leaves the machine in Configured |
| `B211` | core | A mutating RPC carrying a negative shard_epoch or negative sequence_number with a non-empty shard_id is rejected with InvalidArgument, not FAILED_PRECONDITION |
| `B212` | core | A Configure with an int64-max shard_epoch/sequence_number against a fresh shard is accepted and establishes the high-water mark at that value without overflow |
| `B213` | core | A Configure carrying an oversized bootstrap_blob is either accepted or rejected with InvalidArgument (never FAILED_PRECONDITION) with no partial transition |
| `B214` | core | Distinct successive target-state transitions on one machine mint distinct operation_ids (operation_id freshness-per-new-cycle), complementing the same-op-id-at-target invariant |
| `B215` | core,cloud | Get on an unknown machine_id returns NotFound and Delete on an unknown machine_id returns NotFound or Unimplemented |

### Fencing

| ID | Profiles | Behavior |
|---|---|---|
| `B301` | core | A stale token aimed at a non-existent machine is rejected with FAILED_PRECONDITION, proving the fence runs before the not-found check |
| `B302` | core | Fencing high-water marks are isolated per (shard_id, machine_id): one shard's high mark never fences another shard, AND a high mark on one machine never fences a lower token on a DIFFERENT machine of the same shard (the concurrent-execute-pool out-of-order case); each (shard, machine)'s own stale token is still rejected |
| `B303` | core | On a single shard and machine, every not-strictly-newer (epoch,sequence) token is rejected with FAILED_PRECONDITION and every strictly-newer token advances the mark, lexicographically (a higher epoch with a low sequence advances and resets the sequence space) |
| `B305` | core | Get and List succeed throughout a series of interleaved fenced-out mutations, confirming reads carry no token and never fence |
| `B306` | core | The fence runs before the idempotency short-circuit on Configure: a stale token replaying an already-applied Configure is rejected with FAILED_PRECONDITION, not reused |
| `B307` | core | The fence runs before the idempotency short-circuit on Drain: a stale-token Drain replay is rejected with FAILED_PRECONDITION |
| `B308` | core | A two-zero-token (shard_id empty, epoch=0, seq=0) Create followed by another zero-token Create are both accepted, confirming an absent token bypasses fencing |
| `B309` | core | A ShardSession's monotonically auto-advancing tokens are accepted in order across Create/Configure/Drain on one machine, exercising fencing on every mutating RPC |
| `B310` | core | After a ShardSession NewEpoch (epoch++, seq reset), a replay of a pre-restart token is rejected with FAILED_PRECONDITION while the new-epoch token is accepted |
| `B311` | core | A zombie shard's passing token that establishes a mark, then fails its op against an out-of-position machine, still advances the high-water mark so its own retry is fenced |
| `B312` | core | For a randomized stream of (epoch,sequence) tokens on one shard, acceptance matches a monotonic-lexicographic oracle exactly (accept iff strictly greater than the running max) |

### Concurrency & Idempotency

| ID | Profiles | Behavior |
|---|---|---|
| `B401` | core | N parallel identical Create retries on one Speculative machine return exactly one distinct non-empty operation_id and the machine settles in Idle exactly once |
| `B402` | core | N parallel identical Configure retries (same cluster, same metadata) on one Idle machine return a single stable operation_id and settle in Configured exactly once |
| `B403` | core | N parallel identical Drain retries on one Configured machine return a single stable operation_id and settle back to Idle exactly once with cluster/metadata cleared |
| `B404` | core | Two racing conflicting mutations on one machine (e.g. Configure vs Drain) serialize so at most one succeeds and the machine lands in exactly one well-defined stable state |
| `B405` | core | Concurrent interleaved old-epoch zombie and new-epoch live tokens on one machine always let the live (higher) epoch win and always FAILED_PRECONDITION the zombie |
| `B406` | core | Idempotency is keyed on (machine_id, target_state) under contention: concurrent retries toward the same target collapse to one operation_id even when fired across distinct connections |
| `B407` | core | K machines driven to Idle concurrently each return a distinct operation_id and each independently reach Idle with no cross-machine effect bleed |
| `B408` | core | Across a parallel burst of identical retries, every succeeding ack's embedded machine snapshot reports the same target-bound transitional or settled state (no torn snapshot) |
| `B409` | core,cloud | N parallel identical Delete retries on one Idle machine return a single stable operation_id and settle to Speculative exactly once _(cap: Delete)_ |

### Metadata

| ID | Profiles | Behavior |
|---|---|---|
| `B501` | core | A 55-key map mixing embedded NUL, control bytes, unicode, and empty values is echoed byte-for-byte on Get and on List(CONFIGURED), stable across repeated reads |
| `B502` | core | shard_metadata and cluster both clear when a Drain settles to Idle, and a subsequent Configure with a disjoint map shows no key from the prior binding |
| `B503` | core | An oversized or many-key metadata map is either echoed verbatim or rejected with InvalidArgument under a documented cap, never silently truncated or summarized |
| `B504` | core | If a CONFIGURING window is observed mid-flight, shard_metadata is already visible verbatim in it (best-effort against instant actuators); otherwise the verbatim map is asserted once the machine is Configured |
| `B505` | core | Mutating a metadata map after a Configure ack does not alter the provider-stored copy: a fresh Get still returns the originally-sent bytes (no caller aliasing) |

### Timeouts & Failure

| ID | Profiles | Behavior |
|---|---|---|
| `B701` | fault | A Configure whose actuator errors drives the machine to FAILED with a non-empty last_error and no lingering CONFIGURING state _(cap: Fault)_ |
| `B702` | fault | A Create whose actuator errors drives the machine to FAILED with a non-empty last_error _(cap: Fault)_ |
| `B703` | fault | A transition that exceeds its configured timeout drives the machine to FAILED carrying a timeout-shaped non-empty last_error, never silently reverting _(cap: Fault)_ |
| `B704` | fault | A stale async actuator completion arriving after the transition already failed is discarded: the machine stays FAILED and does not flip to the success state _(cap: Fault)_ |
| `B705` | fault | A FAILED machine is terminal-pending-cleanup: a re-issued mutation toward any target is rejected (non-FAILED_PRECONDITION) and the machine stays FAILED with last_error preserved verbatim (the shard recovers on a different slot, never in place) _(cap: Fault)_ |
| `B706` | fault | A Drain with grace_period_seconds=0 against a failing actuator ends in Idle or FAILED-with-last_error, never stuck DRAINING and never a silent revert _(cap: Fault)_ |
| `B707` | fault | A FAILED machine still answers Get/List and reports its FAILED state with last_error preserved verbatim across repeated reads _(cap: Fault)_ |
| `B708` | fault | ADR-0056 node-join readiness gate: a machine is not reported CONFIGURED until its node is observed Ready — it stays CONFIGURING while readiness is unobserved, and if readiness never arrives within the Configure timeout it goes FAILED with non-empty last_error (never phantom-CONFIGURED) _(cap: Fault)_ |

### Field Shape & Cost

| ID | Profiles | Behavior |
|---|---|---|
| `B801` | core | Every machine reports a non-UNSPECIFIED state and a non-empty top-level instance_type, with no instance-type/zone/capacity-type key hidden in labels |
| `B802` | core,spot | Every machine's price_per_hour is finite and >= 0 and interruption_probability lies in [0,1], with SPOT machines reporting interruption_probability > 0 |
| `B803` | core | host is nil for Speculative and set for Idle/Configured, and cluster is empty for Speculative/Idle and non-empty for Configured |
| `B804` | core | When a machine populates allocatable and resources, both are non-empty resource maps so the density floor(allocatable/resources) is computable |
| `B805` | core | A machine's HostRef.provider is identical across every Get and List observation through its Idle->Configured->Idle lifecycle (stable host identity) |
| `B806` | core | zone and capacity_type are cross-field consistent for a stable machine: capacity_type is non-UNSPECIFIED and zone is non-empty wherever a host is set |
| `B807` | core,cloud | Walking an Idle machine through Delete back to Speculative clears host, cluster, and shard_metadata (positive Delete-clears-host) _(cap: Delete)_ |

### List, Revision & Pagination

| ID | Profiles | Behavior |
|---|---|---|
| `B901` | core | List filtered by each single state returns only machines in that state, and a multi-state filter returns the exact union and nothing else |
| `B902` | core | List with max_results=1,2,3 never returns more than the cap, and max_results=0 imposes no cap |
| `B903` | core | List.revision advances after a mutation and a delta List since the prior cursor includes the just-mutated machine _(cap: SinceRevision)_ |
| `B904` | core | A delta List since the current revision, with no intervening mutation, returns an empty machine set (empty-delta when nothing changed) _(cap: SinceRevision)_ |
| `B905` | core | Across many sequential mutations the revision cursor is monotonic: each post-mutation revision differs from the prior and a since-delta keyed on any earlier cursor includes all machines mutated after it _(cap: SinceRevision)_ |
| `B906` | core | A garbage, zero, or non-cursor since_revision value is treated as no-cursor and returns the full list without error |
| `B907` | core | Paging a fleet via repeated max_results+since_revision walks the List-as-a-set with no duplicate and no skipped machine (set-completeness) _(cap: SinceRevision)_ |
| `B908` | core | A since-poller observes no revision bump from idle reconcile ticks: with no client mutation, repeated List returns an empty delta over a quiescent window _(cap: SinceRevision)_ |
| `B909` | core | An opaque revision cursor fed back as the exact bytes a prior List emitted is accepted, while a byte-mutated copy degrades to a full list rather than erroring _(cap: SinceRevision)_ |

### Durability / Restart Recovery

| ID | Profiles | Behavior |
|---|---|---|
| `B1001` | durable | After kill+restart against the same FileStore path, a not-strictly-newer fencing token is still rejected with FAILED_PRECONDITION (high-water marks survived) _(cap: Durable)_ |
| `B1002` | durable | After restart, an idempotent retry of a pre-restart mutation returns the same operation_id as before the restart (idempotency map survived) _(cap: Durable)_ |
| `B1003` | durable | After restart, a previously Configured machine still reports its cluster and verbatim shard_metadata over Get and List (bindings survived) _(cap: Durable)_ |
| `B1004` | durable | After restart, full inventory (machine ids and states) is recovered identically with no machine lost or duplicated _(cap: Durable)_ |
| `B1005` | durable | After restart, a post-restart since-delta cursor and a freshly issued operation_id are well-formed and the operation_id is not reused from a pre-restart cycle (freshness, not counter monotonicity) _(cap: Durable)_ |
| `B1006` | durable | A transition interrupted by the kill is recovered on restart to FAILED (with last_error) or to a clean stable state, never left stuck in a transitional state _(cap: Durable)_ |
| `B1007` | durable | After restart, a brand-new shard_id's first low token is still accepted, confirming per-shard mark isolation survived without a global high-water collapse _(cap: Durable)_ |

### Scale & Soak

| ID | Profiles | Behavior |
|---|---|---|
| `B1101` | scale | With a 10k-100k seeded inventory, a full List returns every machine and each record satisfies the field-shape and cost-bound invariants _(cap: Scale)_ |
| `B1102` | scale | At scale, a since_revision delta after a bounded batch of mutations returns exactly the mutated set, with no missing or extraneous machine _(cap: Scale)_ |
| `B1103` | scale | At scale, paging the whole fleet via max_results+revision yields set-completeness (no-dup/no-skip) over tens of thousands of machines _(cap: Scale)_ |
| `B1104` | scale | A continuous Configure/Drain churn soak over many cycles keeps every machine's invariants intact and leaks no residual cluster/metadata at each Idle return _(cap: Scale)_ |
| `B1105` | scale | Over a multi-minute soak the live machine count is conserved (created==deleted+resident), proving no machine leaks or vanishes _(cap: Scale)_ |
| `B1106` | scale | Per-RPC latency histograms are captured and p99 for Get/List/Create stays within the lane's declared budget at scale _(cap: Scale)_ |
| `B1107` | scale | List cost at N machines stays within budget: full-List latency grows sub-pathologically as inventory scales from baseline to 100k _(cap: Scale)_ |
| `B1108` | scale | Under K parallel walk-to-Idle at scale, sustained mutation throughput holds and every machine reaches Idle without operation_id collision _(cap: Scale)_ |
| `B1109` | scale | Throughout the soak, Consistently-polling a sample of steady-state machines shows none drifts into a transitional or FAILED state without a client mutation _(cap: Scale)_ |

### Property / Fuzz

| ID | Profiles | Behavior |
|---|---|---|
| `B1201` | core | For a seeded random stream of (epoch,sequence) tokens on one shard, acceptance equals the lexicographic-greater-than-running-max oracle on every step (replayable via -seed) |
| `B1202` | core | For seeded random valid-UTF-8 metadata maps, each Configure round-trips byte-identically on the next Get across many generated cases |
| `B1203` | core | For seeded random valid/invalid lifecycle sequences, the provider follows the four-legal-edge model: legal edges succeed, illegal edges reject non-FAILED_PRECONDITION, and no partial transition occurs |
| `B1204` | core | For random interleavings of fenced mutations across multiple shards on shared machines, the invariant oracle confirms FAILED_PRECONDITION is emitted only for fencing rejections, never for out-of-position or not-found |
| `B1205` | core | For seeded random request shapes (empty/unknown id, negative grace, oversize metadata, oversize bootstrap_blob, malformed token), the response code matches the frozen code-discipline oracle (InvalidArgument/NotFound/Unimplemented, never FAILED_PRECONDITION) |
