//go:build certify

package suite

import (
	"testing"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

// C9 — List filtering, max_results, and since_revision (behaviors B90x).

// Filtering by each state returns only machines in that state (swept across
// every state, not just IDLE as upstream does).
func TestList_FilterByEveryState(t *testing.T) {
	h := dial(t)
	for _, st := range []pb.MachineState{
		pb.MachineState_MACHINE_STATE_SPECULATIVE,
		pb.MachineState_MACHINE_STATE_IDLE,
		pb.MachineState_MACHINE_STATE_CONFIGURED,
		pb.MachineState_MACHINE_STATE_CREATING,
		pb.MachineState_MACHINE_STATE_FAILED,
	} {
		for _, m := range h.List(st) {
			if m.GetState() != st {
				t.Errorf("List(%s) returned machine %s in state %s", st, m.GetId(), m.GetState())
			}
		}
	}
}

// Multi-state filter returns the union and nothing else.
func TestList_FilterUnion(t *testing.T) {
	h := dial(t)
	want := map[pb.MachineState]bool{
		pb.MachineState_MACHINE_STATE_SPECULATIVE: true,
		pb.MachineState_MACHINE_STATE_IDLE:        true,
	}
	ctx, cancel := h.Ctx()
	defer cancel()
	resp, err := h.Client.List(ctx, &pb.ListFilter{States: []pb.MachineState{
		pb.MachineState_MACHINE_STATE_SPECULATIVE, pb.MachineState_MACHINE_STATE_IDLE,
	}})
	if err != nil {
		t.Fatalf("List union: %v", err)
	}
	for _, m := range resp.GetMachines() {
		if !want[m.GetState()] {
			t.Errorf("union filter returned %s in state %s", m.GetId(), m.GetState())
		}
	}
}

// max_results is an upper bound, swept across several caps.
func TestList_MaxResults(t *testing.T) {
	h := dial(t)
	for _, n := range []int32{1, 2, 3} {
		ctx, cancel := h.Ctx()
		resp, err := h.Client.List(ctx, &pb.ListFilter{MaxResults: n})
		cancel()
		if err != nil {
			t.Fatalf("List(max=%d): %v", n, err)
		}
		if got := int32(len(resp.GetMachines())); got > n {
			t.Errorf("List(MaxResults=%d) returned %d machines", n, got)
		}
	}
}

// since_revision: the revision advances after a mutation, and a delta List
// since an earlier cursor includes the mutated machine. Providers below the
// since_revision threshold may return the same revision every cycle — detected
// via the capability probe and skipped cleanly.
func TestList_SinceRevisionDelta(t *testing.T) {
	h := dial(t)
	caps := h.Probe()
	if !caps.SinceRevision {
		t.Skip("provider does not advance List.revision (since_revision is opt-in below ~10k machines)")
	}

	ctx, cancel := h.Ctx()
	r0resp, err := h.Client.List(ctx, &pb.ListFilter{})
	cancel()
	if err != nil {
		t.Fatalf("List r0: %v", err)
	}
	r0 := r0resp.GetRevision()

	id := h.WalkToIdle() // a mutation

	ctx2, cancel2 := h.Ctx()
	r1resp, err := h.Client.List(ctx2, &pb.ListFilter{})
	cancel2()
	if err != nil {
		t.Fatalf("List r1: %v", err)
	}
	if string(r0) == string(r1resp.GetRevision()) {
		t.Fatal("revision did not advance after a mutation")
	}

	// A delta since r0 must include the just-mutated machine.
	ctx3, cancel3 := h.Ctx()
	delta, err := h.Client.List(ctx3, &pb.ListFilter{SinceRevision: r0})
	cancel3()
	if err != nil {
		t.Fatalf("List delta: %v", err)
	}
	found := false
	for _, m := range delta.GetMachines() {
		if m.GetId() == id {
			found = true
		}
	}
	if !found {
		t.Errorf("delta List(since=r0) did not include the mutated machine %s", id)
	}
}
