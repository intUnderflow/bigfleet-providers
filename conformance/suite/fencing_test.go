//go:build certify

package suite

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"

	"github.com/intUnderflow/bigfleet-providers/conformance/internal/harness"
)

// Fencing area (B30x) — registry-bound, DEEPENED beyond the upstream
// stale-epoch / stale-seq / new-epoch / unknown-shard / reads-unaffected
// baseline.
//
// The fencing contract (paper §11, M71): every mutating RPC carries a token
// (shard_id, shard_epoch, sequence_number). For a non-empty shard_id the
// provider keeps a per-shard high-water mark and accepts a token IFF its
// (epoch, sequence) is strictly lexicographically greater than the running
// mark; a not-strictly-newer token is rejected with FAILED_PRECONDITION, the
// ONLY code the contract reserves for fencing. The fence runs FIRST — before
// the not-found check and before the idempotency short-circuit. An absent
// token (empty shard_id, epoch=0, seq=0) bypasses fencing entirely. Reads
// (Get/List) carry no token and never fence.

// fpOnly asserts err is exactly FAILED_PRECONDITION (the fencing-reserved code).
func fpOnly(t *testing.T, what string, err error) {
	t.Helper()
	if harness.Code(err) != codes.FailedPrecondition {
		t.Errorf("%s: code %s, want FAILED_PRECONDITION", what, harness.Code(err))
	}
}

// accepted asserts err is nil (the token passed the fence and the op was taken).
func accepted(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: expected accept (nil), got code %s (%v)", what, harness.Code(err), err)
	}
}

// B301 — the fence runs BEFORE the not-found check: a stale token aimed at a
// non-existent machine is rejected with FAILED_PRECONDITION, not NotFound, so a
// zombie cannot even probe machine existence through a fenced RPC. Deepened: we
// establish the mark, then confirm the fence-before-not-found ordering on EVERY
// mutating RPC (Create/Configure/Drain/Delete), not just Create — and confirm a
// FRESH (strictly-newer) token to the same ghost surfaces NotFound (proving the
// ghost really is unknown and FAILED_PRECONDITION above came purely from the
// fence, not from a generic rejection).
func TestB301_FenceBeforeNotFound(t *testing.T) {
	behavior(t, "B301")
	h := dial(t)
	shard := h.UniqueShardID("b301")
	const ghost = "conformance-b301-ghost-machine"

	// Establish a high-water mark for this shard against a real machine.
	real := h.PickSpeculative()
	if err := h.FencedCreate(real, shard, 5, 5); err != nil {
		t.Fatalf("establish mark (5,5): %v", err)
	}

	// A STALE token (1,1) for this shard, aimed at a machine that does not
	// exist, is fenced FIRST on every mutating RPC.
	for _, rpc := range []harness.RPC{harness.RPCCreate, harness.RPCConfigure, harness.RPCDrain, harness.RPCDelete} {
		err := h.FencedCall(rpc, ghost, shard, 1, 1)
		// Delete may be Unimplemented on some providers; the AWS fake supports
		// it. Either way it must NOT leak NotFound for a stale token.
		if rpc == harness.RPCDelete && harness.Code(err) == codes.Unimplemented {
			continue
		}
		fpOnly(t, rpc.String()+" stale token on unknown machine", err)
	}

	// A FRESH token (strictly newer than the mark) to the same ghost now gets
	// past the fence and surfaces the real NotFound — confirming the ghost is
	// genuinely unknown and the FAILED_PRECONDITION above was the fence, not a
	// blanket rejection of the ghost.
	err := h.FencedCreate(ghost, shard, 9, 9)
	if harness.Code(err) != codes.NotFound {
		t.Errorf("fresh token on unknown machine: code %s, want NotFound (fence passed, then not-found)", harness.Code(err))
	}
}

// B302 — fencing high-water marks are isolated per (shard_id, machine_id).
// Two halves: (1) cross-SHARD — one shard's high mark never fences another
// shard's first low-token contact, and the owning shard's own stale token is
// still rejected; (2) cross-MACHINE — within ONE shard, a high mark on one
// machine never fences a LOWER token on a DIFFERENT machine. Half (2) is the
// concurrent-execute-pool case: a shard's worker pool draws monotonic
// sequence numbers but races the sends, so a lower seq for machine B can
// arrive after a higher seq for machine A on the same shard; a per-shard mark
// would brick B as a false zombie. Per-machine isolation accepts B while
// keeping each (shard, machine)'s own monotonicity. A true zombie (lower
// epoch) is still rejected per machine.
func TestB302_PerShardIsolation(t *testing.T) {
	behavior(t, "B302")
	h := dial(t)
	shardA := h.UniqueShardID("b302-a")
	shardB := h.UniqueShardID("b302-b")
	shardC := h.UniqueShardID("b302-c")
	mA := h.PickSpeculative()
	mB := h.PickSpeculative()
	mC := h.PickSpeculative()

	// Drive A and B to high marks.
	accepted(t, "shardA establish (9,9)", h.FencedCreate(mA, shardA, 9, 9))
	accepted(t, "shardB establish (7,3)", h.FencedCreate(mB, shardB, 7, 3))

	// shardC's FIRST contact with the lowest meaningful token is accepted —
	// its mark is independent of A's and B's high marks.
	accepted(t, "shardC first low-token contact (1,1)", h.FencedCreate(mC, shardC, 1, 1))

	// Each owning shard's stale token is STILL rejected (isolation does not
	// weaken any shard's own monotonicity).
	fpOnly(t, "shardA own stale (1,1)", h.FencedCreate(mA, shardA, 1, 1))
	fpOnly(t, "shardB own stale (7,3)", h.FencedCreate(mB, shardB, 7, 3)) // equal == not strictly newer
	// And the just-established shardC mark fences its own replay too.
	fpOnly(t, "shardC own stale (1,1) replay", h.FencedCreate(mC, shardC, 1, 1))

	// Cross-shard non-interference is symmetric: A's high mark does not block a
	// low token on shardB's machine via shardB (shardB only has mark (7,3), so
	// (7,4) advances it cleanly).
	accepted(t, "shardB advance (7,4) unaffected by shardA mark", h.FencedCreate(mB, shardB, 7, 4))

	// (2) Cross-MACHINE isolation within ONE shard — the concurrent-execute-pool
	// out-of-order case. A fresh shard establishes a HIGH mark on machine mA,
	// then a LOWER token on a DIFFERENT machine mB of the same shard must be
	// ACCEPTED (a per-shard mark would have fenced it as a false zombie).
	// Per-machine monotonicity is preserved (each machine's own stale token is
	// still rejected) and a true zombie (lower epoch) is still rejected.
	shardD := h.UniqueShardID("b302-crossmachine")
	accepted(t, "shardD mA establish high (5,30)", h.FencedCreate(mA, shardD, 5, 30))
	accepted(t, "shardD mB LOWER token (5,7) accepted — different machine, same shard", h.FencedCreate(mB, shardD, 5, 7))
	fpOnly(t, "shardD mB own stale (5,7) replay", h.FencedCreate(mB, shardD, 5, 7))
	fpOnly(t, "shardD mA own stale (5,30) replay unaffected by mB", h.FencedCreate(mA, shardD, 5, 30))
	fpOnly(t, "shardD mB stale-epoch zombie (4,999)", h.FencedCreate(mB, shardD, 4, 999))
}

// B303 — exhaustive lexicographic ordering of the (epoch, sequence) mark on a
// single shard: every not-strictly-newer token is rejected with
// FAILED_PRECONDITION; every strictly-newer token advances the mark; a higher
// epoch with a LOW sequence advances and resets the sequence space. Deepened
// well beyond the baseline with epoch resets, equal-token replays, and
// boundary sequences.
func TestB303_LexicographicOrdering(t *testing.T) {
	behavior(t, "B303")
	h := dial(t)
	shard := h.UniqueShardID("b303")
	m := h.PickSpeculative()

	// Establish the mark at (epoch=2, seq=5).
	accepted(t, "establish (2,5)", h.FencedCreate(m, shard, 2, 5))

	type step struct {
		epoch, seq int64
		accept     bool
		why        string
	}
	steps := []step{
		{2, 5, false, "replay equal token (not strictly newer)"},
		{2, 4, false, "lower sequence, same epoch"},
		{1, 9, false, "lower epoch dominates regardless of sequence"},
		{1, 99999, false, "lower epoch dominates even with a huge sequence"},
		{2, 6, true, "higher sequence, same epoch -> advances to (2,6)"},
		{2, 6, false, "replay of the just-accepted token"},
		{2, 5, false, "old in-epoch token after advance"},
		{3, 0, true, "higher epoch with seq=0 -> new epoch resets the seq space, advances to (3,0)"},
		{3, 0, false, "replay after the epoch bump"},
		{2, 999999, false, "any token in the OLD epoch is now stale, however high its seq"},
		{3, 1, true, "advance within the new epoch (3,1)"},
		{4, 0, true, "higher epoch again -> advances to (4,0)"},
		{4, 0, false, "replay after the second epoch bump"},
		{10, 7, true, "skip-ahead epoch is fine (strictly greater) -> (10,7)"},
		{10, 7, false, "replay of the skip-ahead token"},
		{9, 999, false, "epoch below the current mark, any seq, rejected"},
	}
	for _, s := range steps {
		err := h.FencedCreate(m, shard, s.epoch, s.seq)
		if s.accept {
			accepted(t, s.why, err)
		} else {
			fpOnly(t, s.why, err)
		}
	}
}

// B305 — reads carry no token and never fence. Get and List succeed throughout
// a series of interleaved fenced-out mutations. Deepened: interleave several
// stale rejections, and after EACH one confirm both Get and a state-filtered
// List still answer (and that the fenced-out mutation truly left no mark drift
// — the machine never moved out of its resting state).
func TestB305_ReadsNeverFence(t *testing.T) {
	behavior(t, "B305")
	h := dial(t)
	shard := h.UniqueShardID("b305")

	// Establish a high mark by driving the machine to a resting Idle with a
	// successful fenced Create at (7,7). We capture the resting state AFTER this
	// so the stale-token no-op assertion below has a clean baseline.
	m := h.PickSpeculative()
	accepted(t, "establish (7,7) -> Idle", h.FencedCreate(m, shard, 7, 7))
	rest := h.MustReach(m, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second).GetState()

	for i := 0; i < 4; i++ {
		// A stale mutation is fenced out (Create@Idle would be an idempotent
		// no-op absent fencing, so the FAILED_PRECONDITION can only be the fence)...
		fpOnly(t, "stale token", h.FencedCreate(m, shard, 1, int64(i+1)))

		// ...yet reads keep working, carrying no token.
		if _, err := h.GetRaw(m); err != nil {
			t.Errorf("Get after fenced mutation #%d: code %s (%v)", i, harness.Code(err), err)
		}
		if got := h.List(); len(got) == 0 {
			t.Errorf("List after fenced mutation #%d returned nothing", i)
		}
		// A state-filtered List is also a pure read and never fences.
		if got := h.List(rest); len(got) == 0 {
			t.Errorf("state-filtered List after fenced mutation #%d returned nothing", i)
		}
	}

	// The fenced-out mutations never moved the machine: a fenced RPC is rejected
	// before any state change, so the machine still rests where it was.
	if got := h.State(m); got != rest {
		t.Errorf("fenced-out mutations changed state %s -> %s (a fenced RPC must be a no-op)", rest, got)
	}
}

// B306 — the fence runs BEFORE the idempotency short-circuit on Configure. A
// stale token replaying an already-applied Configure is rejected with
// FAILED_PRECONDITION, NOT served from the idempotency cache. This is the
// subtle ordering guarantee: idempotent replay must not become a fencing
// bypass. Deepened: we confirm the legitimate (fresh-token) idempotent replay
// IS still served (same op id), so the rejection is provably the fence and not
// a broken idempotency path.
func TestB306_FenceBeforeIdempotencyConfigure(t *testing.T) {
	behavior(t, "B306")
	h := dial(t)
	shard := h.UniqueShardID("b306")
	id := h.WalkToIdle()

	// Apply a fenced Configure at a high mark (5,5).
	ctx, cancel := h.Ctx()
	a1, err := h.Client.Configure(ctx, &pb.ConfigureRequest{
		MachineId: id, ClusterId: "b306", BootstrapBlob: []byte("# conf\n"),
		ShardId: shard, ShardEpoch: 5, SequenceNumber: 5,
		ShardMetadata: map[string]string{"k": "v"},
	})
	cancel()
	if err != nil {
		t.Fatalf("initial fenced Configure (5,5): %v", err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 15*time.Second)
	op1 := a1.GetOperationId()
	if op1 == "" {
		t.Fatalf("initial Configure ack carried an empty operation_id")
	}

	// A STALE-token replay of the same already-applied Configure: the fence
	// runs first, so it is rejected with FAILED_PRECONDITION rather than
	// short-circuited to the cached ack.
	ctx2, cancel2 := h.Ctx()
	_, errStale := h.Client.Configure(ctx2, &pb.ConfigureRequest{
		MachineId: id, ClusterId: "b306", BootstrapBlob: []byte("# conf\n"),
		ShardId: shard, ShardEpoch: 1, SequenceNumber: 1,
		ShardMetadata: map[string]string{"k": "v"},
	})
	cancel2()
	fpOnly(t, "stale-token replay of applied Configure", errStale)

	// A FRESH-token replay of the same op (strictly-newer mark) DOES pass the
	// fence and is served idempotently — same operation_id, still Configured.
	ctx3, cancel3 := h.Ctx()
	a3, errFresh := h.Client.Configure(ctx3, &pb.ConfigureRequest{
		MachineId: id, ClusterId: "b306", BootstrapBlob: []byte("# conf\n"),
		ShardId: shard, ShardEpoch: 5, SequenceNumber: 6,
		ShardMetadata: map[string]string{"k": "v"},
	})
	cancel3()
	if errFresh != nil {
		t.Fatalf("fresh-token idempotent Configure replay: %v", errFresh)
	}
	if got := a3.GetOperationId(); got != op1 {
		t.Errorf("fresh-token idempotent replay minted a new operation_id %q, want the original %q", got, op1)
	}
	if got := h.State(id); got != pb.MachineState_MACHINE_STATE_CONFIGURED {
		t.Errorf("machine left Configured after replays: %s", got)
	}
}

// B307 — the fence runs BEFORE the idempotency short-circuit on Drain. A
// stale-token Drain replay is rejected with FAILED_PRECONDITION. Symmetric to
// B306 but on the Configured->Idle edge, again confirming the fresh-token
// idempotent replay is still honoured.
func TestB307_FenceBeforeIdempotencyDrain(t *testing.T) {
	behavior(t, "B307")
	h := dial(t)
	shard := h.UniqueShardID("b307")
	id := h.WalkToConfigured("b307", map[string]string{"k": "v"})

	// Apply a fenced Drain at a high mark (5,5).
	ctx, cancel := h.Ctx()
	a1, err := h.Client.Drain(ctx, &pb.DrainRequest{
		MachineId: id, GracePeriodSeconds: 0, ShardId: shard, ShardEpoch: 5, SequenceNumber: 5,
	})
	cancel()
	if err != nil {
		t.Fatalf("initial fenced Drain (5,5): %v", err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)
	op1 := a1.GetOperationId()
	if op1 == "" {
		t.Fatalf("initial Drain ack carried an empty operation_id")
	}

	// STALE-token replay of the applied Drain: fenced first.
	ctx2, cancel2 := h.Ctx()
	_, errStale := h.Client.Drain(ctx2, &pb.DrainRequest{
		MachineId: id, GracePeriodSeconds: 0, ShardId: shard, ShardEpoch: 1, SequenceNumber: 1,
	})
	cancel2()
	fpOnly(t, "stale-token replay of applied Drain", errStale)

	// FRESH-token replay (strictly newer) is served idempotently: same op id,
	// still Idle.
	ctx3, cancel3 := h.Ctx()
	a3, errFresh := h.Client.Drain(ctx3, &pb.DrainRequest{
		MachineId: id, GracePeriodSeconds: 0, ShardId: shard, ShardEpoch: 5, SequenceNumber: 6,
	})
	cancel3()
	if errFresh != nil {
		t.Fatalf("fresh-token idempotent Drain replay: %v", errFresh)
	}
	if got := a3.GetOperationId(); got != op1 {
		t.Errorf("fresh-token idempotent Drain replay minted a new operation_id %q, want %q", got, op1)
	}
	if got := h.State(id); got != pb.MachineState_MACHINE_STATE_IDLE {
		t.Errorf("machine left Idle after Drain replays: %s", got)
	}
}

// B308 — an absent token bypasses fencing. A two-zero-token (shard_id empty,
// epoch=0, seq=0) Create followed by another zero-token Create are both
// accepted (the second as an idempotent no-op-at-Idle). Deepened: we confirm
// the machine actually reaches Idle, and that a THIRD zero-token call on
// another fresh machine is likewise unfenced — proving zero tokens never share
// or build a high-water mark.
func TestB308_ZeroTokenBypassesFencing(t *testing.T) {
	behavior(t, "B308")
	h := dial(t)
	id := h.PickSpeculative()

	// First zero-token Create (empty shard, 0, 0) — unfenced, starts the walk.
	accepted(t, "first zero-token Create", h.FencedCreate(id, "", 0, 0))
	h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)

	// Second zero-token Create — also accepted (idempotent no-op at Idle); a
	// zero token can NEVER be fenced, so even a "replay" passes.
	accepted(t, "second zero-token Create (idempotent)", h.FencedCreate(id, "", 0, 0))
	if got := h.State(id); got != pb.MachineState_MACHINE_STATE_IDLE {
		t.Errorf("zero-token replay disturbed Idle: %s", got)
	}

	// A zero token on a DIFFERENT fresh machine is independently accepted —
	// zero tokens establish no shared mark.
	id2 := h.PickSpeculative()
	accepted(t, "zero-token Create on a second machine", h.FencedCreate(id2, "", 0, 0))
	h.MustReach(id2, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)
}

// B309 — a ShardSession's monotonically auto-advancing tokens are accepted in
// order across Create/Configure/Drain on one machine, exercising the fence on
// every mutating RPC. Deepened: drive a full Idle<->Configured cycle TWICE
// through the same session so the seq advances across six mutating calls, each
// strictly newer than the last, and confirm each lands its expected state.
func TestB309_ShardSessionMonotonicAccepted(t *testing.T) {
	behavior(t, "B309")
	h := dial(t)
	sess := h.NewShard("b309")
	id := h.PickSpeculative()

	// Create with the session's first token.
	accepted(t, "session Create #1", sess.Do(harness.RPCCreate, id))
	h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)

	// Two Configure/Drain cycles, each call auto-advancing the session token.
	for cycle := 0; cycle < 2; cycle++ {
		accepted(t, "session Configure", sess.Do(harness.RPCConfigure, id))
		h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 15*time.Second)

		accepted(t, "session Drain", sess.Do(harness.RPCDrain, id))
		h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)
	}

	// The session has advanced past its starting seq across all the calls — a
	// replay of the very first (epoch=1, seq=1) token is now stale.
	fpOnly(t, "replay of the session's original token", sess.Stale(harness.RPCConfigure, id, 1, 1))
}

// B310 — after a ShardSession NewEpoch (epoch++, seq reset to 1), a replay of a
// pre-restart token is rejected with FAILED_PRECONDITION while the new-epoch
// token is accepted. Models a shard restart / leader change. Deepened: we
// capture the exact pre-restart token, drive the mark up within the old epoch
// first, then confirm BOTH the old high-seq token and the new-epoch low-seq
// token behave per the lexicographic rule.
func TestB310_NewEpochFencesOldReplay(t *testing.T) {
	behavior(t, "B310")
	h := dial(t)
	sess := h.NewShard("b310")
	id := h.PickSpeculative()

	// Drive the old epoch up several seqs so the mark sits well inside epoch 1.
	// Create -> Idle, Delete -> Speculative, Create -> Idle leaves the shard's
	// mark at (epoch=1, seq=3) and the machine back at Idle.
	accepted(t, "old-epoch Create (1,1)", sess.Do(harness.RPCCreate, id))
	h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)
	accepted(t, "old-epoch Delete (1,2)", sess.Do(harness.RPCDelete, id))
	h.MustReach(id, pb.MachineState_MACHINE_STATE_SPECULATIVE, 15*time.Second)
	accepted(t, "old-epoch Create (1,3)", sess.Do(harness.RPCCreate, id))
	h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)

	oldEpoch := sess.Epoch() // == 1; the mark is now (1,3)

	// Restart: NewEpoch bumps to epoch=2, seq resets to 1.
	sess.NewEpoch()
	if sess.Epoch() <= oldEpoch {
		t.Fatalf("NewEpoch did not advance epoch: was %d, now %d", oldEpoch, sess.Epoch())
	}

	// A replay of pre-restart tokens at or below the old mark (1,3) is rejected:
	// they are not strictly newer than the surviving high-water mark.
	fpOnly(t, "pre-restart token (1,1) replay after NewEpoch",
		sess.Stale(harness.RPCConfigure, id, oldEpoch, 1))
	fpOnly(t, "pre-restart token (1,3)==mark replay after NewEpoch",
		sess.Stale(harness.RPCConfigure, id, oldEpoch, 3))

	// The new-epoch token (epoch=2, seq=1) is accepted despite its LOW seq — a
	// higher epoch always wins and resets the sequence space, so the live shard
	// recovers immediately after the restart.
	accepted(t, "new-epoch Configure accepted", sess.Do(harness.RPCConfigure, id))
	h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 15*time.Second)
}

// B312 — for a randomized stream of (epoch, sequence) tokens on one shard,
// acceptance matches a monotonic-lexicographic oracle exactly: accept IFF the
// token is strictly greater than the running max, reject (FAILED_PRECONDITION)
// otherwise, and the running max only ever advances. Deterministic via -seed
// (replayable). This is the property-style stress of the fence rule.
func TestB312_RandomStreamMatchesOracle(t *testing.T) {
	behavior(t, "B312")
	h := dial(t)
	shard := h.UniqueShardID("b312")
	m := h.PickSpeculative()
	rng := h.Rand()

	// Running lexicographic max (the oracle). maxE<0 means "no mark yet", so the
	// first non-zero-shard token (even (0,0)) is accepted and establishes the
	// mark — matching providerkit's first-token-for-an-unknown-shard rule.
	maxE, maxS := int64(-1), int64(-1) // nothing accepted yet
	greater := func(e, s int64) bool {
		if maxE < 0 { // no mark yet: any token with a non-zero/zero seq is the first accept
			return true
		}
		if e != maxE {
			return e > maxE
		}
		return s > maxS
	}

	const steps = 200
	accepts := 0
	for i := 0; i < steps; i++ {
		// Generate tokens clustered near the current mark so we exercise both
		// accepts and rejects densely (pure-random large tokens would almost
		// always advance). Epoch jitter in [-1,+2], seq jitter in [-3,+4].
		var e, s int64
		if maxE < 0 {
			e, s = int64(rng.Intn(3)), int64(rng.Intn(3)) // first token small
		} else {
			e = maxE + int64(rng.Intn(4)) - 1
			if e < 0 {
				e = 0
			}
			s = maxS + int64(rng.Intn(8)) - 3
			if s < 0 {
				s = 0
			}
		}
		want := greater(e, s)
		err := h.FencedCreate(m, shard, e, s)
		if want {
			accepted(t, "oracle-accept token", err)
			maxE, maxS = e, s
			accepts++
		} else {
			fpOnly(t, "oracle-reject token", err)
		}
	}
	if accepts == 0 {
		t.Fatalf("oracle never accepted any token across %d steps — generator degenerate", steps)
	}
	t.Logf("B312: %d/%d tokens accepted, final mark (%d,%d)", accepts, steps, maxE, maxS)
}

// B311 — a zombie shard's passing token that establishes a mark, then fails its
// op against an out-of-position machine, STILL advances the high-water mark, so
// the zombie's own retry with that same (now-stale) token is fenced.
//
// This is pure black-box fencing — no fault injection. The contract: the fence
// runs FIRST and advances the mark for any strictly-newer token, BEFORE the
// position check, so even an op that is then rejected out-of-position
// (non-FAILED_PRECONDITION) has already moved the mark. We prove it by replaying
// a now-stale token and observing FAILED_PRECONDITION.
//
//  1. A fenced Drain (token 5,5) on a Speculative machine: Drain is
//     out-of-position from Speculative, so the op is rejected — but NOT with
//     FAILED_PRECONDITION (that code is reserved for fencing). The fence passed
//     and the mark advanced to (5,5).
//  2. A fenced Create with the now-stale token (5,4): strictly older than the
//     mark established in step 1, so it MUST be FAILED_PRECONDITION — proving the
//     mark advanced despite step 1's op being rejected.
func TestB311_MarkAdvancesEvenWhenOpRejected(t *testing.T) {
	behavior(t, "B311")
	h := dial(t)
	shard := h.UniqueShardID("b311")
	m := h.PickSpeculative()

	// Step 1: a higher token (5,5) on an out-of-position op. The fence passes
	// (advancing the mark), then the position check rejects the Drain — but
	// never with FAILED_PRECONDITION, which is reserved for fencing.
	err := h.FencedCall(harness.RPCDrain, m, shard, 5, 5)
	h.RejectsNonFencing("B311 out-of-position fenced Drain on Speculative (token 5,5)", err)

	// Step 2: replay a now-stale token (5,4) — strictly older than the mark the
	// passing-but-rejected op in step 1 established. It MUST be FAILED_PRECONDITION,
	// proving the mark advanced to (5,5) even though step 1's op was rejected.
	fpOnly(t, "B311 stale-token Create (5,4) after mark advanced to (5,5)",
		h.FencedCreate(m, shard, 5, 4))

	// And a strictly-newer token (5,6) still passes the fence (the mark is at
	// exactly (5,5), not higher), confirming step 1 advanced it to (5,5) — not
	// further — and the machine is genuinely fenceable forward.
	accepted(t, "B311 fresh token Create (5,6) past the (5,5) mark",
		h.FencedCreate(m, shard, 5, 6))
}
