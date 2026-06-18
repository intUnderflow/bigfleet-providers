package providerkit

import (
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

func TestFenceMark_Newer(t *testing.T) {
	mark := FenceMark{Epoch: 2, Sequence: 5}
	cases := []struct {
		f    Fence
		want bool
	}{
		{Fence{ShardEpoch: 3, SequenceNumber: 0}, true},   // higher epoch, any seq
		{Fence{ShardEpoch: 2, SequenceNumber: 6}, true},   // same epoch, higher seq
		{Fence{ShardEpoch: 2, SequenceNumber: 5}, false},  // equal
		{Fence{ShardEpoch: 2, SequenceNumber: 4}, false},  // lower seq
		{Fence{ShardEpoch: 1, SequenceNumber: 99}, false}, // lower epoch
	}
	for _, tc := range cases {
		if got := mark.newer(tc.f); got != tc.want {
			t.Errorf("FenceMark%+v.newer(Fence%+v) = %v, want %v", mark, tc.f, got, tc.want)
		}
	}
}

func TestFence_Zero(t *testing.T) {
	if !(Fence{}).zero() {
		t.Error("empty Fence must be zero")
	}
	if (Fence{ShardID: "s"}).zero() {
		t.Error("Fence with shard_id is not zero")
	}
	if (Fence{SequenceNumber: 1}).zero() {
		t.Error("Fence with sequence is not zero")
	}
}

// A transition in flight when the process dies is reloaded with its
// transitional state, but the goroutine + timeout that would drive it are
// gone. The kit must surface it as FAILED on restart rather than leaving it
// stuck forever (an idempotent retry would otherwise short-circuit on the
// persisted operation_id without re-dispatching).
func TestRestart_InterruptedTransitionBecomesFailed(t *testing.T) {
	store := NewMemStore()
	if err := store.Save(Snapshot{
		Rev:    5,
		NextOp: 1,
		Machines: []*Machine{{
			ID: "m1", State: StateCreating,
			InstanceType: "x.large", CapacityType: CapacityOnDemand, PricePerHour: 1,
		}},
		Ops: []OpRecord{{MachineID: "m1", Kind: "create", OperationID: "op-1"}},
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	// Describe is NOT consulted on a non-empty store, so an empty backend is fine.
	s, err := New(&fakeBackend{}, store, quietOptions())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := getMachine(t, s, "m1")
	if m.GetState() != pb.MachineState_MACHINE_STATE_FAILED {
		t.Errorf("interrupted Creating machine after restart = %s, want FAILED", m.GetState())
	}
	if m.GetLastError() == "" {
		t.Error("recovered machine has empty last_error")
	}
}

// FileStore must durably round-trip the full state, and a Server rebuilt
// from it must preserve fence marks, the idempotency map, the inventory, and
// the cluster binding + shard_metadata.
func TestFileStore_RoundTripPreservesEverything(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	store1, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	b := &fakeBackend{seed: speculativeSeed(3, CapacityOnDemand, 0)}
	s1, err := New(b, store1, quietOptions())
	if err != nil {
		t.Fatalf("New s1: %v", err)
	}
	id := firstSpeculative(t, s1)

	// Establish a fence mark, and drive the machine to Configured with
	// metadata.
	if _, err := s1.Create(bg(), &pb.CreateRequest{MachineId: id, ShardId: "shard-1", ShardEpoch: 4, SequenceNumber: 2}); err != nil {
		t.Fatalf("fenced Create: %v", err)
	}
	waitState(t, s1, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)
	md := map[string]string{"bigfleet.lucy.sh/assigned-priority": "5", "x/opaque": "keep"}
	if _, err := s1.Configure(bg(), &pb.ConfigureRequest{
		MachineId: id, ClusterId: "cl", BootstrapBlob: []byte("b"), ShardMetadata: md,
		ShardId: "shard-1", ShardEpoch: 4, SequenceNumber: 3,
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	waitState(t, s1, id, pb.MachineState_MACHINE_STATE_CONFIGURED, 2*time.Second)
	_ = store1.Close()

	// Reopen: a fresh Server over the same file must reload everything.
	store2, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("reopen FileStore: %v", err)
	}
	s2, err := New(b, store2, quietOptions())
	if err != nil {
		t.Fatalf("New s2: %v", err)
	}

	// Inventory + binding + metadata preserved.
	got := getMachine(t, s2, id)
	if got.GetState() != pb.MachineState_MACHINE_STATE_CONFIGURED {
		t.Errorf("reloaded state = %s, want CONFIGURED", got.GetState())
	}
	if got.GetCluster() != "cl" {
		t.Errorf("reloaded cluster = %q, want cl", got.GetCluster())
	}
	if got.GetShardMetadata()["x/opaque"] != "keep" {
		t.Errorf("reloaded shard_metadata lost keys: %v", got.GetShardMetadata())
	}

	// Fence marks preserved: a not-newer token is still rejected after the
	// restart (the zombie window did not re-open).
	_, err = s2.Drain(bg(), &pb.DrainRequest{MachineId: id, ShardId: "shard-1", ShardEpoch: 4, SequenceNumber: 3})
	if codeOf(err) != codes.FailedPrecondition {
		t.Errorf("stale token after restart: code = %s, want FailedPrecondition (fence marks must survive)", codeOf(err))
	}
}
