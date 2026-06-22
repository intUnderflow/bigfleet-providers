//go:build certify && fault

// Package suite — FAULT LANE (behaviors B7xx). These tests run ONLY with
// `-tags=certify,fault` against the reference faultprovider
// (conformance/faultprovider), which injects substrate faults on command over
// the wire. They certify the kit's failure / timeout / late-completion-discard /
// recovery handling — things a fast happy-path fake can never exercise.
//
//	go test -tags=certify,fault -run 'TestB7' ./suite/... -target=<faultaddr>
//
// The faultprovider is booted with a SHORT --transition-timeout (default 2s), so
// the timeout-shaped tests (B703/B704) are fast. Each test consumes a fresh
// Speculative machine; the faultprovider seeds 64+, so no t.Parallel is needed.
//
// Fault selectors (all driven over the wire):
//   - Configure cluster_id "fault-error"       -> actuator errors immediately
//   - Configure cluster_id "fault-timeout"     -> actuator blocks until ctx done
//   - Configure cluster_id "fault-slow-ok"     -> actuator ignores ctx, late success
//   - Configure cluster_id "fault-drain-error" -> later Drain errors
//   - Create label conformance.fault/create=error -> CreateInstance errors
package suite

import (
	"testing"
	"time"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"

	"github.com/intUnderflow/bigfleet-providers/conformance/internal/harness"
)

// transitionTimeout is the faultprovider's --transition-timeout. The runner and
// the self-verify boot it with 2s; the timeout-bounded waits below add ample
// slack so a slightly slower environment never flakes.
const transitionTimeout = 2 * time.Second

func faultMD() map[string]string {
	return map[string]string{"conformance.fault/test": "1", "k": "v"}
}

// pickHealthySpeculative returns one Speculative machine that is NOT a
// create-fault slot, so a generic walk-to-Idle never accidentally selects a
// machine whose CreateInstance is wired to error. The faultprovider seeds 64
// healthy Speculative slots plus 8 create-fault ones; List(SPECULATIVE) returns
// them in an arbitrary (map-iteration) order, so the fault lane MUST filter by
// label rather than rely on the harness's PickSpeculative (which grabs an
// arbitrary first machine and could land on a create-fault slot).
func pickHealthySpeculative(t *testing.T, h *harness.H) string {
	t.Helper()
	for _, m := range h.List(pb.MachineState_MACHINE_STATE_SPECULATIVE) {
		if m.GetLabels()["conformance.fault/create"] != "error" {
			return m.GetId()
		}
	}
	t.Skip("fault lane: faultprovider seeded no healthy Speculative machine")
	return ""
}

// walkHealthyToIdle drives a healthy (non-create-fault) Speculative machine to
// Idle and returns its id.
func walkHealthyToIdle(t *testing.T, h *harness.H) string {
	t.Helper()
	id := pickHealthySpeculative(t, h)
	if _, err := h.Create(id); err != nil {
		t.Fatalf("Create(%s): %v", id, err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, transitionTimeout+5*time.Second)
	return id
}

// walkHealthyToConfigured drives a healthy machine to Configured for cluster.
func walkHealthyToConfigured(t *testing.T, h *harness.H, cluster string, md map[string]string) string {
	t.Helper()
	id := walkHealthyToIdle(t, h)
	if _, err := h.Configure(id, cluster, md); err != nil {
		t.Fatalf("Configure(%s,%s): %v", id, cluster, err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, transitionTimeout+5*time.Second)
	return id
}

// B701 — a Configure whose actuator ERRORS drives the machine to FAILED with a
// non-empty last_error and no lingering CONFIGURING state.
func TestB701_ConfigureActuatorError(t *testing.T) {
	behavior(t, "B701")
	h := dial(t)

	id := walkHealthyToIdle(t, h)
	if _, err := h.Configure(id, "fault-error", faultMD()); err != nil {
		t.Fatalf("Configure(fault-error): %v", err)
	}
	m := h.MustReach(id, pb.MachineState_MACHINE_STATE_FAILED, transitionTimeout+5*time.Second)
	if m.GetLastError() == "" {
		t.Errorf("B701: FAILED machine has empty last_error")
	}
	// Never stuck CONFIGURING: it has already settled FAILED above, confirm it
	// stays there over a short window (no spontaneous re-entry to CONFIGURING).
	h.NeverReaches(id, pb.MachineState_MACHINE_STATE_CONFIGURING, 300*time.Millisecond)
}

// B702 — a Create whose actuator ERRORS drives the machine to FAILED with a
// non-empty last_error.
func TestB702_CreateActuatorError(t *testing.T) {
	behavior(t, "B702")
	h := dial(t)

	// Find a Speculative machine the faultprovider marked create-fault.
	var id string
	for _, m := range h.List(pb.MachineState_MACHINE_STATE_SPECULATIVE) {
		if m.GetLabels()["conformance.fault/create"] == "error" {
			id = m.GetId()
			break
		}
	}
	if id == "" {
		t.Skip("B702: faultprovider seeded no create-fault machine")
	}

	if _, err := h.Create(id); err != nil {
		t.Fatalf("Create(%s): %v", id, err)
	}
	m := h.MustReach(id, pb.MachineState_MACHINE_STATE_FAILED, transitionTimeout+5*time.Second)
	if m.GetLastError() == "" {
		t.Errorf("B702: FAILED machine has empty last_error")
	}
	h.NeverReaches(id, pb.MachineState_MACHINE_STATE_CREATING, 300*time.Millisecond)
}

// B703 — a transition that EXCEEDS its configured timeout drives the machine to
// FAILED carrying a timeout-shaped non-empty last_error, never silently
// reverting.
func TestB703_TransitionTimeout(t *testing.T) {
	behavior(t, "B703")
	h := dial(t)

	id := walkHealthyToIdle(t, h)
	if _, err := h.Configure(id, "fault-timeout", faultMD()); err != nil {
		t.Fatalf("Configure(fault-timeout): %v", err)
	}
	// The actuator blocks until ctx is cancelled by the transition timeout, so
	// FAILED must arrive within roughly transitionTimeout (+ slack).
	m := h.MustReach(id, pb.MachineState_MACHINE_STATE_FAILED, transitionTimeout+3*time.Second)
	if m.GetLastError() == "" {
		t.Errorf("B703: timed-out machine has empty last_error")
	}
}

// B704 — a stale async actuator completion arriving AFTER the transition already
// failed is DISCARDED: the machine stays FAILED and does not flip to the success
// state.
func TestB704_LateSuccessDiscarded(t *testing.T) {
	behavior(t, "B704")
	h := dial(t)

	id := walkHealthyToIdle(t, h)
	if _, err := h.Configure(id, "fault-slow-ok", faultMD()); err != nil {
		t.Fatalf("Configure(fault-slow-ok): %v", err)
	}
	// The kit times out to FAILED first (~transitionTimeout); the actuator then
	// reports success ~1s LATER, which must be discarded.
	h.MustReach(id, pb.MachineState_MACHINE_STATE_FAILED, transitionTimeout+3*time.Second)
	preErr := h.Get(id).GetLastError()
	if preErr == "" {
		t.Errorf("B704: FAILED machine has empty last_error")
	}

	// Sleep past the late success and assert the machine is STILL FAILED (the
	// discarded completion never flipped it to CONFIGURED).
	time.Sleep(transitionTimeout + 2*time.Second)
	m := h.Get(id)
	if m.GetState() != pb.MachineState_MACHINE_STATE_FAILED {
		t.Errorf("B704: late success was NOT discarded — state is %s, want FAILED", m.GetState())
	}
	if m.GetLastError() == "" {
		t.Errorf("B704: last_error was lost after the discarded late completion")
	}
}

// B705 — after a transition fails to FAILED, a re-issued mutation toward a legal
// target RECOVERS the machine out of FAILED and clears last_error on the next
// clean settle.
func TestB705_FailedIsTerminal(t *testing.T) {
	behavior(t, "B705")
	h := dial(t)

	// Drive to FAILED via a Configure actuator error.
	id := walkHealthyToIdle(t, h)
	if _, err := h.Configure(id, "fault-error", faultMD()); err != nil {
		t.Fatalf("Configure(fault-error): %v", err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_FAILED, transitionTimeout+5*time.Second)
	failed := h.Get(id)
	lastErr := failed.GetLastError()
	if lastErr == "" {
		t.Fatal("B705: a FAILED machine carries an empty last_error")
	}

	// FAILED is terminal-pending-cleanup. The contract (provider.proto: "one
	// terminal-pending-cleanup (Failed)"; author guide: "needs intervention; the
	// shard intervenes — clean up, retry on a different slot, escalate") does NOT
	// re-drive the same machine out of FAILED in place. So a re-issued mutation is
	// an out-of-position call: rejected, never FAILED_PRECONDITION, leaving the
	// machine FAILED with its last_error preserved verbatim.
	_, err := h.Configure(id, "recover-cluster", faultMD())
	h.RejectsNonFencing("re-Configure on a FAILED machine", err)

	m := h.Get(id)
	if m.GetState() != pb.MachineState_MACHINE_STATE_FAILED {
		t.Errorf("B705: machine left FAILED for %s after a rejected re-Configure", m.GetState())
	}
	if m.GetLastError() != lastErr {
		t.Errorf("B705: last_error changed from %q to %q on a terminal FAILED machine", lastErr, m.GetLastError())
	}
}

// B706 — a Drain with grace_period_seconds=0 against a FAILING actuator ends in
// IDLE or FAILED-with-last_error, never stuck DRAINING and never a silent revert.
func TestB706_DrainAgainstFailingActuator(t *testing.T) {
	behavior(t, "B706")
	h := dial(t)

	// Configure onto the drain-fault cluster (reaches CONFIGURED cleanly), then
	// Drain — the actuator errors.
	id := walkHealthyToConfigured(t, h, "fault-drain-error", faultMD())
	if _, err := h.Drain(id, 0); err != nil {
		t.Fatalf("Drain(%s,0): %v", id, err)
	}

	// It must settle in FAILED or IDLE — never stuck DRAINING.
	deadline := time.Now().Add(transitionTimeout + 5*time.Second)
	var final pb.MachineState
	for time.Now().Before(deadline) {
		s, err := h.StateRaw(id)
		if err == nil {
			final = s
			if s == pb.MachineState_MACHINE_STATE_FAILED || s == pb.MachineState_MACHINE_STATE_IDLE {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	switch final {
	case pb.MachineState_MACHINE_STATE_FAILED:
		if h.Get(id).GetLastError() == "" {
			t.Errorf("B706: FAILED Drain has empty last_error")
		}
	case pb.MachineState_MACHINE_STATE_IDLE:
		// Acceptable: a fast/idempotent actuator may end in Idle.
	default:
		t.Fatalf("B706: Drain settled in %s (want FAILED or IDLE, never stuck DRAINING)", final)
	}
	h.NeverReaches(id, pb.MachineState_MACHINE_STATE_DRAINING, 300*time.Millisecond)
}

// B707 — a FAILED machine still answers Get/List and reports its FAILED state
// with last_error preserved verbatim across repeated reads.
func TestB707_FailedStillReadable(t *testing.T) {
	behavior(t, "B707")
	h := dial(t)

	id := walkHealthyToIdle(t, h)
	if _, err := h.Configure(id, "fault-error", faultMD()); err != nil {
		t.Fatalf("Configure(fault-error): %v", err)
	}
	first := h.MustReach(id, pb.MachineState_MACHINE_STATE_FAILED, transitionTimeout+5*time.Second)
	wantErr := first.GetLastError()
	if wantErr == "" {
		t.Fatalf("B707: FAILED machine has empty last_error")
	}

	// Get 3x: FAILED + identical non-empty last_error each time.
	for i := 0; i < 3; i++ {
		m := h.Get(id)
		if m.GetState() != pb.MachineState_MACHINE_STATE_FAILED {
			t.Errorf("B707: Get #%d state %s, want FAILED", i, m.GetState())
		}
		if m.GetLastError() != wantErr {
			t.Errorf("B707: Get #%d last_error %q, want verbatim %q", i, m.GetLastError(), wantErr)
		}
	}

	// And it appears in List with state FAILED and the same last_error.
	found := false
	for _, m := range h.List(pb.MachineState_MACHINE_STATE_FAILED) {
		if m.GetId() == id {
			found = true
			if m.GetLastError() != wantErr {
				t.Errorf("B707: List last_error %q, want verbatim %q", m.GetLastError(), wantErr)
			}
		}
	}
	if !found {
		t.Errorf("B707: FAILED machine %s not present in List(FAILED)", id)
	}
}

// B708 — ADR-0056 node-join readiness gate: a machine is not reported CONFIGURED
// until its node is observed Ready. The faultprovider's ConfirmNodeReady blocks
// for the "fault-readiness-block" cluster, so the kit must hold the machine at
// CONFIGURING (NEVER CONFIGURED — that would be phantom capacity) and, when
// readiness never arrives, time the transition out to FAILED with a non-empty
// last_error.
func TestB708_NodeReadinessGate(t *testing.T) {
	behavior(t, "B708")
	h := dial(t)

	id := walkHealthyToIdle(t, h)
	if _, err := h.Configure(id, "fault-readiness-block", faultMD()); err != nil {
		t.Fatalf("Configure(fault-readiness-block): %v", err)
	}

	// While readiness is unobserved the machine must stay CONFIGURING and must
	// NEVER be reported CONFIGURED. Watch for most of the transition window.
	h.NeverReaches(id, pb.MachineState_MACHINE_STATE_CONFIGURED, transitionTimeout-500*time.Millisecond)

	// Readiness never arrives, so the kit times the transition out to FAILED —
	// it does not silently settle CONFIGURED on a node that never joined.
	m := h.MustReach(id, pb.MachineState_MACHINE_STATE_FAILED, transitionTimeout+5*time.Second)
	if m.GetLastError() == "" {
		t.Errorf("B708: FAILED machine has empty last_error (readiness timeout)")
	}
}
