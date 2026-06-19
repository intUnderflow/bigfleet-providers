//go:build certify

package suite

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"

	"github.com/intUnderflow/bigfleet-providers/conformance/internal/harness"
)

// C4 — Concurrency & Idempotency (behaviors B40x).
//
// These deepen the upstream happy-path: every behavior fires a PARALLEL burst of
// retries (or a deliberate race) and asserts the provider serializes them into a
// single well-defined effect. The contract under test:
//
//   - Idempotency is keyed on (machine_id, target_state): N racing identical
//     retries collapse to exactly ONE distinct non-empty operation_id and the
//     target is reached exactly once (never N times, never a torn intermediate).
//   - Conflicting mutations serialize: the machine lands in exactly one legal
//     stable state, never a torn/partial one.
//   - Fencing still arbitrates concurrent zombie vs live tokens.
//
// Hard rules honored throughout:
//   - Black-box: 6 RPCs + gRPC codes only.
//   - FAILED_PRECONDITION is asserted ONLY for fencing; every other rejection
//     goes through RejectsNonFencing.
//   - Async: after a mutation we MustReach the stable target; we never assume
//     synchronous completion.
//   - The fake is fast: transitional windows may be unobservable, so any
//     transitional assertion is "if observed it was the right one", never
//     "must be observed".
//   - List is an unordered set: membership/count only.
//   - operation_id: stability/equality only, never literal value/format.

// fanN is the retry burst width. Big enough to actually race the provider's
// serialization, small enough to keep the run cheap.
const fanN = 16

// assertReachedExactlyOnce confirms, via the unordered List set, that the
// machine appears exactly once in the want-state filter (it settled there once,
// not duplicated by the racing retries) and that a direct Get agrees.
func assertReachedExactlyOnce(t *testing.T, h *harness.H, id string, want pb.MachineState) {
	t.Helper()
	m := h.MustReach(id, want, 15*time.Second)
	if m.GetState() != want {
		t.Fatalf("machine %s settled in %s, want %s", id, m.GetState(), want)
	}
	count := 0
	for _, mm := range h.List(want) {
		if mm.GetId() == id {
			count++
		}
	}
	if count != 1 {
		t.Errorf("machine %s appears %d times in List(%s); a settled idempotent retry must yield exactly one", id, count, want)
	}
}

// assertSnapshotStateConsistent asserts the no-torn-snapshot invariant for a
// Configure burst: every SUCCEEDING ack's embedded machine snapshot reports a
// TARGET-BOUND state — either the settled target (CONFIGURED) or its single
// bound transitional (CONFIGURING) — and NEVER a foreign state (any other
// transitional, or a stable state outside the target binding). The two
// target-bound values may legitimately coexist across the burst (some acks land
// pre-settle, some post-settle), so we do NOT require a single global value;
// what would be "torn" is a snapshot whose state is neither the target nor its
// bound transitional. Additionally, a snapshot that already claims the settled
// target must carry the requested cluster binding (state and payload agree, not
// torn). Tolerant of an absent snapshot (provider may omit it).
func assertSnapshotStateConsistent(t *testing.T, results []harness.AckErr, target, transitional pb.MachineState, wantCluster string) {
	t.Helper()
	seen := map[pb.MachineState]int{}
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		snap := r.Ack.GetMachine()
		if snap == nil {
			continue // snapshot is best-effort; absence is not a torn snapshot.
		}
		s := snap.GetState()
		if s != target && s != transitional {
			t.Errorf("ack snapshot reported %s, want target %s or its bound transitional %s (foreign/torn snapshot)", s, target, transitional)
			continue
		}
		seen[s]++
		// A snapshot already in the settled target must agree on the binding.
		if s == target && snap.GetCluster() != wantCluster {
			t.Errorf("CONFIGURED snapshot carries cluster %q, want %q (state/payload torn)", snap.GetCluster(), wantCluster)
		}
	}
	// Every observed value must be one of the two target-bound states (the loop
	// above already errored otherwise); record for diagnostics only.
	if len(seen) == 0 {
		t.Logf("no ack carried a machine snapshot (provider omits it); torn-snapshot invariant vacuously held")
	}
}

// --- B401: parallel Create retries on one Speculative machine ---------------

func TestB401_ParallelCreateRetries(t *testing.T) {
	behavior(t, "B401")
	h := dial(t)
	id := h.PickSpeculative()

	results := h.FanoutCreate(id, fanN)
	// Exactly one distinct non-empty operation_id across all succeeding retries.
	h.AssertSingleOperationID("B401 parallel Create", results)
	// No succeeding retry was rejected with a non-fencing code masquerading as a
	// race failure: a Create retry must either succeed idempotently or, if it
	// loses the race transiently, still not surface FAILED_PRECONDITION (reserved
	// for fencing) — these calls carry no token, so any FP would be a bug.
	for i, r := range results {
		if r.Err != nil && harness.Code(r.Err) == codes.FailedPrecondition {
			t.Errorf("retry %d: unfenced Create rejected FAILED_PRECONDITION (reserved for fencing): %v", i, r.Err)
		}
	}
	// Settles in Idle exactly once.
	assertReachedExactlyOnce(t, h, id, pb.MachineState_MACHINE_STATE_IDLE)
}

// --- B402: parallel identical Configure retries on one Idle machine ---------

func TestB402_ParallelConfigureRetries(t *testing.T) {
	behavior(t, "B402")
	h := dial(t)
	id := h.WalkToIdle()
	md := map[string]string{"shard": "b402", "role": "primary"}

	results := h.FanoutConfigure(id, "b402-cluster", md, fanN)
	h.AssertSingleOperationID("B402 parallel Configure", results)
	for i, r := range results {
		if r.Err != nil && harness.Code(r.Err) == codes.FailedPrecondition {
			t.Errorf("retry %d: unfenced Configure rejected FAILED_PRECONDITION (reserved for fencing): %v", i, r.Err)
		}
	}
	assertReachedExactlyOnce(t, h, id, pb.MachineState_MACHINE_STATE_CONFIGURED)

	// The single applied effect is the requested binding (not a torn/partial one).
	m := h.Get(id)
	if m.GetCluster() != "b402-cluster" {
		t.Errorf("settled cluster %q, want %q", m.GetCluster(), "b402-cluster")
	}
	for k, v := range md {
		if got := m.GetShardMetadata()[k]; got != v {
			t.Errorf("settled shard_metadata[%q]=%q, want %q", k, got, v)
		}
	}
}

// --- B403: parallel identical Drain retries on one Configured machine -------

func TestB403_ParallelDrainRetries(t *testing.T) {
	behavior(t, "B403")
	h := dial(t)
	id := h.WalkToConfigured("b403-cluster", map[string]string{"k": "v"})

	results := harness.Fanout(fanN, func(int) harness.AckErr {
		ack, err := h.Drain(id, 0)
		return harness.AckErr{Ack: ack, Err: err}
	})
	h.AssertSingleOperationID("B403 parallel Drain", results)
	for i, r := range results {
		if r.Err != nil && harness.Code(r.Err) == codes.FailedPrecondition {
			t.Errorf("retry %d: unfenced Drain rejected FAILED_PRECONDITION (reserved for fencing): %v", i, r.Err)
		}
	}
	assertReachedExactlyOnce(t, h, id, pb.MachineState_MACHINE_STATE_IDLE)

	// Drain to Idle clears cluster and metadata exactly once.
	m := h.Get(id)
	if m.GetCluster() != "" {
		t.Errorf("cluster %q survived parallel Drain", m.GetCluster())
	}
	if len(m.GetShardMetadata()) != 0 {
		t.Errorf("shard_metadata %v survived parallel Drain", m.GetShardMetadata())
	}
}

// --- B404: two racing conflicting mutations serialize -----------------------

func TestB404_RacingConflictingMutations(t *testing.T) {
	behavior(t, "B404")
	h := dial(t)
	// Start from Configured so Configure (already-at-target idempotent no-op) and
	// Drain (the legal forward edge to Idle) genuinely conflict on one machine.
	id := h.WalkToConfigured("b404-cluster", map[string]string{"k": "v"})

	// Fire both at once. We don't care which wins; we require the machine lands in
	// exactly one well-defined STABLE state (Idle if Drain won, Configured if the
	// idempotent Configure won/Drain lost) and never a torn/partial state, and
	// that no rejection is a (reserved) fencing code.
	type res struct {
		which string
		err   error
	}
	out := harness.Fanout(2, func(i int) res {
		if i == 0 {
			_, err := h.Configure(id, "b404-cluster", map[string]string{"k": "v"})
			return res{"Configure", err}
		}
		_, err := h.Drain(id, 0)
		return res{"Drain", err}
	})
	for _, r := range out {
		if r.err != nil {
			h.RejectsNonFencing("B404 "+r.which+" loser", r.err)
		}
	}

	// Poll until the machine rests in a stable state, then assert it is one of the
	// two legal outcomes and stays there (serialized to a single settled effect).
	settled := waitStable(t, h, id, 15*time.Second)
	if settled != pb.MachineState_MACHINE_STATE_IDLE && settled != pb.MachineState_MACHINE_STATE_CONFIGURED {
		t.Fatalf("racing Configure/Drain settled in %s, want exactly one of Idle/Configured", settled)
	}
	h.StaysIn(id, settled, 300*time.Millisecond)

	// Cross-field consistency of the single winning effect.
	m := h.Get(id)
	switch settled {
	case pb.MachineState_MACHINE_STATE_IDLE: // Drain won: binding cleared.
		if m.GetCluster() != "" {
			t.Errorf("Drain won but cluster %q lingers", m.GetCluster())
		}
	case pb.MachineState_MACHINE_STATE_CONFIGURED: // Configure won / Drain lost: binding intact.
		if m.GetCluster() != "b404-cluster" {
			t.Errorf("Configured outcome but cluster=%q, want b404-cluster", m.GetCluster())
		}
	}
}

// --- B405: concurrent zombie (old epoch) vs live (new epoch) tokens ---------

func TestB405_ZombieVsLiveEpoch(t *testing.T) {
	behavior(t, "B405")
	h := dial(t)
	id := h.PickSpeculative()
	shard := h.UniqueShardID("b405")

	// Establish a HIGH high-water mark at (epoch=1, seq=1000) on this shard via an
	// accepted Create. (Create is idempotent on target=Idle, so the token is the
	// only thing that can fence a same-shard call.) Picking a high seq makes every
	// zombie below STRICTLY older than the mark under ANY interleaving with the
	// live epoch-2 tokens — there is no window in which a zombie's seq exceeds the
	// running mark.
	const markSeq = 1000
	if err := h.FencedCreate(id, shard, 1, markSeq); err != nil {
		t.Fatalf("establish mark (1,%d): %v", markSeq, err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)

	// Now race a stream of zombie (epoch=1, seq strictly < markSeq — older than the
	// mark in the SAME epoch) tokens against live (epoch=2 — strictly newer, the
	// higher epoch dominates regardless of sequence) tokens, concurrently, on the
	// same shard+machine. The live epoch must ALWAYS win (accepted) and every
	// zombie must ALWAYS be FAILED_PRECONDITION.
	const rounds = 12
	type tok struct {
		zombie     bool
		epoch, seq int64
	}
	calls := make([]tok, 0, rounds*2)
	for i := int64(0); i < rounds; i++ {
		calls = append(calls, tok{zombie: true, epoch: 1, seq: i})        // strictly < markSeq; always stale
		calls = append(calls, tok{zombie: false, epoch: 2, seq: 100 + i}) // epoch 2 dominates
	}
	results := harness.Fanout(len(calls), func(i int) harness.AckErr {
		err := h.FencedCreate(id, shard, calls[i].epoch, calls[i].seq)
		return harness.AckErr{Err: err}
	})
	liveAccepted := 0
	for i, r := range results {
		if calls[i].zombie {
			if harness.Code(r.Err) != codes.FailedPrecondition {
				t.Errorf("zombie token (%d,%d): code %s, want FAILED_PRECONDITION", calls[i].epoch, calls[i].seq, harness.Code(r.Err))
			}
			continue
		}
		// Live (epoch=2) token: must NOT be fenced. It either advances the mark
		// (accepted) or loses a race to a strictly-higher live seq that already
		// advanced past it — but that loser would itself be a strictly-older
		// epoch-2 token relative to the new mark, so a FAILED_PRECONDITION among
		// the live batch is legal ONLY if some other live token already advanced.
		if r.Err == nil {
			liveAccepted++
		} else if harness.Code(r.Err) != codes.FailedPrecondition {
			t.Errorf("live token (%d,%d): code %s, want OK or FAILED_PRECONDITION-vs-newer-live", calls[i].epoch, calls[i].seq, harness.Code(r.Err))
		}
	}
	if liveAccepted == 0 {
		t.Errorf("no live (epoch=2) token was accepted; the higher epoch must win at least once")
	}
	// After the storm, a fresh below-mark zombie is still fenced, and a strictly
	// newer live token is still accepted: the mark advanced into epoch 2.
	if err := h.FencedCreate(id, shard, 1, 9); harness.Code(err) != codes.FailedPrecondition {
		t.Errorf("post-storm zombie (1,9): code %s, want FAILED_PRECONDITION", harness.Code(err))
	}
	if err := h.FencedCreate(id, shard, 2, 9999); err != nil {
		t.Errorf("post-storm fresh live (2,9999): %v, want accepted", err)
	}
}

// --- B406: idempotency keyed on (machine_id, target_state) across conns -----

func TestB406_IdempotencyAcrossConnections(t *testing.T) {
	behavior(t, "B406")
	// Two INDEPENDENT connections to the same provider. Idempotency must be keyed
	// on (machine_id, target_state) in the provider, not on a per-connection
	// channel, so retries fired across distinct connections still collapse to one
	// operation_id.
	h1 := dial(t)
	h2 := dial(t)

	id := h1.WalkToIdle()
	md := map[string]string{"k": "v"}

	// Half the burst on conn1, half on conn2, all identical Configure(target=Configured).
	results := harness.Fanout(fanN, func(i int) harness.AckErr {
		hh := h1
		if i%2 == 1 {
			hh = h2
		}
		ack, err := hh.Configure(id, "b406-cluster", md)
		return harness.AckErr{Ack: ack, Err: err}
	})
	// Collapses to one operation_id even split across two connections.
	h1.AssertSingleOperationID("B406 cross-connection Configure", results)
	for i, r := range results {
		if r.Err != nil && harness.Code(r.Err) == codes.FailedPrecondition {
			t.Errorf("retry %d: unfenced Configure rejected FAILED_PRECONDITION: %v", i, r.Err)
		}
	}
	assertReachedExactlyOnce(t, h1, id, pb.MachineState_MACHINE_STATE_CONFIGURED)

	// And a fresh idempotent replay from EITHER connection still reuses that id.
	a1, err := h1.Configure(id, "b406-cluster", md)
	if err != nil {
		t.Fatalf("post-settle replay conn1: %v", err)
	}
	a2, err := h2.Configure(id, "b406-cluster", md)
	if err != nil {
		t.Fatalf("post-settle replay conn2: %v", err)
	}
	if a1.GetOperationId() == "" || a1.GetOperationId() != a2.GetOperationId() {
		t.Errorf("idempotent replay op-ids differ across connections: conn1=%q conn2=%q (must be equal and non-empty)", a1.GetOperationId(), a2.GetOperationId())
	}
}

// --- B407: K machines driven to Idle concurrently, no cross-bleed ----------

func TestB407_KMachinesConcurrentToIdle(t *testing.T) {
	behavior(t, "B407")
	h := dial(t)
	const k = 8
	ids := h.PickNSpeculative(k)

	// One Create per machine, all in parallel.
	results := harness.Fanout(k, func(i int) harness.AckErr {
		ack, err := h.Create(ids[i])
		return harness.AckErr{Ack: ack, Err: err}
	})

	// Each independent machine's first transition mints a DISTINCT operation_id —
	// no cross-machine id collision (these are different (machine_id,target) keys).
	distinct := map[string]string{} // opID -> machineId that minted it
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("Create(%s): %v", ids[i], r.Err)
		}
		op := r.Ack.GetOperationId()
		if op == "" {
			t.Errorf("Create(%s) ack carried empty operation_id", ids[i])
			continue
		}
		if other, dup := distinct[op]; dup {
			t.Errorf("operation_id %q reused across distinct machines %s and %s (cross-machine bleed)", op, other, ids[i])
		}
		distinct[op] = ids[i]
	}

	// Each independently reaches Idle; no machine NOT in our set was disturbed
	// into our cluster (no effect bleed): a spot-check that every target settles
	// and carries no spurious cluster binding from a sibling.
	for _, id := range ids {
		m := h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 30*time.Second)
		if m.GetCluster() != "" {
			t.Errorf("machine %s reached Idle but carries cluster %q (cross-bleed)", id, m.GetCluster())
		}
		if len(m.GetShardMetadata()) != 0 {
			t.Errorf("machine %s reached Idle but carries metadata %v (cross-bleed)", id, m.GetShardMetadata())
		}
	}
}

// --- B408: no torn snapshot across a parallel burst -------------------------

func TestB408_NoTornSnapshotInBurst(t *testing.T) {
	behavior(t, "B408")
	h := dial(t)
	id := h.WalkToIdle()
	md := map[string]string{"k": "v"}

	// Parallel identical Configure burst. Every succeeding ack's embedded snapshot
	// must report a TARGET-BOUND state: either CONFIGURED (settled) or CONFIGURING
	// (its bound transitional) — never another transitional, never a foreign state.
	// The two target-bound values may coexist across the burst (some acks land
	// pre-settle, some post-settle); the torn-snapshot failure is a state outside
	// that bound pair, or a CONFIGURED snapshot whose cluster payload disagrees.
	results := h.FanoutConfigure(id, "b408-cluster", md, fanN)
	h.AssertSingleOperationID("B408 burst", results)
	assertSnapshotStateConsistent(t, results,
		pb.MachineState_MACHINE_STATE_CONFIGURED,
		pb.MachineState_MACHINE_STATE_CONFIGURING,
		"b408-cluster")

	assertReachedExactlyOnce(t, h, id, pb.MachineState_MACHINE_STATE_CONFIGURED)
}

// --- B409: parallel Delete retries on one Idle machine (cap-gated) ----------

func TestB409_ParallelDeleteRetries(t *testing.T) {
	behavior(t, "B409")
	h := dial(t)
	if !h.Probe().Delete {
		t.Skip("provider does not implement Delete; B409 N/A")
	}
	id := h.WalkToIdle()

	results := harness.Fanout(fanN, func(int) harness.AckErr {
		ack, err := h.Delete(id)
		return harness.AckErr{Ack: ack, Err: err}
	})
	h.AssertSingleOperationID("B409 parallel Delete", results)
	for i, r := range results {
		if r.Err != nil && harness.Code(r.Err) == codes.FailedPrecondition {
			t.Errorf("retry %d: unfenced Delete rejected FAILED_PRECONDITION (reserved for fencing): %v", i, r.Err)
		}
	}
	assertReachedExactlyOnce(t, h, id, pb.MachineState_MACHINE_STATE_SPECULATIVE)
}

// waitStable polls Get until the machine rests in any stable state (or timeout),
// returning the settled state. Used where the winning outcome of a race is one
// of several legal stable states (we can't MustReach a single known one).
func waitStable(t *testing.T, h *harness.H, id string, timeout time.Duration) pb.MachineState {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last pb.MachineState
	for time.Now().Before(deadline) {
		s, err := h.StateRaw(id)
		if err == nil {
			last = s
			if harness.IsStable(s) {
				return s
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("machine %s did not reach a stable state within %s (last %s)", id, timeout, last)
	return last
}
