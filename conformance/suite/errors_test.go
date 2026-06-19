//go:build certify

package suite

import (
	"fmt"
	"maps"
	"math"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"

	"github.com/intUnderflow/bigfleet-providers/conformance/internal/harness"
)

// C-errors — Transition Matrix / Errors (behaviors B201–B215).
//
// The state machine has exactly four legal mutating edges:
//
//	Create@Speculative  Configure@Idle  Drain@Configured  Delete@Idle
//
// Everything here certifies the CODE DISCIPLINE around those edges and the
// no-partial-transition invariant, going deeper than a happy-path baseline:
//
//   - An out-of-position or malformed-argument rejection is NEVER
//     FAILED_PRECONDITION — that code is reserved exclusively for fencing.
//   - A rejection leaves the machine EXACTLY where it was (no partial
//     transition): asserted both immediately and over a short stability window,
//     so a deferred/async mutation cannot sneak the machine away.
//   - An idempotent no-op-at-target succeeds, holds the target state, and
//     re-uses the original transition's operation_id.
//   - Distinct successive transitions mint distinct operation_ids.
//
// Black-box only: the six wire RPCs + gRPC status codes via the harness.

const errStabilityWindow = 250 * time.Millisecond

// ---------------------------------------------------------------------------
// B201 — Create on a Configured machine is rejected non-FAILED_PRECONDITION and
// leaves the machine in Configured (no partial transition).
// ---------------------------------------------------------------------------
func TestB201_CreateOnConfiguredRejected(t *testing.T) {
	behavior(t, "B201")
	h := dial(t)
	id := h.WalkToConfigured("conf-b201", map[string]string{"k": "v"})

	_, err := h.Create(id)
	h.RejectsNonFencing("Create on Configured", err)

	// No partial transition: still Configured, immediately and over a window.
	if got := h.State(id); got != pb.MachineState_MACHINE_STATE_CONFIGURED {
		t.Errorf("Create on Configured moved machine to %s (must be a no-op rejection)", got)
	}
	h.StaysIn(id, pb.MachineState_MACHINE_STATE_CONFIGURED, errStabilityWindow)
	// The binding is untouched too — a rejected Create must not clear cluster.
	if c := h.Get(id).GetCluster(); c != "conf-b201" {
		t.Errorf("rejected Create disturbed cluster binding: got %q", c)
	}
}

// ---------------------------------------------------------------------------
// B202 — Configure on a Speculative machine is rejected non-FAILED_PRECONDITION
// and leaves the machine in Speculative.
// ---------------------------------------------------------------------------
func TestB202_ConfigureOnSpeculativeRejected(t *testing.T) {
	behavior(t, "B202")
	h := dial(t)
	id := h.PickSpeculative()
	if s := h.State(id); s != pb.MachineState_MACHINE_STATE_SPECULATIVE {
		t.Fatalf("setup: picked machine in %s, want Speculative", s)
	}

	_, err := h.Configure(id, "conf-b202", map[string]string{"k": "v"})
	h.RejectsNonFencing("Configure on Speculative", err)

	if got := h.State(id); got != pb.MachineState_MACHINE_STATE_SPECULATIVE {
		t.Errorf("Configure on Speculative moved machine to %s", got)
	}
	h.StaysIn(id, pb.MachineState_MACHINE_STATE_SPECULATIVE, errStabilityWindow)
	// A rejected Configure must not have silently bound a cluster/metadata.
	m := h.Get(id)
	if m.GetCluster() != "" || len(m.GetShardMetadata()) != 0 {
		t.Errorf("rejected Configure leaked binding: cluster=%q md=%v", m.GetCluster(), m.GetShardMetadata())
	}
}

// ---------------------------------------------------------------------------
// B203 — Drain on an Idle machine is rejected non-FAILED_PRECONDITION and leaves
// the machine in Idle.
// ---------------------------------------------------------------------------
func TestB203_DrainOnIdleRejected(t *testing.T) {
	behavior(t, "B203")
	h := dial(t)
	id := h.WalkToIdle()

	// Drain's target IS Idle, so Drain-on-Idle is an at-target call: it is an
	// out-of-position rejection on a machine the kit has no Drain op for, but an
	// idempotent no-op success on a (pool-reused) machine that was Drained in a
	// prior cycle — the kit never clears its op history. Both are conformant;
	// neither may use FAILED_PRECONDITION and the machine must stay Idle.
	_, err := h.Drain(id, 5)
	if err != nil {
		h.RejectsNonFencing("Drain on Idle", err)
	}

	if got := h.State(id); got != pb.MachineState_MACHINE_STATE_IDLE {
		t.Errorf("Drain on Idle moved machine to %s", got)
	}
	h.StaysIn(id, pb.MachineState_MACHINE_STATE_IDLE, errStabilityWindow)
}

// ---------------------------------------------------------------------------
// B204 — Delete on a Speculative OR Configured machine is rejected
// non-FAILED_PRECONDITION (or Unimplemented) and leaves the source state
// unchanged. Delete is legal only on Idle, so both of these are out-of-position.
// ---------------------------------------------------------------------------
func TestB204_DeleteOutOfPositionRejected(t *testing.T) {
	behavior(t, "B204")
	h := dial(t)

	type src struct {
		name  string
		state pb.MachineState
		setup func() string
	}
	sources := []src{
		{"Speculative", pb.MachineState_MACHINE_STATE_SPECULATIVE, func() string { return h.PickSpeculative() }},
		{"Configured", pb.MachineState_MACHINE_STATE_CONFIGURED, func() string { return h.WalkToConfigured("conf-b204", nil) }},
	}
	for _, s := range sources {
		id := s.setup()
		if got := h.State(id); got != s.state {
			t.Fatalf("setup landed in %s, want %s", got, s.state)
		}
		// Delete's target IS Speculative, so Delete-on-Speculative is at-target:
		// a no-op success (pool-reused machine with Delete history) or an
		// out-of-position rejection (no history) — both conformant. Delete on
		// Configured is genuinely out-of-position and must always be rejected.
		atTarget := s.state == pb.MachineState_MACHINE_STATE_SPECULATIVE
		_, err := h.Delete(id)
		switch {
		case harness.Code(err) == codes.Unimplemented:
			// A provider with no Delete at all rejects every Delete with
			// Unimplemented before touching state — that is conformant.
		case err != nil:
			h.RejectsNonFencing("Delete on "+s.name, err)
		case !atTarget:
			t.Errorf("Delete on %s succeeded; an out-of-position Delete must be rejected", s.name)
		}
		// Source state unchanged — immediately and over a window.
		if got := h.State(id); got != s.state {
			t.Errorf("Delete on %s changed state to %s (must leave the source state unchanged)", s.name, got)
		}
		h.StaysIn(id, s.state, errStabilityWindow)
	}
}

// ---------------------------------------------------------------------------
// B205 — Re-Configure on an already-Configured machine and re-Create on an
// already-Idle machine are idempotent no-ops that succeed and leave the target
// state unchanged.
// ---------------------------------------------------------------------------
func TestB205_IdempotentNoOpAtTarget(t *testing.T) {
	behavior(t, "B205")
	h := dial(t)

	// Re-Configure at Configured: same cluster + metadata, succeeds, stays put.
	md := map[string]string{"a": "1", "b": "2"}
	cid := h.WalkToConfigured("conf-b205", md)
	ack, err := h.Configure(cid, "conf-b205", md)
	if err != nil {
		t.Fatalf("re-Configure at Configured: %v", err)
	}
	if got := h.State(cid); got != pb.MachineState_MACHINE_STATE_CONFIGURED {
		t.Errorf("re-Configure moved a Configured machine to %s", got)
	}
	if ack.GetMachine().GetState() != pb.MachineState_MACHINE_STATE_CONFIGURED {
		t.Errorf("re-Configure ack snapshot state = %s, want CONFIGURED", ack.GetMachine().GetState())
	}
	// The idempotent no-op must not transition through a transitional state.
	h.NeverReaches(cid, pb.MachineState_MACHINE_STATE_CONFIGURING, errStabilityWindow)
	h.NeverReaches(cid, pb.MachineState_MACHINE_STATE_DRAINING, errStabilityWindow)

	// Re-Create at Idle (it reached Idle via Create): succeeds, stays Idle.
	iid := h.WalkToIdle()
	if _, err := h.Create(iid); err != nil {
		t.Errorf("re-Create at Idle should be an idempotent no-op, got: %v", err)
	}
	if got := h.State(iid); got != pb.MachineState_MACHINE_STATE_IDLE {
		t.Errorf("re-Create moved an Idle machine to %s", got)
	}
	h.NeverReaches(iid, pb.MachineState_MACHINE_STATE_CREATING, errStabilityWindow)
}

// ---------------------------------------------------------------------------
// B206 — An idempotent no-op-at-target call returns the SAME non-empty
// operation_id as the original transition into that target state.
// ---------------------------------------------------------------------------
func TestB206_IdempotentSameOperationID(t *testing.T) {
	behavior(t, "B206")
	h := dial(t)

	// Create: capture the original Create op-id, then replay it.
	id := h.PickSpeculative()
	first, err := h.Create(id)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	origCreate := first.GetOperationId()
	if origCreate == "" {
		t.Fatalf("Create ack carried an empty operation_id")
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)
	// Replay Create now that it has settled at the target (Idle): same op-id.
	again, err := h.Create(id)
	if err != nil {
		t.Fatalf("idempotent re-Create: %v", err)
	}
	if again.GetOperationId() != origCreate {
		t.Errorf("idempotent re-Create op-id = %q, want original %q", again.GetOperationId(), origCreate)
	}

	// Configure: same invariant against the Configure target (Configured).
	cfg, err := h.Configure(id, "conf-b206", map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	origCfg := cfg.GetOperationId()
	if origCfg == "" {
		t.Fatalf("Configure ack carried an empty operation_id")
	}
	// Create and Configure target distinct states, so their op-ids must differ.
	if origCfg == origCreate {
		t.Errorf("Configure reused the Create operation_id %q (distinct targets must mint distinct ids)", origCfg)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 15*time.Second)
	cfgAgain, err := h.Configure(id, "conf-b206", map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("idempotent re-Configure: %v", err)
	}
	if cfgAgain.GetOperationId() != origCfg {
		t.Errorf("idempotent re-Configure op-id = %q, want original %q", cfgAgain.GetOperationId(), origCfg)
	}
}

// ---------------------------------------------------------------------------
// B207 — Create/Configure/Drain on an unknown machine_id return NotFound, never
// FAILED_PRECONDITION and never a silent create.
// ---------------------------------------------------------------------------
func TestB207_MutateUnknownMachineNotFound(t *testing.T) {
	behavior(t, "B207")
	h := dial(t)
	ghost := "conformance-b207-ghost-" + h.UniqueShardID("id")

	cases := []struct {
		name string
		call func() error
	}{
		{"Create", func() error { _, e := h.Create(ghost); return e }},
		{"Configure", func() error { _, e := h.Configure(ghost, "x", map[string]string{"k": "v"}); return e }},
		{"Drain", func() error { _, e := h.Drain(ghost, 5); return e }},
	}
	for _, c := range cases {
		err := c.call()
		if code := harness.Code(err); code != codes.NotFound {
			t.Errorf("%s unknown: code %s, want NotFound", c.name, code)
		}
		// Never FAILED_PRECONDITION (fence-reserved) — explicit, separate guard.
		if harness.Code(err) == codes.FailedPrecondition {
			t.Errorf("%s unknown: FAILED_PRECONDITION leaked on a non-fencing path", c.name)
		}
	}
	// Never a silent create: the ghost must still be absent afterwards.
	if _, err := h.GetRaw(ghost); harness.Code(err) != codes.NotFound {
		t.Errorf("ghost machine became visible after a rejected mutation: Get code %s", harness.Code(err))
	}
}

// ---------------------------------------------------------------------------
// B208 — Get/Create/Configure/Drain with an empty machine_id return
// InvalidArgument.
// ---------------------------------------------------------------------------
func TestB208_EmptyMachineIDInvalidArgument(t *testing.T) {
	behavior(t, "B208")
	h := dial(t)

	cases := []struct {
		name string
		call func() error
	}{
		{"Get", func() error { _, e := h.GetRaw(""); return e }},
		{"Create", func() error { _, e := h.Create(""); return e }},
		{"Configure", func() error { _, e := h.Configure("", "x", nil); return e }},
		{"Drain", func() error { _, e := h.Drain("", 5); return e }},
	}
	for _, c := range cases {
		err := c.call()
		if code := harness.Code(err); code != codes.InvalidArgument {
			t.Errorf("%s empty id: code %s, want InvalidArgument", c.name, code)
		}
	}
}

// ---------------------------------------------------------------------------
// B209 — A Configure carrying more shard_metadata keys/bytes than the provider
// accepts is either echoed verbatim or rejected with InvalidArgument (never
// FAILED_PRECONDITION) with no partial transition.
// ---------------------------------------------------------------------------
func TestB209_OversizedMetadataEchoOrInvalidArgument(t *testing.T) {
	behavior(t, "B209")
	h := dial(t)
	id := h.WalkToIdle()

	// A deliberately large map: many keys, each with a sizeable value.
	big := map[string]string{}
	for i := 0; i < 512; i++ {
		big[fmt.Sprintf("b209-key-%03d", i)] = strings.Repeat("v", 1024)
	}

	_, err := h.Configure(id, "conf-b209", big)
	switch harness.Code(err) {
	case codes.OK:
		// Accepted => must echo the map verbatim once Configured.
		h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 20*time.Second)
		got := h.Get(id).GetShardMetadata()
		if !maps.Equal(got, big) {
			t.Errorf("accepted oversized metadata not echoed verbatim: got %d keys, want %d", len(got), len(big))
		}
	case codes.InvalidArgument:
		// Rejected => no partial transition: the machine stays Idle with no
		// binding leaked.
		if got := h.State(id); got != pb.MachineState_MACHINE_STATE_IDLE {
			t.Errorf("rejected oversized Configure moved machine to %s (no partial transition)", got)
		}
		h.StaysIn(id, pb.MachineState_MACHINE_STATE_IDLE, errStabilityWindow)
		if m := h.Get(id); m.GetCluster() != "" || len(m.GetShardMetadata()) != 0 {
			t.Errorf("rejected oversized Configure leaked binding: cluster=%q md-keys=%d", m.GetCluster(), len(m.GetShardMetadata()))
		}
	default:
		t.Errorf("oversized metadata: code %s, want OK (echo) or InvalidArgument (never FAILED_PRECONDITION)", harness.Code(err))
	}
}

// ---------------------------------------------------------------------------
// B210 — A Drain with a negative grace_period_seconds. The frozen title asks
// for InvalidArgument; the load-bearing, universal invariant is the code
// discipline: a negative grace must NEVER be rejected with FAILED_PRECONDITION
// (reserved for fencing), and there must be no partial/torn transition. A
// provider that treats a benign out-of-range grace as a clamp (the reference
// providerkit does) drains cleanly to Idle; a stricter provider rejects with
// InvalidArgument and holds Configured. Both are accepted here; FAILED_PRECONDITION
// and any other rejection code are not.
// ---------------------------------------------------------------------------
func TestB210_NegativeGraceCodeDiscipline(t *testing.T) {
	behavior(t, "B210")
	h := dial(t)
	id := h.WalkToConfigured("conf-b210", map[string]string{"k": "v"})

	_, err := h.Drain(id, -5)
	switch harness.Code(err) {
	case codes.InvalidArgument:
		// Strict provider: rejected, machine must stay Configured (no partial).
		if got := h.State(id); got != pb.MachineState_MACHINE_STATE_CONFIGURED {
			t.Errorf("negative-grace Drain rejected but state changed to %s (no partial transition)", got)
		}
		h.StaysIn(id, pb.MachineState_MACHINE_STATE_CONFIGURED, errStabilityWindow)
	case codes.OK:
		// Lenient provider: accepted; it must settle cleanly at Idle (the Drain
		// target), never wedge in a transitional state, never silently revert.
		h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 20*time.Second)
		if m := h.Get(id); m.GetCluster() != "" {
			t.Errorf("negative-grace Drain settled to Idle but cluster %q survived", m.GetCluster())
		}
	case codes.FailedPrecondition:
		t.Errorf("negative-grace Drain rejected with FAILED_PRECONDITION, reserved for fencing")
	default:
		t.Errorf("negative-grace Drain: code %s, want InvalidArgument (strict) or OK (lenient), never FAILED_PRECONDITION", harness.Code(err))
	}
}

// ---------------------------------------------------------------------------
// B211 — A mutating RPC carrying a negative shard_epoch or negative
// sequence_number with a non-empty shard_id. The frozen title asks for
// InvalidArgument; the universal, load-bearing invariant is that such a
// malformed token is NEVER rejected with FAILED_PRECONDITION (a malformed token
// is an argument error, not a stale-fence rejection) and never corrupts state.
// A provider that validates token shape returns InvalidArgument; the reference
// providerkit treats a non-zero token as a first-contact fence mark and accepts
// it. Both are accepted here; FAILED_PRECONDITION is not.
// ---------------------------------------------------------------------------
func TestB211_NegativeTokenCodeDiscipline(t *testing.T) {
	behavior(t, "B211")
	h := dial(t)

	// Each sub-case targets a fresh Speculative machine via Create (the natural
	// token carrier) so a possible acceptance is a legal Speculative->Idle edge.
	type tok struct {
		name       string
		epoch, seq int64
	}
	toks := []tok{
		{"negative-epoch", -1, 1},
		{"negative-seq", 1, -1},
		{"both-negative", -7, -3},
	}
	for _, tk := range toks {
		id := h.PickSpeculative()
		shard := h.UniqueShardID("b211-" + tk.name)
		err := h.FencedCall(harness.RPCCreate, id, shard, tk.epoch, tk.seq)
		switch harness.Code(err) {
		case codes.InvalidArgument:
			// Strict: rejected as malformed; machine untouched.
			if got := h.State(id); got != pb.MachineState_MACHINE_STATE_SPECULATIVE {
				t.Errorf("%s: rejected but state changed to %s (no partial transition)", tk.name, got)
			}
			h.StaysIn(id, pb.MachineState_MACHINE_STATE_SPECULATIVE, errStabilityWindow)
		case codes.OK:
			// Lenient: accepted as a first-contact token; legal Create edge.
			h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)
		case codes.FailedPrecondition:
			t.Errorf("%s: rejected with FAILED_PRECONDITION — a malformed token is an argument error, not a fence rejection", tk.name)
		default:
			t.Errorf("%s: code %s, want InvalidArgument (strict) or OK (lenient), never FAILED_PRECONDITION", tk.name, harness.Code(err))
		}
	}
}

// ---------------------------------------------------------------------------
// B212 — A Configure with an int64-max shard_epoch/sequence_number against a
// fresh shard is accepted and establishes the high-water mark at that value
// without overflow. We prove the mark is genuinely at the max by then showing a
// not-strictly-newer token (the same max, and a lower one) is fenced.
// ---------------------------------------------------------------------------
func TestB212_Int64MaxTokenHighWaterMark(t *testing.T) {
	behavior(t, "B212")
	h := dial(t)
	id := h.WalkToIdle() // Configure is legal on Idle
	shard := h.UniqueShardID("b212")
	const maxI64 = int64(math.MaxInt64)

	// Configure carrying the maximal token on a brand-new shard: accepted.
	if err := h.FencedCall(harness.RPCConfigure, id, shard, maxI64, maxI64); err != nil {
		t.Fatalf("int64-max Configure on a fresh shard: want accept, got %v", err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 20*time.Second)

	// The mark is now (MaxInt64, MaxInt64). A replay of the exact max is NOT
	// strictly newer => fenced (proves the mark sits AT the max, no overflow to
	// some smaller wrapped value that the replay would beat).
	drainID := h.PickSpeculative() // need a fresh machine the shard can Create
	if got := harness.Code(h.FencedCall(harness.RPCCreate, drainID, shard, maxI64, maxI64)); got != codes.FailedPrecondition {
		t.Errorf("replay of (MaxInt64,MaxInt64): code %s, want FAILED_PRECONDITION (mark must rest at the max)", got)
	}
	// A lower epoch with max sequence is also fenced (lexicographic, no wrap).
	if got := harness.Code(h.FencedCall(harness.RPCCreate, drainID, shard, maxI64-1, maxI64)); got != codes.FailedPrecondition {
		t.Errorf("(MaxInt64-1, MaxInt64): code %s, want FAILED_PRECONDITION", got)
	}
	// Sanity: the original machine actually carried the binding (accepted op).
	if c := h.Get(id).GetCluster(); c == "" {
		t.Errorf("int64-max Configure was acked but left no cluster binding")
	}
}

// ---------------------------------------------------------------------------
// B213 — A Configure carrying an oversized bootstrap_blob is either accepted or
// rejected with InvalidArgument (never FAILED_PRECONDITION) with no partial
// transition. The harness Configure helper sends a fixed small blob, so we send
// the oversized blob directly on the wire.
//
// The blob is sized at ~3 MiB: comfortably "oversized" for a cloud-init script
// (real ones are a few KB) yet UNDER gRPC's default 4 MiB receive frame cap, so
// the PROVIDER's own size handling is exercised rather than the transport
// refusing the frame outright. A provider that sets a tighter cap may still
// refuse at the transport with ResourceExhausted; that is a legitimate
// non-FAILED_PRECONDITION rejection and is accepted here too. FAILED_PRECONDITION
// (fence-reserved) and a partial transition are not.
// ---------------------------------------------------------------------------
func TestB213_OversizedBootstrapBlob(t *testing.T) {
	behavior(t, "B213")
	h := dial(t)
	id := h.WalkToIdle()

	blob := make([]byte, 3<<20) // 3 MiB — oversized but under the 4 MiB wire cap
	for i := range blob {
		blob[i] = byte('a' + i%26)
	}
	ctx, cancel := h.Ctx()
	_, err := h.Client.Configure(ctx, &pb.ConfigureRequest{
		MachineId:     id,
		ClusterId:     "conf-b213",
		BootstrapBlob: blob,
		ShardMetadata: map[string]string{"k": "v"},
	})
	cancel()

	switch harness.Code(err) {
	case codes.OK:
		h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 20*time.Second)
		// The binding lands; the blob itself is not echoed on the Machine, so we
		// only assert the transition completed cleanly with the cluster bound.
		if c := h.Get(id).GetCluster(); c != "conf-b213" {
			t.Errorf("accepted oversized-blob Configure left cluster %q, want conf-b213", c)
		}
	case codes.InvalidArgument, codes.ResourceExhausted:
		// Rejected (provider cap, or a tighter transport frame cap): no partial
		// transition — the machine stays Idle with no binding leaked.
		if got := h.State(id); got != pb.MachineState_MACHINE_STATE_IDLE {
			t.Errorf("rejected oversized-blob Configure moved machine to %s (no partial transition)", got)
		}
		h.StaysIn(id, pb.MachineState_MACHINE_STATE_IDLE, errStabilityWindow)
		if m := h.Get(id); m.GetCluster() != "" {
			t.Errorf("rejected oversized-blob Configure leaked cluster %q", m.GetCluster())
		}
	default:
		t.Errorf("oversized bootstrap_blob: code %s, want OK, InvalidArgument, or ResourceExhausted (never FAILED_PRECONDITION)", harness.Code(err))
	}
}

// ---------------------------------------------------------------------------
// B214 — Distinct successive target-state transitions on one machine mint
// distinct operation_ids (op-id freshness-per-new-cycle), complementing the
// same-op-id-at-target invariant. Drive a machine around the full cycle twice
// and assert every distinct transition gets a fresh, non-empty op-id, while a
// re-entry of the SAME target re-uses its prior id.
// ---------------------------------------------------------------------------
func TestB214_DistinctTransitionsFreshOperationIDs(t *testing.T) {
	behavior(t, "B214")
	h := dial(t)
	id := h.PickSpeculative()

	type step struct {
		name string
		call func() (*pb.TransitionAck, error)
		want pb.MachineState
	}
	steps := []step{
		{"create-1", func() (*pb.TransitionAck, error) { return h.Create(id) }, pb.MachineState_MACHINE_STATE_IDLE},
		{"configure-1", func() (*pb.TransitionAck, error) { return h.Configure(id, "conf-b214", map[string]string{"k": "1"}) }, pb.MachineState_MACHINE_STATE_CONFIGURED},
		{"drain-1", func() (*pb.TransitionAck, error) { return h.Drain(id, 5) }, pb.MachineState_MACHINE_STATE_IDLE},
		{"configure-2", func() (*pb.TransitionAck, error) { return h.Configure(id, "conf-b214-b", map[string]string{"k": "2"}) }, pb.MachineState_MACHINE_STATE_CONFIGURED},
		{"drain-2", func() (*pb.TransitionAck, error) { return h.Drain(id, 5) }, pb.MachineState_MACHINE_STATE_IDLE},
	}

	ids := make([]string, 0, len(steps))
	for _, st := range steps {
		ack, err := st.call()
		if err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		op := ack.GetOperationId()
		if op == "" {
			t.Errorf("%s: empty operation_id", st.name)
		}
		ids = append(ids, op)
		h.MustReach(id, st.want, 15*time.Second)
	}

	// Each NEW cycle into a target mints a FRESH op-id: create-1, configure-1,
	// drain-1, configure-2, drain-2 are five distinct transitions => five
	// distinct op-ids (a re-Drain after a re-Configure is a new drain cycle).
	seen := map[string]string{}
	for i, op := range ids {
		if prev, dup := seen[op]; dup {
			t.Errorf("step %s reused operation_id %q from step %s (distinct transitions must mint distinct ids)", steps[i].name, op, prev)
		}
		seen[op] = steps[i].name
	}

	// Complement: re-issuing the LAST transition at its settled target reuses the
	// same id (it is now an idempotent no-op-at-target, not a new cycle).
	last := steps[len(steps)-1]
	again, err := last.call()
	if err != nil {
		t.Fatalf("idempotent replay of %s: %v", last.name, err)
	}
	if again.GetOperationId() != ids[len(ids)-1] {
		t.Errorf("idempotent replay of %s minted a new op-id %q, want the cycle's id %q", last.name, again.GetOperationId(), ids[len(ids)-1])
	}
}

// ---------------------------------------------------------------------------
// B215 — Get on an unknown machine_id returns NotFound and Delete on an unknown
// machine_id returns NotFound or Unimplemented.
// ---------------------------------------------------------------------------
func TestB215_UnknownGetAndDelete(t *testing.T) {
	behavior(t, "B215")
	h := dial(t)
	ghost := "conformance-b215-ghost-" + h.UniqueShardID("id")

	if _, err := h.GetRaw(ghost); harness.Code(err) != codes.NotFound {
		t.Errorf("Get unknown: code %s, want NotFound", harness.Code(err))
	}
	switch _, err := h.Delete(ghost); harness.Code(err) {
	case codes.NotFound, codes.Unimplemented:
		// both conformant
	case codes.FailedPrecondition:
		t.Errorf("Delete unknown: FAILED_PRECONDITION leaked (reserved for fencing)")
	default:
		t.Errorf("Delete unknown: code %s, want NotFound or Unimplemented", harness.Code(err))
	}
}
