// Package harness is the black-box client the BigFleet provider conformance
// program drives every provider through. It speaks only the six wire RPCs of
// bigfleet.v1alpha1.CapacityProvider — no providerkit imports, no process
// introspection — so it certifies ANY provider that implements the contract,
// in-tree or out, Go or not.
//
// It deliberately mirrors the upstream suite's conventions (run-unique
// shard_ids so reruns against a long-lived provider never collide; aggressive
// Get polling to observe transitional states) and adds the helpers the
// extension suite needs: a capability probe, a full-state walker, a sequence
// source, and Eventually/Consistently polling.
package harness

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

// H is a connected conformance harness for one provider endpoint.
type H struct {
	t      *testing.T
	Client pb.CapacityProviderClient
	conn   *grpc.ClientConn
	seq    *uint64
}

// Dial connects to the provider at addr (insecure transport; the suite is a
// black-box wire test). It registers cleanup on t.
func Dial(t *testing.T, addr string) *H {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("harness: dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return DialConn(t, conn)
}

// DialConn builds a harness over an already-constructed gRPC connection. It lets
// callers that need non-default dial options (e.g. the scale lane raising
// MaxCallRecvMsgSize so a full List of a very large fleet does not exceed the
// default ~4MB recv limit) reuse every harness primitive. The caller owns the
// connection's lifecycle (registering its own t.Cleanup to Close it).
func DialConn(t *testing.T, conn *grpc.ClientConn) *H {
	t.Helper()
	var seq uint64
	return &H{t: t, Client: pb.NewCapacityProviderClient(conn), conn: conn, seq: &seq}
}

// next returns a process-unique monotonically increasing counter, used to mint
// run-unique ids and shard_ids.
func (h *H) next() uint64 { return atomic.AddUint64(h.seq, 1) }

// UniqueShardID mints a shard_id no prior run/test has used, so fencing
// high-water marks never collide across reruns against a long-lived provider.
func (h *H) UniqueShardID(prefix string) string {
	return fmt.Sprintf("conf-%s-%d-%d", prefix, time.Now().UnixNano(), h.next())
}

// Ctx returns a context with a sensible per-call timeout.
func (h *H) Ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// --- inventory helpers ----------------------------------------------------

// List returns all machines (optionally filtered by states).
func (h *H) List(states ...pb.MachineState) []*pb.Machine {
	h.t.Helper()
	ctx, cancel := h.Ctx()
	defer cancel()
	resp, err := h.Client.List(ctx, &pb.ListFilter{States: states})
	if err != nil {
		h.t.Fatalf("List: %v", err)
	}
	return resp.GetMachines()
}

// PickSpeculative returns one Speculative machine id, skipping the test if the
// provider seeded none.
func (h *H) PickSpeculative() string {
	h.t.Helper()
	ctx, cancel := h.Ctx()
	defer cancel()
	resp, err := h.Client.List(ctx, &pb.ListFilter{
		States:     []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE},
		MaxResults: 1,
	})
	if err != nil {
		h.t.Fatalf("List speculative: %v", err)
	}
	if len(resp.GetMachines()) == 0 {
		h.t.Skip("conformance: provider has no Speculative machines; seed at least one")
	}
	return resp.GetMachines()[0].GetId()
}

// Get returns one machine.
func (h *H) Get(id string) *pb.Machine {
	h.t.Helper()
	ctx, cancel := h.Ctx()
	defer cancel()
	m, err := h.Client.Get(ctx, &pb.MachineRef{Id: id})
	if err != nil {
		h.t.Fatalf("Get(%s): %v", id, err)
	}
	return m
}

// GetRaw returns Get's result and error without failing the test, for
// asserting error codes (NotFound / InvalidArgument).
func (h *H) GetRaw(id string) (*pb.Machine, error) {
	ctx, cancel := h.Ctx()
	defer cancel()
	return h.Client.Get(ctx, &pb.MachineRef{Id: id})
}

// State reads one machine's current state.
func (h *H) State(id string) pb.MachineState { return h.Get(id).GetState() }

// MustReach polls Get until the machine reaches want or timeout elapses.
func (h *H) MustReach(id string, want pb.MachineState, timeout time.Duration) *pb.Machine {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := h.Ctx()
		m, err := h.Client.Get(ctx, &pb.MachineRef{Id: id})
		cancel()
		if err == nil && m.GetState() == want {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("machine %s did not reach %s within %s (final %s)", id, want, timeout, h.State(id))
	return nil
}

// --- lifecycle drivers (zero fencing token = unfenced, like the upstream non-
// fencing tests) ----------------------------------------------------------

func (h *H) Create(id string) (*pb.TransitionAck, error) {
	ctx, cancel := h.Ctx()
	defer cancel()
	return h.Client.Create(ctx, &pb.CreateRequest{MachineId: id})
}

func (h *H) Configure(id, cluster string, md map[string]string) (*pb.TransitionAck, error) {
	ctx, cancel := h.Ctx()
	defer cancel()
	return h.Client.Configure(ctx, &pb.ConfigureRequest{
		MachineId: id, ClusterId: cluster, BootstrapBlob: []byte("# conformance\n"), ShardMetadata: md,
	})
}

func (h *H) Drain(id string, grace int64) (*pb.TransitionAck, error) {
	ctx, cancel := h.Ctx()
	defer cancel()
	return h.Client.Drain(ctx, &pb.DrainRequest{MachineId: id, GracePeriodSeconds: grace})
}

func (h *H) Delete(id string) (*pb.TransitionAck, error) {
	ctx, cancel := h.Ctx()
	defer cancel()
	return h.Client.Delete(ctx, &pb.DeleteRequest{MachineId: id})
}

// FencedCreate is the standard fencing probe: Create is idempotent on
// (machine, target=Idle), so absent fencing a repeat succeeds — any
// FAILED_PRECONDITION can only have come from the token.
func (h *H) FencedCreate(id, shard string, epoch, seq int64) error {
	ctx, cancel := h.Ctx()
	defer cancel()
	_, err := h.Client.Create(ctx, &pb.CreateRequest{
		MachineId: id, ShardId: shard, ShardEpoch: epoch, SequenceNumber: seq,
	})
	return err
}

// WalkToIdle drives a fresh Speculative machine to Idle and returns its id.
func (h *H) WalkToIdle() string {
	h.t.Helper()
	id := h.PickSpeculative()
	if _, err := h.Create(id); err != nil {
		h.t.Fatalf("Create(%s): %v", id, err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)
	return id
}

// WalkToConfigured drives a fresh machine to Configured for the given cluster
// and metadata, returning its id.
func (h *H) WalkToConfigured(cluster string, md map[string]string) string {
	h.t.Helper()
	id := h.WalkToIdle()
	if _, err := h.Configure(id, cluster, md); err != nil {
		h.t.Fatalf("Configure(%s): %v", id, err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 15*time.Second)
	return id
}

// --- assertions -----------------------------------------------------------

// Code returns the gRPC code of err (OK for nil).
func Code(err error) codes.Code { return status.Code(err) }

// RejectsNonFencing asserts err is a rejection that is NOT FAILED_PRECONDITION
// (which the contract reserves for fencing) and not OK.
func (h *H) RejectsNonFencing(what string, err error) {
	h.t.Helper()
	if err == nil {
		h.t.Errorf("%s: expected a rejection, got success", what)
		return
	}
	if Code(err) == codes.FailedPrecondition {
		h.t.Errorf("%s: rejected with FAILED_PRECONDITION, which is reserved for fencing (got %v)", what, err)
	}
}

// --- capability probe -----------------------------------------------------

// Capabilities describes what a provider supports, driving profile
// applicability and skips.
type Capabilities struct {
	Delete        bool // implements Delete (not Unimplemented)
	SinceRevision bool // advances List.revision (incremental cursor)
}

// Probe detects provider capabilities without leaving durable side effects it
// can't undo. It walks one throwaway machine.
func (h *H) Probe() Capabilities {
	h.t.Helper()
	caps := Capabilities{}

	// since_revision: does the revision advance across a mutation?
	r0 := h.listRevision()
	id := h.WalkToIdle()
	r1 := h.listRevision()
	caps.SinceRevision = len(r0) > 0 && string(r0) != string(r1)

	// Delete: probe on the Idle machine. Unimplemented => no Delete.
	_, err := h.Delete(id)
	caps.Delete = Code(err) != codes.Unimplemented
	if caps.Delete && err == nil {
		h.MustReach(id, pb.MachineState_MACHINE_STATE_SPECULATIVE, 15*time.Second)
	}
	return caps
}

func (h *H) listRevision() []byte {
	ctx, cancel := h.Ctx()
	defer cancel()
	resp, err := h.Client.List(ctx, &pb.ListFilter{})
	if err != nil {
		h.t.Fatalf("List revision: %v", err)
	}
	return resp.GetRevision()
}
