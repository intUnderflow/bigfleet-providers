//go:build certify

package suite

import (
	"bytes"
	"testing"
	"time"

	"github.com/intUnderflow/bigfleet-providers/conformance/internal/harness"
	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

// Area C9 — List, Revision & Pagination (behaviors B901-B909).
//
// List is an UNORDERED set: every assertion here is about membership, count, and
// revision-cursor discipline, never about position. operation_id is never read
// (this area does not assert on it). since_revision is capability-gated: tests
// that need a real delta cursor Probe() first and t.Skip when the provider does
// not advance List.revision. The provider under test is a fast in-memory fake,
// so transitional windows are not asserted here; we drive mutations to their
// settled state via the harness walkers and MustReach.

// allStates is the closed set of machine states a List filter can name.
var allStates = []pb.MachineState{
	pb.MachineState_MACHINE_STATE_SPECULATIVE,
	pb.MachineState_MACHINE_STATE_IDLE,
	pb.MachineState_MACHINE_STATE_CONFIGURED,
	pb.MachineState_MACHINE_STATE_CREATING,
	pb.MachineState_MACHINE_STATE_CONFIGURING,
	pb.MachineState_MACHINE_STATE_DRAINING,
	pb.MachineState_MACHINE_STATE_DELETING,
	pb.MachineState_MACHINE_STATE_FAILED,
}

// idSet collects the ids of a machine slice into a set, failing on any duplicate
// (List is a set: a conformant provider must never repeat an id within one List).
func idSet(t *testing.T, where string, ms []*pb.Machine) map[string]struct{} {
	t.Helper()
	out := make(map[string]struct{}, len(ms))
	for _, m := range ms {
		id := m.GetId()
		if _, dup := out[id]; dup {
			t.Errorf("%s: List returned duplicate id %s within a single response (List is a set)", where, id)
		}
		out[id] = struct{}{}
	}
	return out
}

// B901 — single-state filters return only that state; a multi-state filter
// returns EXACTLY the union of the singles and nothing else (set equality both
// ways), swept across every state including the transitional ones.
func TestB901_FilterByStateAndUnion(t *testing.T) {
	behavior(t, "B901")
	h := dial(t)

	// 1. Every single-state filter returns only machines in that state.
	perState := make(map[pb.MachineState]map[string]struct{}, len(allStates))
	for _, st := range allStates {
		ms := h.List(st)
		set := idSet(t, "single", ms)
		for _, m := range ms {
			if m.GetState() != st {
				t.Errorf("List(%s) returned %s in state %s", st, m.GetId(), m.GetState())
			}
		}
		perState[st] = set
	}

	// 2. A multi-state filter over a meaningful pair returns the EXACT union of
	//    the corresponding single-state filters: nothing extra, nothing missing.
	//    Use SPECULATIVE+IDLE+CONFIGURED — the three stable states the fake will
	//    actually populate, so the union is non-trivial.
	pair := []pb.MachineState{
		pb.MachineState_MACHINE_STATE_SPECULATIVE,
		pb.MachineState_MACHINE_STATE_IDLE,
		pb.MachineState_MACHINE_STATE_CONFIGURED,
	}
	// Drive one machine to each of IDLE and CONFIGURED so the union is non-empty
	// in more than one bucket (deepen past a Speculative-only inventory).
	_ = h.WalkToIdle()
	_ = h.WalkToConfigured("conf-b901", map[string]string{"area": "list"})

	union := h.List(pair...)
	unionSet := idSet(t, "union", union)

	// Every machine the union returns is in one of the named states...
	wantStates := map[pb.MachineState]bool{}
	for _, st := range pair {
		wantStates[st] = true
	}
	for _, m := range union {
		if !wantStates[m.GetState()] {
			t.Errorf("union filter returned %s in unrequested state %s", m.GetId(), m.GetState())
		}
	}

	// ...and the union is exactly the sum of the singles. Recompute the singles
	// fresh (the two walks above changed the inventory) to compare like with like
	// in the same observation window.
	expected := map[string]pb.MachineState{}
	for _, st := range pair {
		for _, m := range h.List(st) {
			expected[m.GetId()] = st
		}
	}
	// Union ⊆ sum-of-singles.
	for id := range unionSet {
		if _, ok := expected[id]; !ok {
			t.Errorf("union returned %s which no single-state filter in %v returned", id, pair)
		}
	}
	// sum-of-singles ⊆ union (modulo machines that legitimately changed state
	// between the two observations — those we re-Get and only flag a true miss
	// if the machine is still resting in a requested state).
	for id, st := range expected {
		if _, ok := unionSet[id]; ok {
			continue
		}
		if cur := h.State(id); wantStates[cur] {
			t.Errorf("union omitted %s which single filter saw in %s and is still in a requested state (%s)", id, st, cur)
		}
	}
}

// B902 — max_results is a hard upper bound at 1,2,3 and max_results=0 imposes no
// cap. Asserts the cap is a CEILING (never exceeded) and that the uncapped list
// is at least as large as any capped slice and as large as the seeded fleet.
func TestB902_MaxResultsCap(t *testing.T) {
	behavior(t, "B902")
	h := dial(t)

	full := h.List() // max_results unset == 0 == no cap
	fullSet := idSet(t, "full", full)
	if len(full) == 0 {
		t.Fatal("uncapped List returned an empty fleet; expected a seeded inventory")
	}

	for _, n := range []int32{1, 2, 3} {
		ms := h.ListMax(n)
		if int32(len(ms)) > n {
			t.Errorf("List(max_results=%d) returned %d machines, exceeding the cap", n, len(ms))
		}
		// Whatever the cap returns must be a real subset of the full inventory
		// (same set semantics, no duplicates, no phantom ids).
		capSet := idSet(t, "capped", ms)
		for id := range capSet {
			if _, ok := fullSet[id]; !ok {
				t.Errorf("List(max_results=%d) returned %s not present in the uncapped List", n, id)
			}
		}
		// And the cap must actually bite when the fleet is larger than the cap.
		if int32(len(full)) > n && int32(len(ms)) < 1 {
			t.Errorf("List(max_results=%d) returned 0 machines despite a fleet of %d", n, len(full))
		}
	}

	// max_results=0 explicitly imposes no cap: it returns the same full set as
	// the unset-field call (the fake has < hundreds of machines, well under any
	// implicit server page size we'd need to worry about).
	zero := h.ListMax(0)
	if len(zero) != len(full) {
		t.Errorf("List(max_results=0) returned %d, want the uncapped %d (zero must mean no cap)", len(zero), len(full))
	}
}

// B903 — List.revision advances after a mutation and a delta List since the
// prior cursor includes the just-mutated machine. Capability-gated.
func TestB903_RevisionAdvancesAndDelta(t *testing.T) {
	behavior(t, "B903")
	h := dial(t)
	if !h.Probe().SinceRevision {
		t.Skip("provider does not advance List.revision (since_revision capability absent)")
	}

	r0 := h.Revision()
	if len(r0) == 0 {
		t.Fatal("baseline revision is empty but provider advertised SinceRevision")
	}

	id := h.WalkToIdle() // a real mutation: Speculative -> Idle

	r1 := h.Revision()
	if bytes.Equal(r0, r1) {
		t.Fatalf("revision did not advance after a Create mutation (still %x)", r0)
	}

	// The delta since r0 must include the just-mutated machine, and must not
	// duplicate any id.
	delta, _ := h.ListSince(r0)
	deltaSet := idSet(t, "delta", delta)
	if _, ok := deltaSet[id]; !ok {
		t.Errorf("delta List(since=r0) did not include the mutated machine %s", id)
	}
}

// B904 — a delta since the CURRENT revision, with no intervening mutation, is
// empty. Capability-gated.
func TestB904_EmptyDeltaWhenQuiescent(t *testing.T) {
	behavior(t, "B904")
	h := dial(t)
	if !h.Probe().SinceRevision {
		t.Skip("provider does not advance List.revision (since_revision capability absent)")
	}

	// Take a cursor that already reflects all current state.
	rNow := h.Revision()

	// Immediately read the delta with no mutation in between: must be empty.
	delta, _ := h.ListSince(rNow)
	if len(delta) != 0 {
		t.Errorf("delta since current revision returned %d machines with no intervening mutation: %v",
			len(delta), IDsHelper(delta))
	}
}

// B905 — across many sequential mutations the revision cursor is monotonic in
// the sense that each post-mutation revision differs from the immediately prior
// one, AND a since-delta keyed on ANY earlier cursor includes EVERY machine
// mutated after that cursor was taken. Capability-gated.
func TestB905_MonotonicRevisionCursor(t *testing.T) {
	behavior(t, "B905")
	h := dial(t)
	if !h.Probe().SinceRevision {
		t.Skip("provider does not advance List.revision (since_revision capability absent)")
	}

	const rounds = 4
	cursors := make([][]byte, 0, rounds+1)
	mutated := make([]string, 0, rounds)

	cursors = append(cursors, h.Revision()) // cursor[0], before any mutation

	for i := 0; i < rounds; i++ {
		id := h.WalkToIdle()
		mutated = append(mutated, id)
		rev := h.Revision()
		// Each new revision differs from the one captured before this mutation.
		if bytes.Equal(rev, cursors[len(cursors)-1]) {
			t.Errorf("round %d: revision did not change after mutating %s", i, id)
		}
		cursors = append(cursors, rev)
	}

	// A delta keyed on each earlier cursor must include all machines mutated
	// strictly after that cursor was captured. cursors[k] was taken after the
	// first k mutations, so it must surface mutated[k:].
	for k := 0; k < len(cursors)-1; k++ {
		delta, _ := h.ListSince(cursors[k])
		got := map[string]struct{}{}
		for _, m := range delta {
			got[m.GetId()] = struct{}{}
		}
		for _, want := range mutated[k:] {
			if _, ok := got[want]; !ok {
				t.Errorf("delta since cursor[%d] missing machine %s mutated after it", k, want)
			}
		}
	}
}

// B906 — a garbage, zero-length, or non-cursor since_revision is treated as
// no-cursor: it returns the FULL list without error (never a server error, never
// a spuriously-empty delta). Not capability-gated: providers that ignore the
// field entirely must still satisfy this.
func TestB906_GarbageCursorIsFullList(t *testing.T) {
	behavior(t, "B906")
	h := dial(t)

	full := h.List()
	fullSet := idSet(t, "full", full)
	if len(full) == 0 {
		t.Fatal("uncapped List returned an empty fleet; expected a seeded inventory")
	}

	garbage := [][]byte{
		nil,                              // unset (== no cursor)
		{},                               // explicit zero-length
		[]byte("not-a-real-cursor"),      // arbitrary ASCII
		{0x00, 0xff, 0x10, 0x42, 0x7f},   // arbitrary binary
		bytes.Repeat([]byte{0xde}, 4096), // oversized junk
	}
	for _, g := range garbage {
		got, _ := h.ListSince(g) // ListSince t.Fatalf's on a transport error
		// A garbage cursor must NOT be interpreted as "everything changed since
		// some valid point and nothing matched": it degrades to the full list,
		// so the full inventory must be present.
		gotSet := idSet(t, "garbage-delta", got)
		for id := range fullSet {
			if _, ok := gotSet[id]; !ok {
				t.Errorf("garbage since_revision %q dropped machine %s (must degrade to full list)", g, id)
				break
			}
		}
	}
}

// B907 — paging the changed-set via max_results + since_revision walks the
// List-as-a-set with no duplicate and no skipped machine (set-completeness).
// Capability-gated.
//
// since_revision on this contract is a "changed-since" delta keyed on a
// monotone revision, and List echoes the CURRENT head revision on every call
// (not a per-page continuation token). So there are two conformant cursor
// styles and this test tolerates both:
//
//   - advancing cursor: each page returns a fresh continuation; we thread it
//     forward, accumulate, and require no id twice and no id skipped.
//   - stable head cursor: every call against a fixed `since` returns the same
//     changed-set capped by max_results; we cannot offset-page it, so we assert
//     completeness against ONE uncapped delta and require each capped page to be
//     a duplicate-free subset of it.
//
// Either way the invariant is the same: the union of what paging exposes is
// exactly the changed-set, with no duplicate and no skip.
func TestB907_PagingSetCompleteness(t *testing.T) {
	behavior(t, "B907")
	h := dial(t)
	if !h.Probe().SinceRevision {
		t.Skip("provider does not advance List.revision (since_revision capability absent)")
	}

	const k = 5
	base := h.Revision()
	want := map[string]struct{}{}
	for i := 0; i < k; i++ {
		want[h.WalkToIdle()] = struct{}{}
	}

	// Ground truth: the full (uncapped) changed-set since `base`. Every machine
	// we mutated must be in it, with no duplicate id.
	fullDelta, _ := h.ListSince(base)
	fullDeltaSet := idSet(t, "full-delta", fullDelta)
	for id := range want {
		if _, ok := fullDeltaSet[id]; !ok {
			t.Errorf("uncapped delta since base skipped mutated machine %s (set-incompleteness)", id)
		}
	}

	// Probe the cursor style with two capped reads. Page 0 reads up to 2 of the
	// changed-set since `base`; we then thread the cursor it returns and read
	// page 1. A true continuation cursor yields a DISJOINT next chunk of the same
	// changed-set; a head-echo cursor (this contract echoes the current head on
	// every call) yields an empty page 1 because nothing changed past the head.
	page0, next0 := listSinceMax(t, h, base, 2)
	if int32(len(page0)) > 2 {
		t.Errorf("capped read returned %d machines, exceeding max_results=2", len(page0))
	}
	seen := idSet(t, "page0", page0)
	for id := range seen {
		if _, ok := fullDeltaSet[id]; !ok {
			t.Errorf("capped page0 returned %s not in the uncapped changed-set", id)
		}
	}

	page1, _ := listSinceMax(t, h, next0, 2)
	page1New := 0
	for _, m := range page1 {
		if _, dup := seen[m.GetId()]; dup {
			t.Errorf("threaded page1 returned duplicate machine %s already on page0", m.GetId())
		}
		if _, ok := fullDeltaSet[m.GetId()]; ok {
			page1New++ // a genuinely new member of our changed-set
		}
		seen[m.GetId()] = struct{}{}
	}

	if page1New > 0 {
		// Continuation-cursor style: keep threading until the changed-set drains,
		// then require the accumulated union to cover every mutated machine with
		// no duplicate.
		cursor := next0
		const maxPages = 64
		for page := 1; page < maxPages; page++ {
			ms, next := listSinceMax(t, h, cursor, 2)
			if len(ms) == 0 || bytes.Equal(next, cursor) {
				break
			}
			for _, m := range ms {
				seen[m.GetId()] = struct{}{}
			}
			cursor = next
		}
		for id := range want {
			if _, ok := seen[id]; !ok {
				t.Errorf("continuation paged walk skipped mutated machine %s (set-incompleteness)", id)
			}
		}
	}
	// Head-echo style (page1New==0): the cursor cannot offset-page a fixed delta,
	// so set-completeness was already proven against the uncapped ground truth
	// above; the capped reads only had to be no-dup subsets, which they were.
}

// B908 — with no client mutation, a since-poller observes no revision bump from
// background idle reconcile ticks: repeated List over a quiescent window returns
// an empty delta every time. Capability-gated.
func TestB908_NoBumpFromIdleReconcile(t *testing.T) {
	behavior(t, "B908")
	h := dial(t)
	if !h.Probe().SinceRevision {
		t.Skip("provider does not advance List.revision (since_revision capability absent)")
	}

	// Anchor on the current revision, then poll the delta repeatedly across a
	// quiescent window WITHOUT issuing any mutation. A conformant provider does
	// not bump the cursor on its own reconcile/heartbeat ticks, so every delta
	// is empty. (Idle background ticks must be client-invisible.)
	anchor := h.Revision()
	deadline := time.Now().Add(750 * time.Millisecond)
	polls := 0
	for time.Now().Before(deadline) {
		delta, _ := h.ListSince(anchor)
		if len(delta) != 0 {
			t.Errorf("quiescent poll #%d saw a non-empty delta (%d machines) with no client mutation: %v",
				polls, len(delta), IDsHelper(delta))
		}
		polls++
		time.Sleep(50 * time.Millisecond)
	}
	if polls == 0 {
		t.Fatal("quiescent window elapsed without a single poll")
	}
}

// B909 — an opaque revision cursor fed back as the EXACT bytes a prior List
// emitted is accepted; a byte-mutated copy degrades to a full list rather than
// erroring (opaque-cursor robustness). Capability-gated.
func TestB909_OpaqueCursorRoundTripAndCorruption(t *testing.T) {
	behavior(t, "B909")
	h := dial(t)
	if !h.Probe().SinceRevision {
		t.Skip("provider does not advance List.revision (since_revision capability absent)")
	}

	full := h.List()
	fullSet := idSet(t, "full", full)

	// Exact bytes a prior List emitted: accepted, and (no mutation since) yields
	// an empty delta.
	cursor := h.Revision()
	if len(cursor) == 0 {
		t.Fatal("revision cursor is empty but provider advertised SinceRevision")
	}
	delta, _ := h.ListSince(cursor)
	if len(delta) != 0 {
		t.Errorf("verbatim cursor with no intervening mutation returned %d machines, want empty delta", len(delta))
	}

	// A byte-mutated copy must NEVER error (the load-bearing robustness
	// invariant): the provider either recognizes it as a valid-but-different
	// cursor (a narrow delta) or fails to parse it and degrades to the full
	// list. Either is conformant; an error is not. ListSince fatals on a
	// transport error, so simply reaching past each call proves no-error.
	//
	// We additionally assert the STRONG form of degradation for cursors that are
	// guaranteed NOT to be a cursor the provider could have issued — non-numeric
	// suffixes (this contract's cursor is an opaque decimal revision; a non-
	// digit byte makes it unparseable, so it must degrade to the full list).
	nonCursors := [][]byte{
		append(bytes.Clone(cursor), '!'),        // numeric prefix, non-digit suffix
		[]byte("~" + string(cursor)),            // non-digit prefix
		append(bytes.Clone(cursor), 0x00, 0xff), // trailing binary garbage
	}
	for _, bad := range nonCursors {
		got, _ := h.ListSince(bad) // no error tolerated past this point
		gotSet := idSet(t, "non-cursor-delta", got)
		for id := range fullSet {
			if _, ok := gotSet[id]; !ok {
				if cur := h.State(id); IsStableHelper(cur) {
					t.Errorf("non-parseable cursor %q dropped machine %s (must degrade to full list)", bad, id)
				}
			}
		}
	}

	// A byte-mutated copy that may still parse as some other valid revision: it
	// must not error and must never return a machine outside the full inventory
	// (no phantom ids, no duplicates) — a delta is a subset of the fleet.
	flipped := bytes.Clone(cursor)
	flipped[len(flipped)-1] ^= 0x01 // smallest perturbation; often still a digit
	maybeDelta, _ := h.ListSince(flipped)
	for id := range idSet(t, "flipped-delta", maybeDelta) {
		if _, ok := fullSet[id]; !ok {
			if cur := h.State(id); IsStableHelper(cur) {
				t.Errorf("perturbed cursor surfaced %s outside the current fleet", id)
			}
		}
	}
}

// listSinceMax issues one List with BOTH a since_revision cursor and a
// max_results cap — the combination the harness exposes only separately — and
// returns the page plus the new cursor. It fails the test on a transport error.
func listSinceMax(t *testing.T, h *harness.H, rev []byte, max int32) ([]*pb.Machine, []byte) {
	t.Helper()
	ctx, cancel := h.Ctx()
	defer cancel()
	resp, err := h.Client.List(ctx, &pb.ListFilter{SinceRevision: rev, MaxResults: max})
	if err != nil {
		t.Fatalf("List(since,max=%d): %v", max, err)
	}
	return resp.GetMachines(), resp.GetRevision()
}

// --- small local helpers (kept here, not in the frozen harness) ------------

// IDsHelper renders ids for error messages without importing the harness IDsOf
// into every callsite.
func IDsHelper(ms []*pb.Machine) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.GetId())
	}
	return out
}

// IsStableHelper mirrors the harness stable-state predicate for the corruption
// check (only flag a missing machine if it is currently resting in a stable
// state, i.e. legitimately List-visible).
func IsStableHelper(s pb.MachineState) bool {
	switch s {
	case pb.MachineState_MACHINE_STATE_SPECULATIVE,
		pb.MachineState_MACHINE_STATE_IDLE,
		pb.MachineState_MACHINE_STATE_CONFIGURED,
		pb.MachineState_MACHINE_STATE_FAILED:
		return true
	}
	return false
}
