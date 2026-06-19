//go:build certify

package suite

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"

	"github.com/intUnderflow/bigfleet-providers/conformance/internal/harness"
)

// C2 — the out-of-position transition MATRIX (behaviors B200x).
//
// The state machine has exactly four legal mutating edges:
//
//	Create@Speculative  Configure@Idle  Drain@Configured  Delete@Idle
//
// Every other (RPC × stable-source-state) pair that is NOT a legal edge and is
// NOT an idempotent no-op (an RPC whose target the machine already occupies)
// MUST be rejected — with a code that is NOT FAILED_PRECONDITION (reserved for
// fencing) — and MUST leave the machine's state unchanged (no partial
// transition). The upstream suite checks two of these cells (Drain@Speculative,
// Delete@Configured); this exercises the full deterministic stable-state
// complement and additionally asserts the no-partial-transition invariant on
// every cell.

// stateSetup puts a fresh machine into a stable source state and returns its id.
type stateSetup struct {
	name  string
	state pb.MachineState
	setup func(h *harness.H) string
}

func speculativeSetup() stateSetup {
	return stateSetup{"Speculative", pb.MachineState_MACHINE_STATE_SPECULATIVE,
		func(h *harness.H) string { return h.PickSpeculative() }}
}
func idleSetup() stateSetup {
	return stateSetup{"Idle", pb.MachineState_MACHINE_STATE_IDLE,
		func(h *harness.H) string { return h.WalkToIdle() }}
}
func configuredSetup() stateSetup {
	return stateSetup{"Configured", pb.MachineState_MACHINE_STATE_CONFIGURED,
		func(h *harness.H) string { return h.WalkToConfigured("conf-matrix", nil) }}
}

// illegalCell is one out-of-position call to verify.
type illegalCell struct {
	rpc    string
	source stateSetup
	call   func(h *harness.H, id string) error
	// deleteCell cells are skipped-as-pass when the provider returns
	// Unimplemented (bare-metal free pool).
	deleteCell bool
}

func matrixCells() []illegalCell {
	create := func(h *harness.H, id string) error { _, err := h.Create(id); return err }
	configure := func(h *harness.H, id string) error { _, err := h.Configure(id, "x", nil); return err }
	drain := func(h *harness.H, id string) error { _, err := h.Drain(id, 5); return err }
	del := func(h *harness.H, id string) error { _, err := h.Delete(id); return err }
	return []illegalCell{
		// Create is legal only on Speculative.
		{"Create", configuredSetup(), create, false},
		// Configure is legal only on Idle.
		{"Configure", speculativeSetup(), configure, false},
		// Drain is legal only on Configured.
		{"Drain", speculativeSetup(), drain, false}, // also upstream
		{"Drain", idleSetup(), drain, false},
		// Delete is legal only on Idle.
		{"Delete", speculativeSetup(), del, true},
		{"Delete", configuredSetup(), del, true}, // also upstream
	}
}

func TestMatrix_OutOfPositionRejected(t *testing.T) {
	for _, c := range matrixCells() {
		c := c
		t.Run(c.rpc+"_on_"+c.source.name, func(t *testing.T) {
			th := dial(t)
			id := c.source.setup(th)
			before := th.State(id)
			if before != c.source.state {
				t.Fatalf("setup landed in %s, want %s", before, c.source.state)
			}
			err := c.call(th, id)
			if c.deleteCell && harness.Code(err) == codes.Unimplemented {
				t.Skipf("%s not implemented (bare-metal free pool) — OK", c.rpc)
			}
			th.RejectsNonFencing(c.rpc+" on "+c.source.name, err)
			// No partial transition: the machine is exactly where it was.
			if got := th.State(id); got != before {
				t.Errorf("%s on %s changed state to %s (must be a no-op rejection)", c.rpc, c.source.name, got)
			}
		})
	}
}

// An RPC whose target the machine already occupies is an IDEMPOTENT no-op, not
// a rejection — the complement of the matrix above. Re-Configuring a Configured
// machine and re-Creating... (Create's target Idle) must succeed idempotently.
func TestMatrix_IdempotentNoOpAtTarget(t *testing.T) {
	h := dial(t)

	// Configure on an already-Configured machine: same operation_id, no error.
	id := h.WalkToConfigured("conf-idem", nil)
	a, err := h.Configure(id, "conf-idem", nil)
	if err != nil {
		t.Fatalf("re-Configure at Configured: %v", err)
	}
	if a.GetMachine().GetState() != pb.MachineState_MACHINE_STATE_CONFIGURED {
		t.Errorf("re-Configure moved a Configured machine to %s", a.GetMachine().GetState())
	}

	// Create on an already-Idle machine (it reached Idle via Create): idempotent.
	id2 := h.WalkToIdle()
	if _, err := h.Create(id2); err != nil {
		t.Errorf("re-Create at Idle should be an idempotent no-op, got: %v", err)
	}
	if got := h.State(id2); got != pb.MachineState_MACHINE_STATE_IDLE {
		t.Errorf("re-Create moved an Idle machine to %s", got)
	}
}

// Unknown-machine handling across all four mutating RPCs + Get (behaviors
// B205x). Get/Create/Configure/Drain → NotFound; Delete → NotFound or
// Unimplemented.
func TestMatrix_UnknownMachine(t *testing.T) {
	h := dial(t)
	const ghost = "conformance-definitely-not-real-9e3f"

	if _, err := h.GetRaw(ghost); harness.Code(err) != codes.NotFound {
		t.Errorf("Get unknown: code %s, want NotFound", harness.Code(err))
	}
	if _, err := h.Create(ghost); harness.Code(err) != codes.NotFound {
		t.Errorf("Create unknown: code %s, want NotFound", harness.Code(err))
	}
	if _, err := h.Configure(ghost, "x", nil); harness.Code(err) != codes.NotFound {
		t.Errorf("Configure unknown: code %s, want NotFound", harness.Code(err))
	}
	if _, err := h.Drain(ghost, 1); harness.Code(err) != codes.NotFound {
		t.Errorf("Drain unknown: code %s, want NotFound", harness.Code(err))
	}
	switch _, err := h.Delete(ghost); harness.Code(err) {
	case codes.NotFound, codes.Unimplemented: // both OK
	default:
		t.Errorf("Delete unknown: code %s, want NotFound or Unimplemented", harness.Code(err))
	}
}

// Empty machine_id → InvalidArgument on every mutating RPC + Get.
func TestMatrix_EmptyMachineID(t *testing.T) {
	h := dial(t)
	if _, err := h.Create(""); harness.Code(err) != codes.InvalidArgument {
		t.Errorf("Create empty id: code %s, want InvalidArgument", harness.Code(err))
	}
	if _, err := h.Configure("", "x", nil); harness.Code(err) != codes.InvalidArgument {
		t.Errorf("Configure empty id: code %s, want InvalidArgument", harness.Code(err))
	}
	if _, err := h.Drain("", 1); harness.Code(err) != codes.InvalidArgument {
		t.Errorf("Drain empty id: code %s, want InvalidArgument", harness.Code(err))
	}
	if _, err := h.GetRaw(""); harness.Code(err) != codes.InvalidArgument {
		t.Errorf("Get empty id: code %s, want InvalidArgument", harness.Code(err))
	}
}

// A repeated full lifecycle leaves no residue: cluster, shard_metadata, and
// last_error are all clear every time the machine returns to a clean Idle.
func TestLifecycle_RepeatedCyclesNoResidue(t *testing.T) {
	h := dial(t)
	id := h.WalkToIdle()
	cycles := 4
	for i := 0; i < cycles; i++ {
		if _, err := h.Configure(id, "cycle", map[string]string{"k": "v"}); err != nil {
			t.Fatalf("cycle %d Configure: %v", i, err)
		}
		h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 15*time.Second)
		if _, err := h.Drain(id, 5); err != nil {
			t.Fatalf("cycle %d Drain: %v", i, err)
		}
		m := h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)
		if m.GetCluster() != "" {
			t.Errorf("cycle %d: cluster %q survived Drain", i, m.GetCluster())
		}
		if len(m.GetShardMetadata()) != 0 {
			t.Errorf("cycle %d: shard_metadata %v survived Drain", i, m.GetShardMetadata())
		}
		if m.GetLastError() != "" {
			t.Errorf("cycle %d: last_error %q on a clean Idle", i, m.GetLastError())
		}
	}
}
