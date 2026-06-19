package providerkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

// --- async dispatch -------------------------------------------------------

func TestCreateIsAsync_AckReportsTransitional(t *testing.T) {
	s, _ := newTestServer(t, 4)
	id := firstSpeculative(t, s)

	ack := create(t, s, id)
	if ack.GetOperationId() == "" {
		t.Fatal("Create returned empty operation_id")
	}
	// The ack is returned immediately with the machine already in the
	// transitional Creating state — not blocked until Idle.
	if got := ack.GetMachine().GetState(); got != pb.MachineState_MACHINE_STATE_CREATING {
		t.Errorf("ack state = %s, want CREATING (Create must be async)", got)
	}
	// The transition then completes in the background, observable via Get.
	m := waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)
	if m.GetHost().GetRef() == "" {
		t.Error("Idle machine has no host attached")
	}
}

func TestFullLifecycle(t *testing.T) {
	s, _ := newTestServer(t, 4)
	id := firstSpeculative(t, s)

	create(t, s, id)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)

	configure(t, s, id, "cluster-a", map[string]string{"k": "v"})
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED, 2*time.Second)

	if _, err := s.Drain(bg(), &pb.DrainRequest{MachineId: id, GracePeriodSeconds: 5}); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)

	if _, err := s.Delete(bg(), &pb.DeleteRequest{MachineId: id}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	m := waitState(t, s, id, pb.MachineState_MACHINE_STATE_SPECULATIVE, 2*time.Second)
	if m.GetHost().GetRef() != "" {
		t.Error("Speculative machine still has a host after Delete")
	}
}

// --- idempotency ----------------------------------------------------------

func TestIdempotentCreate_SameOperationID(t *testing.T) {
	s, _ := newTestServer(t, 4)
	id := firstSpeculative(t, s)

	a := create(t, s, id)
	b := create(t, s, id) // retry, mid-Creating or after Idle
	if a.GetOperationId() != b.GetOperationId() {
		t.Errorf("operation_id changed across idempotent Create: %s vs %s", a.GetOperationId(), b.GetOperationId())
	}
}

func TestIdempotency_SurvivesStoreRoundTrip(t *testing.T) {
	store := NewMemStore()
	b := &fakeBackend{seed: speculativeSeed(2, CapacityOnDemand, 0)}
	s1, err := New(b, store, quietOptions())
	if err != nil {
		t.Fatalf("New s1: %v", err)
	}
	id := firstSpeculative(t, s1)
	a := create(t, s1, id)
	waitState(t, s1, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)

	// Restart: a brand-new Server over the same (persisted) store must
	// reload the idempotency map and return the same operation_id.
	s2, err := New(b, store, quietOptions())
	if err != nil {
		t.Fatalf("New s2: %v", err)
	}
	if got := getMachine(t, s2, id).GetState(); got != pb.MachineState_MACHINE_STATE_IDLE {
		t.Fatalf("reloaded machine state = %s, want IDLE", got)
	}
	c, err := s2.Create(bg(), &pb.CreateRequest{MachineId: id})
	if err != nil {
		t.Fatalf("Create after restart: %v", err)
	}
	if c.GetOperationId() != a.GetOperationId() {
		t.Errorf("operation_id not preserved across restart: %s vs %s", c.GetOperationId(), a.GetOperationId())
	}
}

func TestConfigureAndDrainIdempotent(t *testing.T) {
	s, _ := newTestServer(t, 4)
	id := firstSpeculative(t, s)
	create(t, s, id)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)

	c1 := configure(t, s, id, "c", nil)
	c2 := configure(t, s, id, "c", nil)
	if c1.GetOperationId() != c2.GetOperationId() {
		t.Errorf("Configure not idempotent: %s vs %s", c1.GetOperationId(), c2.GetOperationId())
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED, 2*time.Second)

	d1, _ := s.Drain(bg(), &pb.DrainRequest{MachineId: id, GracePeriodSeconds: 1})
	d2, _ := s.Drain(bg(), &pb.DrainRequest{MachineId: id, GracePeriodSeconds: 1})
	if d1.GetOperationId() != d2.GetOperationId() {
		t.Errorf("Drain not idempotent: %s vs %s", d1.GetOperationId(), d2.GetOperationId())
	}
}

// --- fencing --------------------------------------------------------------

func fenced(s *Server, id, shard string, epoch, seq int64) error {
	_, err := s.Create(bg(), &pb.CreateRequest{
		MachineId: id, ShardId: shard, ShardEpoch: epoch, SequenceNumber: seq,
	})
	return err
}

func TestFencing_UnknownShardAccepted_ThenReplayRejected(t *testing.T) {
	s, _ := newTestServer(t, 4)
	id := firstSpeculative(t, s)

	if err := fenced(s, id, "shard-x", 1, 1); err != nil {
		t.Fatalf("first contact from unknown shard must be accepted: %v", err)
	}
	if got := codeOf(fenced(s, id, "shard-x", 1, 1)); got != codes.FailedPrecondition {
		t.Errorf("replay of accepted token: code = %s, want FailedPrecondition", got)
	}
}

func TestFencing_StaleEpochAndSequenceRejected(t *testing.T) {
	s, _ := newTestServer(t, 8)
	id := firstSpeculative(t, s)

	if err := fenced(s, id, "shard-y", 2, 5); err != nil {
		t.Fatalf("establish mark: %v", err)
	}
	if got := codeOf(fenced(s, id, "shard-y", 1, 99)); got != codes.FailedPrecondition {
		t.Errorf("stale epoch: code = %s, want FailedPrecondition", got)
	}
	if got := codeOf(fenced(s, id, "shard-y", 2, 5)); got != codes.FailedPrecondition {
		t.Errorf("equal token (replay): code = %s, want FailedPrecondition", got)
	}
	if got := codeOf(fenced(s, id, "shard-y", 2, 4)); got != codes.FailedPrecondition {
		t.Errorf("lower sequence: code = %s, want FailedPrecondition", got)
	}
}

func TestFencing_NewEpochResetsSequence(t *testing.T) {
	s, _ := newTestServer(t, 4)
	id := firstSpeculative(t, s)
	if err := fenced(s, id, "shard-z", 1, 1000); err != nil {
		t.Fatalf("establish mark: %v", err)
	}
	// A restarted shard's higher epoch with a low sequence must be accepted.
	if err := fenced(s, id, "shard-z", 2, 1); err != nil {
		t.Errorf("new epoch with low sequence must be accepted: %v", err)
	}
}

func TestFencing_MarkAdvancesEvenWhenOpFails(t *testing.T) {
	s, b := newTestServer(t, 4)
	b.setCreate(func(context.Context, CreateInstanceRequest) (CreateInstanceResult, error) {
		return CreateInstanceResult{}, errors.New("synthetic backend failure")
	})
	id := firstSpeculative(t, s)

	// The op will fail asynchronously, but the fence mark advances at
	// dispatch time and must stick.
	if err := fenced(s, id, "shard-f", 3, 7); err != nil {
		t.Fatalf("create with fresh token must be accepted at dispatch: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_FAILED, 2*time.Second)

	// The mark is now (3,7) despite the failure: not-strictly-newer tokens
	// are rejected.
	if got := codeOf(fenced(s, id, "shard-f", 3, 7)); got != codes.FailedPrecondition {
		t.Errorf("replay after failed op: code = %s, want FailedPrecondition (mark must have advanced)", got)
	}
	if got := codeOf(fenced(s, id, "shard-f", 3, 6)); got != codes.FailedPrecondition {
		t.Errorf("lower seq after failed op: code = %s, want FailedPrecondition", got)
	}
}

func TestFencing_BeforeIdempotency_ZombieGetsNoCachedOp(t *testing.T) {
	s, _ := newTestServer(t, 4)
	id := firstSpeculative(t, s)

	// Establish the mark and drive the machine to Idle (the Create target).
	if err := fenced(s, id, "shard-zombie", 5, 5); err != nil {
		t.Fatalf("first create: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)

	// A replay with the SAME token would, absent fence-first ordering, hit
	// the idempotency short-circuit (machine is at the Idle target) and
	// return a cached operation_id — telling the zombie its mutation
	// "succeeded". Fence-first must reject it instead.
	if got := codeOf(fenced(s, id, "shard-zombie", 5, 5)); got != codes.FailedPrecondition {
		t.Errorf("zombie replay at target state: code = %s, want FailedPrecondition (fence before idempotency)", got)
	}
}

func TestFencing_ReadsAreUnfenced(t *testing.T) {
	s, _ := newTestServer(t, 4)
	id := firstSpeculative(t, s)
	if err := fenced(s, id, "shard-r", 9, 9); err != nil {
		t.Fatalf("establish mark: %v", err)
	}
	// A fenced-out mutation right before the reads.
	if got := codeOf(fenced(s, id, "shard-r", 1, 1)); got != codes.FailedPrecondition {
		t.Fatalf("stale token: code = %s, want FailedPrecondition", got)
	}
	if _, err := s.Get(bg(), &pb.MachineRef{Id: id}); err != nil {
		t.Errorf("Get after fenced mutation: %v", err)
	}
	if _, err := s.List(bg(), &pb.ListFilter{}); err != nil {
		t.Errorf("List after fenced mutation: %v", err)
	}
}

func TestFencing_ZeroTokenBypasses(t *testing.T) {
	s, _ := newTestServer(t, 4)
	id := firstSpeculative(t, s)
	// Two zero-token Creates in a row must both be accepted (idempotent),
	// not rejected as a non-advancing token. This is exactly what the
	// conformance suite's non-fencing tests rely on.
	if _, err := s.Create(bg(), &pb.CreateRequest{MachineId: id}); err != nil {
		t.Fatalf("first zero-token Create: %v", err)
	}
	if _, err := s.Create(bg(), &pb.CreateRequest{MachineId: id}); err != nil {
		t.Fatalf("second zero-token Create must be accepted (idempotent), got: %v", err)
	}
}

// --- out-of-position transitions -----------------------------------------

func TestOutOfPosition_DrainOnSpeculative_NotFailedPrecondition(t *testing.T) {
	s, _ := newTestServer(t, 4)
	id := firstSpeculative(t, s)
	_, err := s.Drain(bg(), &pb.DrainRequest{MachineId: id, GracePeriodSeconds: 5})
	if err == nil {
		t.Fatal("Drain on Speculative must fail")
	}
	if codeOf(err) == codes.FailedPrecondition {
		t.Errorf("invalid transition used FAILED_PRECONDITION (reserved for fencing): %v", err)
	}
	if codeOf(err) != codes.Internal {
		t.Errorf("invalid transition code = %s, want Internal (matching the fake)", codeOf(err))
	}
}

func TestOutOfPosition_DeleteOnConfigured_RejectedAndUntouched(t *testing.T) {
	s, _ := newTestServer(t, 4)
	id := firstSpeculative(t, s)
	create(t, s, id)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)
	configure(t, s, id, "c", nil)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED, 2*time.Second)

	_, err := s.Delete(bg(), &pb.DeleteRequest{MachineId: id})
	if err == nil {
		t.Fatal("Delete on Configured must fail")
	}
	if codeOf(err) == codes.FailedPrecondition {
		t.Errorf("invalid transition used FAILED_PRECONDITION (reserved for fencing): %v", err)
	}
	if got := getMachine(t, s, id).GetState(); got != pb.MachineState_MACHINE_STATE_CONFIGURED {
		t.Errorf("machine state after rejected Delete = %s, want CONFIGURED (no partial transition)", got)
	}
}

// --- Delete capability ----------------------------------------------------

func TestDelete_Unsupported_ReturnsUnimplemented(t *testing.T) {
	b := &bareBackend{seed: speculativeSeed(2, CapacityBareMetal, 0)}
	s, err := New(b, NewMemStore(), quietOptions())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := firstSpeculative(t, s)
	create(t, s, id)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)

	_, err = s.Delete(bg(), &pb.DeleteRequest{MachineId: id})
	if codeOf(err) != codes.Unimplemented {
		t.Errorf("Delete on a non-Deleter backend: code = %s, want Unimplemented", codeOf(err))
	}
	// The machine must be untouched (still Idle).
	if got := getMachine(t, s, id).GetState(); got != pb.MachineState_MACHINE_STATE_IDLE {
		t.Errorf("machine state after unsupported Delete = %s, want IDLE", got)
	}
}

// --- not-found ------------------------------------------------------------

func TestGetUnknown_NotFound(t *testing.T) {
	s, _ := newTestServer(t, 1)
	_, err := s.Get(bg(), &pb.MachineRef{Id: "nope"})
	if codeOf(err) != codes.NotFound {
		t.Errorf("Get unknown: code = %s, want NotFound", codeOf(err))
	}
}

func TestCreateUnknown_NotFound(t *testing.T) {
	s, _ := newTestServer(t, 1)
	_, err := s.Create(bg(), &pb.CreateRequest{MachineId: "nope"})
	if codeOf(err) != codes.NotFound {
		t.Errorf("Create unknown: code = %s, want NotFound", codeOf(err))
	}
}

// --- transition timeout → Failed -----------------------------------------

func TestTransitionTimeout_MovesToFailedWithLastError(t *testing.T) {
	b := &fakeBackend{seed: speculativeSeed(2, CapacityOnDemand, 0)}
	b.setCreate(func(ctx context.Context, _ CreateInstanceRequest) (CreateInstanceResult, error) {
		<-ctx.Done() // never completes within the timeout
		return CreateInstanceResult{}, ctx.Err()
	})
	opts := quietOptions()
	opts.Timeouts.Create = 30 * time.Millisecond
	s, err := New(b, NewMemStore(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := firstSpeculative(t, s)
	create(t, s, id)

	m := waitState(t, s, id, pb.MachineState_MACHINE_STATE_FAILED, 2*time.Second)
	if m.GetLastError() == "" {
		t.Error("Failed machine has empty last_error")
	}
}

func TestTransitionBackendError_MovesToFailed(t *testing.T) {
	b := &fakeBackend{seed: speculativeSeed(2, CapacityOnDemand, 0)}
	b.setCreate(func(context.Context, CreateInstanceRequest) (CreateInstanceResult, error) {
		return CreateInstanceResult{}, errors.New("boom")
	})
	s, err := New(b, NewMemStore(), quietOptions())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := firstSpeculative(t, s)
	create(t, s, id)
	m := waitState(t, s, id, pb.MachineState_MACHINE_STATE_FAILED, 2*time.Second)
	if m.GetLastError() == "" {
		t.Error("Failed machine has empty last_error")
	}
}

// --- List behaviour -------------------------------------------------------

func TestList_FilterMaxResultsAndRevision(t *testing.T) {
	s, _ := newTestServer(t, 5)

	// Filter by state.
	resp, err := s.List(bg(), &pb.ListFilter{States: []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.GetMachines()) != 5 {
		t.Errorf("List(SPECULATIVE) = %d, want 5", len(resp.GetMachines()))
	}
	for _, m := range resp.GetMachines() {
		if m.GetState() != pb.MachineState_MACHINE_STATE_SPECULATIVE {
			t.Errorf("filtered list returned non-Speculative %s", m.GetId())
		}
	}

	// Cap.
	capped, _ := s.List(bg(), &pb.ListFilter{MaxResults: 2})
	if len(capped.GetMachines()) != 2 {
		t.Errorf("List(MaxResults=2) = %d, want 2", len(capped.GetMachines()))
	}

	// Revision advances after a mutation.
	r0, _ := s.List(bg(), &pb.ListFilter{})
	id := firstSpeculative(t, s)
	create(t, s, id)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)
	r1, _ := s.List(bg(), &pb.ListFilter{})
	if string(r0.GetRevision()) == string(r1.GetRevision()) {
		t.Error("revision did not advance after a mutation")
	}
}

func TestList_SinceRevisionReturnsOnlyChanged(t *testing.T) {
	s, _ := newTestServer(t, 4)
	r0, _ := s.List(bg(), &pb.ListFilter{})
	id := firstSpeculative(t, s)
	create(t, s, id)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)

	delta, _ := s.List(bg(), &pb.ListFilter{SinceRevision: r0.GetRevision()})
	for _, m := range delta.GetMachines() {
		if m.GetId() != id {
			t.Errorf("delta List returned unchanged machine %s", m.GetId())
		}
	}
	if len(delta.GetMachines()) == 0 {
		t.Error("delta List returned nothing after a mutation")
	}
}

// Ensure the proto snapshot in a TransitionAck carries the well-known fields
// top-level, never only in labels.
func TestFieldShape_TopLevelNotLabels(t *testing.T) {
	s, _ := newTestServer(t, 2)
	resp, _ := s.List(bg(), &pb.ListFilter{})
	for _, m := range resp.GetMachines() {
		if m.GetInstanceType() == "" {
			t.Errorf("machine %s: instance_type empty (must be top-level)", m.GetId())
		}
		if m.GetCapacityType() == pb.CapacityType_CAPACITY_TYPE_UNSPECIFIED {
			t.Errorf("machine %s: capacity_type unspecified", m.GetId())
		}
		if _, ok := m.GetLabels()["instance-type"]; ok {
			t.Errorf("machine %s: instance_type leaked into labels", m.GetId())
		}
	}
}
