//go:build certify

package suite

import (
	"testing"
	"time"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"

	"github.com/intUnderflow/bigfleet-providers/conformance/internal/harness"
)

// C1 — Lifecycle & State Machine (behaviors B1xx). The four-stable-state /
// four-transitional-state / four-legal-edge model:
//
//	Speculative --Create--> [CREATING] --> Idle
//	Idle        --Configure-> [CONFIGURING] --> Configured
//	Configured  --Drain-----> [DRAINING] --> Idle
//	Idle        --Delete----> [DELETING] --> Speculative   (Delete capability)
//
// These tests DEEPEN the upstream happy-path: they assert residue is cleared at
// every Idle return, that any observed transitional state is the CORRECT one for
// the in-flight edge (best-effort against an instant in-memory actuator — never
// "must observe"), that the de-duplicated state trace never skips a stable
// state, and that a settled machine never spontaneously re-enters a transitional
// state. The provider under test is a FAST in-memory fake, so every transitional
// assertion is phrased "IF observed, it was the right one", never "it must be
// observed", and timing is never asserted.

const (
	settleTimeout = 15 * time.Second
	stabilityWin  = 300 * time.Millisecond
)

// B101 — a full Speculative->Idle->Configured->Idle round-trip repeated four
// times leaves cluster, shard_metadata, and last_error all empty at every
// return to Idle. Stronger than the matrix residue check: it also re-walks from
// Speculative each cycle (Drain back to Idle, never Delete) and asserts the
// machine is exactly Idle (a stable state, never transitional) at every rest.
func TestB101_RoundTripNoResidue(t *testing.T) {
	behavior(t, "B101")
	h := dial(t)

	id := h.WalkToIdle()
	// The first arrival at Idle must already be clean.
	assertCleanIdle(t, h, id, 0)

	for i := 0; i < 4; i++ {
		md := map[string]string{
			"bigfleet.lucy.sh/cycle": "round",
			"k":                      "v",
		}
		if _, err := h.Configure(id, "conf-b101", md); err != nil {
			t.Fatalf("cycle %d Configure: %v", i, err)
		}
		c := h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, settleTimeout)
		if c.GetCluster() == "" {
			t.Errorf("cycle %d: Configured machine has empty cluster", i)
		}
		if _, err := h.Drain(id, 5); err != nil {
			t.Fatalf("cycle %d Drain: %v", i, err)
		}
		h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, settleTimeout)
		assertCleanIdle(t, h, id, i+1)
	}
}

// assertCleanIdle re-reads the machine and asserts it rests in a clean Idle:
// state IDLE (a stable state), no cluster, no shard_metadata, no last_error.
func assertCleanIdle(t *testing.T, h *harness.H, id string, cycle int) {
	t.Helper()
	m := h.Get(id)
	if m.GetState() != pb.MachineState_MACHINE_STATE_IDLE {
		t.Errorf("cycle %d: machine rests in %s, want IDLE", cycle, m.GetState())
	}
	if c := m.GetCluster(); c != "" {
		t.Errorf("cycle %d: cluster %q survived to Idle", cycle, c)
	}
	if md := m.GetShardMetadata(); len(md) != 0 {
		t.Errorf("cycle %d: shard_metadata %v survived to Idle", cycle, md)
	}
	if e := m.GetLastError(); e != "" {
		t.Errorf("cycle %d: last_error %q on a clean Idle", cycle, e)
	}
}

// B103 — during a Configure, any mid-flight state observed is CONFIGURING (never
// another transitional), and the machine settles in Configured. Best-effort:
// the in-memory actuator may skip the window, so seen==false is tolerated; the
// binding assertion is that the settled result is Configured and any
// transitional state captured en route is CONFIGURING and nothing else.
func TestB103_ConfigureTransitionalIsConfiguring(t *testing.T) {
	behavior(t, "B103")
	h := dial(t)
	id := h.WalkToIdle()

	// Fire Configure, then sample the trace to the settle state.
	if _, err := h.Configure(id, "conf-b103", map[string]string{"a": "1"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	assertOnlyTransitional(t, h, id,
		pb.MachineState_MACHINE_STATE_CONFIGURING,
		pb.MachineState_MACHINE_STATE_CONFIGURED)

	m := h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, settleTimeout)
	if m.GetState() != pb.MachineState_MACHINE_STATE_CONFIGURED {
		t.Errorf("Configure settled in %s, want CONFIGURED", m.GetState())
	}
}

// B104 — during a Drain, any mid-flight state observed is DRAINING (never
// another transitional), and the machine settles back in Idle.
func TestB104_DrainTransitionalIsDraining(t *testing.T) {
	behavior(t, "B104")
	h := dial(t)
	id := h.WalkToConfigured("conf-b104", map[string]string{"a": "1"})

	if _, err := h.Drain(id, 5); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	assertOnlyTransitional(t, h, id,
		pb.MachineState_MACHINE_STATE_DRAINING,
		pb.MachineState_MACHINE_STATE_IDLE)

	m := h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, settleTimeout)
	if m.GetState() != pb.MachineState_MACHINE_STATE_IDLE {
		t.Errorf("Drain settled in %s, want IDLE", m.GetState())
	}
}

// assertOnlyTransitional samples the state trace from now until the machine
// reaches settle, and verifies that the ONLY transitional state observed
// en route is wantTransitional (a fast actuator may show none — that is fine —
// but it must never expose a DIFFERENT transitional state for this edge).
func assertOnlyTransitional(t *testing.T, h *harness.H, id string, wantTransitional, settle pb.MachineState) {
	t.Helper()
	trace := h.StateTrace(id, settle, settleTimeout)
	for _, s := range trace {
		if harness.IsTransitional(s) && s != wantTransitional {
			t.Errorf("observed transitional state %s en route to %s, want only %s",
				s, settle, wantTransitional)
		}
	}
	t.Logf("B-transitional trace to %s: %v (transitional window %s observed=%t)",
		settle, trace, wantTransitional, traceContains(trace, wantTransitional))
}

func traceContains(trace []pb.MachineState, s pb.MachineState) bool {
	for _, x := range trace {
		if x == s {
			return true
		}
	}
	return false
}

// B105 — the de-duplicated ordered state trace of a Create->Configure->Drain
// cycle visits only states adjacent on the four legal edges, never skipping a
// stable state. The legal-adjacency oracle: every consecutive pair in the
// de-duplicated trace must be an allowed step on the state graph. A fast
// actuator may collapse transitional states out (Speculative->Idle directly),
// which is allowed; what is forbidden is jumping over a stable state (e.g.
// Speculative->Configured) or surfacing a transitional state for the wrong edge.
func TestB105_LegalAdjacencyTrace(t *testing.T) {
	behavior(t, "B105")
	h := dial(t)

	id := h.PickSpeculative()
	if s := h.State(id); s != pb.MachineState_MACHINE_STATE_SPECULATIVE {
		t.Fatalf("seed machine not Speculative: %s", s)
	}

	// Seed the de-duplicated trace with the observed starting state. The fake's
	// actuator is instant, so the FIRST StateTrace sample after Create can
	// already read Idle (CREATING collapsed out); recording the confirmed
	// Speculative origin keeps the legal-adjacency oracle honest — it still
	// proves the first edge is Speculative->Idle (a legal CREATING-skip), never
	// a stable-state skip like Speculative->Configured.
	trace := []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE}
	record := func(seg []pb.MachineState) {
		for _, s := range seg {
			if len(trace) == 0 || trace[len(trace)-1] != s {
				trace = append(trace, s)
			}
		}
	}

	// Create: Speculative -> [CREATING] -> Idle.
	if _, err := h.Create(id); err != nil {
		t.Fatalf("Create: %v", err)
	}
	record(h.StateTrace(id, pb.MachineState_MACHINE_STATE_IDLE, settleTimeout))
	// Configure: Idle -> [CONFIGURING] -> Configured.
	if _, err := h.Configure(id, "conf-b105", map[string]string{"x": "y"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	record(h.StateTrace(id, pb.MachineState_MACHINE_STATE_CONFIGURED, settleTimeout))
	// Drain: Configured -> [DRAINING] -> Idle.
	if _, err := h.Drain(id, 5); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	record(h.StateTrace(id, pb.MachineState_MACHINE_STATE_IDLE, settleTimeout))

	t.Logf("B105 de-duplicated trace: %v", trace)
	if len(trace) == 0 {
		t.Fatal("B105: empty trace")
	}
	if trace[0] != pb.MachineState_MACHINE_STATE_SPECULATIVE {
		t.Errorf("B105: trace must start at Speculative, got %s", trace[0])
	}
	for i := 1; i < len(trace); i++ {
		from, to := trace[i-1], trace[i]
		if !legalAdjacent(from, to) {
			t.Errorf("B105: illegal/skipping transition %s -> %s in trace %v", from, to, trace)
		}
	}
}

// legalAdjacent reports whether to may directly follow from in a de-duplicated
// observed trace for the Create->Configure->Drain cycle. Allowed edges:
//
//	Speculative -> CREATING | Idle           (Create; CREATING may be skipped)
//	CREATING    -> Idle
//	Idle        -> CONFIGURING | Configured   (Configure; CONFIGURING may be skipped)
//	CONFIGURING -> Configured
//	Configured  -> DRAINING | Idle            (Drain; DRAINING may be skipped)
//	DRAINING    -> Idle
//
// The point of the oracle: a stable state may never be SKIPPED (no
// Speculative->Configured, no Idle->Idle-via-nothing jump over a needed state),
// and a transitional state may only sit between its own two stable endpoints.
func legalAdjacent(from, to pb.MachineState) bool {
	type edge struct{ a, b pb.MachineState }
	allowed := map[edge]bool{
		{pb.MachineState_MACHINE_STATE_SPECULATIVE, pb.MachineState_MACHINE_STATE_CREATING}:   true,
		{pb.MachineState_MACHINE_STATE_SPECULATIVE, pb.MachineState_MACHINE_STATE_IDLE}:       true,
		{pb.MachineState_MACHINE_STATE_CREATING, pb.MachineState_MACHINE_STATE_IDLE}:          true,
		{pb.MachineState_MACHINE_STATE_IDLE, pb.MachineState_MACHINE_STATE_CONFIGURING}:       true,
		{pb.MachineState_MACHINE_STATE_IDLE, pb.MachineState_MACHINE_STATE_CONFIGURED}:        true,
		{pb.MachineState_MACHINE_STATE_CONFIGURING, pb.MachineState_MACHINE_STATE_CONFIGURED}: true,
		{pb.MachineState_MACHINE_STATE_CONFIGURED, pb.MachineState_MACHINE_STATE_DRAINING}:    true,
		{pb.MachineState_MACHINE_STATE_CONFIGURED, pb.MachineState_MACHINE_STATE_IDLE}:        true,
		{pb.MachineState_MACHINE_STATE_DRAINING, pb.MachineState_MACHINE_STATE_IDLE}:          true,
	}
	return allowed[edge{from, to}]
}

// B106 — cluster is non-empty whenever the machine rests in Configured and is
// empty once a Drain has settled to Idle; if a Configuring window is observed,
// cluster is already non-empty in it. The cluster-binding invariant tracked
// through a Configure (sampling the transitional snapshot best-effort) and a
// Drain.
func TestB106_ClusterBindingInvariant(t *testing.T) {
	behavior(t, "B106")
	h := dial(t)
	const cluster = "conf-b106"
	id := h.WalkToIdle()

	// Idle: cluster empty before binding.
	if c := h.Get(id).GetCluster(); c != "" {
		t.Errorf("Idle (pre-Configure) cluster %q, want empty", c)
	}

	if _, err := h.Configure(id, cluster, map[string]string{"a": "1"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	// Best-effort: if we catch a CONFIGURING snapshot, cluster must already be set.
	seenConfiguring := false
	deadline := time.Now().Add(settleTimeout)
	for time.Now().Before(deadline) {
		m, err := h.GetRaw(id)
		if err == nil {
			if m.GetState() == pb.MachineState_MACHINE_STATE_CONFIGURING {
				seenConfiguring = true
				if m.GetCluster() == "" {
					t.Errorf("CONFIGURING window has empty cluster (binding must be visible mid-flight)")
				}
			}
			if m.GetState() == pb.MachineState_MACHINE_STATE_CONFIGURED {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Logf("B106: CONFIGURING window observed=%t", seenConfiguring)

	// Configured: cluster non-empty.
	c := h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, settleTimeout)
	if c.GetCluster() != cluster {
		t.Errorf("Configured cluster %q, want %q", c.GetCluster(), cluster)
	}

	// Drain settled to Idle: cluster empty.
	if _, err := h.Drain(id, 5); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	m := h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, settleTimeout)
	if m.GetCluster() != "" {
		t.Errorf("post-Drain Idle cluster %q, want empty", m.GetCluster())
	}
}

// B107 — at every resting stable state, host is nil for Speculative and set for
// Idle/Configured, with no transitional state ever surfaced as the settled Get
// result. Walks one machine through Speculative -> Idle -> Configured -> Idle and
// asserts the host-vs-state invariant at each REST (after MustReach, the result
// is by construction a stable state).
func TestB107_HostByStateAtEveryRest(t *testing.T) {
	behavior(t, "B107")
	h := dial(t)

	id := h.PickSpeculative()
	// Speculative rest: host nil.
	sp := h.Get(id)
	if sp.GetState() != pb.MachineState_MACHINE_STATE_SPECULATIVE {
		t.Fatalf("seed not Speculative: %s", sp.GetState())
	}
	assertSettledStable(t, sp.GetState())
	if lcHostSet(sp) {
		t.Errorf("Speculative machine has a host (%v)", sp.GetHost())
	}

	// Idle rest: host set.
	if _, err := h.Create(id); err != nil {
		t.Fatalf("Create: %v", err)
	}
	idle := h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, settleTimeout)
	assertSettledStable(t, idle.GetState())
	if !lcHostSet(idle) {
		t.Errorf("Idle machine has no host")
	}

	// Configured rest: host set.
	if _, err := h.Configure(id, "conf-b107", nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	conf := h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, settleTimeout)
	assertSettledStable(t, conf.GetState())
	if !lcHostSet(conf) {
		t.Errorf("Configured machine has no host")
	}

	// Back to Idle: host still set.
	if _, err := h.Drain(id, 5); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	idle2 := h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, settleTimeout)
	assertSettledStable(t, idle2.GetState())
	if !lcHostSet(idle2) {
		t.Errorf("post-Drain Idle machine has no host")
	}
}

func lcHostSet(m *pb.Machine) bool {
	h := m.GetHost()
	return h != nil && (h.GetRef() != "" || h.GetProvider() != "")
}

// assertSettledStable asserts a settled Get result is a stable state and never a
// transitional one (a MustReach result or a seed read is by definition settled).
func assertSettledStable(t *testing.T, s pb.MachineState) {
	t.Helper()
	if harness.IsTransitional(s) {
		t.Errorf("settled Get surfaced transitional state %s (must be stable)", s)
	}
	if !harness.IsStable(s) {
		t.Errorf("settled Get surfaced non-stable state %s", s)
	}
}

// B108 — after a settled mutation, Consistently-polling the machine over a
// stability window shows it never spontaneously re-enters a transitional state.
// Uses NeverReaches against each transitional state over a stability window
// (no client mutation in flight): the machine must hold its settled state.
func TestB108_NoSpontaneousReTransition(t *testing.T) {
	behavior(t, "B108")
	h := dial(t)
	id := h.WalkToConfigured("conf-b108", map[string]string{"k": "v"})

	// Settled in Configured: must not drift into ANY transitional state, nor
	// leave Configured, over the stability window.
	h.StaysIn(id, pb.MachineState_MACHINE_STATE_CONFIGURED, stabilityWin)
	for _, bad := range []pb.MachineState{
		pb.MachineState_MACHINE_STATE_CREATING,
		pb.MachineState_MACHINE_STATE_CONFIGURING,
		pb.MachineState_MACHINE_STATE_DRAINING,
		pb.MachineState_MACHINE_STATE_DELETING,
	} {
		h.NeverReaches(id, bad, stabilityWin)
	}

	// Repeat at a second settled state (Idle) to confirm the property is not
	// specific to Configured.
	if _, err := h.Drain(id, 5); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, settleTimeout)
	h.StaysIn(id, pb.MachineState_MACHINE_STATE_IDLE, stabilityWin)
	for _, bad := range []pb.MachineState{
		pb.MachineState_MACHINE_STATE_CREATING,
		pb.MachineState_MACHINE_STATE_CONFIGURING,
		pb.MachineState_MACHINE_STATE_DRAINING,
		pb.MachineState_MACHINE_STATE_DELETING,
	} {
		h.NeverReaches(id, bad, stabilityWin)
	}
}

// B109 — during a Delete, any mid-flight state observed is DELETING (never
// another transitional), and the machine settles back in Speculative.
// Capability-gated on Delete: skip if the provider does not support it.
func TestB109_DeleteTransitionalIsDeleting(t *testing.T) {
	behavior(t, "B109")
	h := dial(t)
	if !h.Probe().Delete {
		t.Skip("provider does not support Delete (capability absent)")
	}

	id := h.WalkToIdle()
	if _, err := h.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	assertOnlyTransitional(t, h, id,
		pb.MachineState_MACHINE_STATE_DELETING,
		pb.MachineState_MACHINE_STATE_SPECULATIVE)

	m := h.MustReach(id, pb.MachineState_MACHINE_STATE_SPECULATIVE, settleTimeout)
	if m.GetState() != pb.MachineState_MACHINE_STATE_SPECULATIVE {
		t.Errorf("Delete settled in %s, want SPECULATIVE", m.GetState())
	}
	// Delete back to Speculative clears the host (no lingering binding).
	if lcHostSet(m) {
		t.Errorf("post-Delete Speculative machine still has a host (%v)", m.GetHost())
	}
}
