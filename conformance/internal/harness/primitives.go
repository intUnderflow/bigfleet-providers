package harness

// This file adds the higher-order primitives the Kubernetes-scale conformance
// areas are built on, on top of the wire-level helpers in harness.go. Every
// primitive here is still PURE WIRE — it speaks only the six RPCs + gRPC status
// codes, imports no providerkit, and does no process introspection — so the
// extension suite continues to certify any provider, in-tree or out.

import (
	"flag"
	"math/rand"
	"sort"
	"sync"
	"time"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

// --- state predicates (the lifecycle oracle) ------------------------------

// IsTransitional reports whether s is one of the four in-flight states a
// machine passes through while an actuator runs.
func IsTransitional(s pb.MachineState) bool {
	switch s {
	case pb.MachineState_MACHINE_STATE_CREATING,
		pb.MachineState_MACHINE_STATE_CONFIGURING,
		pb.MachineState_MACHINE_STATE_DRAINING,
		pb.MachineState_MACHINE_STATE_DELETING:
		return true
	}
	return false
}

// IsStable reports whether s is a settled state a machine can rest in.
func IsStable(s pb.MachineState) bool {
	switch s {
	case pb.MachineState_MACHINE_STATE_SPECULATIVE,
		pb.MachineState_MACHINE_STATE_IDLE,
		pb.MachineState_MACHINE_STATE_CONFIGURED,
		pb.MachineState_MACHINE_STATE_FAILED:
		return true
	}
	return false
}

// StateRaw reads a machine's state WITHOUT failing the test on a Get error
// (returns UNSPECIFIED + the error). Safe to call from a goroutine — unlike
// State/Get, it never calls t.Fatalf (which would runtime.Goexit off-test). The
// polling primitives below use it so a momentary mid-transition Get error is a
// skipped sample, not a spurious test failure.
func (h *H) StateRaw(id string) (pb.MachineState, error) {
	m, err := h.GetRaw(id)
	if err != nil {
		return pb.MachineState_MACHINE_STATE_UNSPECIFIED, err
	}
	return m.GetState(), nil
}

// --- negative-stability polling -------------------------------------------

// NeverReaches polls over window and fails if the machine ever enters bad. The
// negative-stability primitive (e.g. a no-op at target must not transition
// through a forbidden state). Keep window small (200-500ms is plenty against a
// fast backend).
func (h *H) NeverReaches(id string, bad pb.MachineState, window time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if s, err := h.StateRaw(id); err == nil && s == bad {
			h.t.Fatalf("machine %s entered forbidden state %s within stability window", id, bad)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// StaysIn asserts the machine remains in want for the whole window (a no-op or
// rejected mutation must not silently move the machine).
func (h *H) StaysIn(id string, want pb.MachineState, window time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if s, err := h.StateRaw(id); err == nil && s != want {
			h.t.Fatalf("machine %s left %s for %s within stability window", id, want, s)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- transitional observation (best-effort) -------------------------------

// ObserveTransitional polls fast (1ms floor) until the machine reaches settle
// or timeout, reporting whether want (a transitional state) was seen en route.
// Best-effort: a fast actuator may skip the window entirely, so callers MUST
// tolerate seen==false. Returns (seenWant, reachedSettle).
func (h *H) ObserveTransitional(id string, want, settle pb.MachineState, timeout time.Duration) (seen, reached bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s, err := h.StateRaw(id); err == nil {
			if s == want {
				seen = true
			}
			if s == settle {
				return seen, true
			}
		}
		time.Sleep(time.Millisecond)
	}
	return seen, false
}

// StateTrace records the ordered, de-duplicated sequence of states observed
// until the machine reaches until or timeout (best-effort fast sampling).
func (h *H) StateTrace(id string, until pb.MachineState, timeout time.Duration) []pb.MachineState {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var trace []pb.MachineState
	for time.Now().Before(deadline) {
		s, err := h.StateRaw(id)
		if err == nil {
			if len(trace) == 0 || trace[len(trace)-1] != s {
				trace = append(trace, s)
			}
			if s == until {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	return trace
}

// --- generic fenced dispatch + shard session ------------------------------

// RPC identifies one of the four mutating RPCs for generic fenced dispatch.
type RPC int

const (
	RPCCreate RPC = iota
	RPCConfigure
	RPCDrain
	RPCDelete
)

func (r RPC) String() string {
	switch r {
	case RPCCreate:
		return "Create"
	case RPCConfigure:
		return "Configure"
	case RPCDrain:
		return "Drain"
	case RPCDelete:
		return "Delete"
	}
	return "RPC?"
}

// FencedCall issues rpc against machine id carrying an explicit fencing token
// (shard, epoch, seq), filling benign defaults for the rpc's other args, and
// returns only the error. This lets fencing invariants — fence-before-idempotency,
// fence-before-not-found, mark-advances-on-pass-even-if-the-op-fails — be
// exercised uniformly on all four RPCs, not just Create. Goroutine-safe.
func (h *H) FencedCall(rpc RPC, id, shard string, epoch, seq int64) error {
	ctx, cancel := h.Ctx()
	defer cancel()
	switch rpc {
	case RPCCreate:
		_, err := h.Client.Create(ctx, &pb.CreateRequest{
			MachineId: id, ShardId: shard, ShardEpoch: epoch, SequenceNumber: seq,
		})
		return err
	case RPCConfigure:
		_, err := h.Client.Configure(ctx, &pb.ConfigureRequest{
			MachineId: id, ClusterId: "conf-cluster", BootstrapBlob: []byte("# conformance\n"),
			ShardId: shard, ShardEpoch: epoch, SequenceNumber: seq,
		})
		return err
	case RPCDrain:
		_, err := h.Client.Drain(ctx, &pb.DrainRequest{
			MachineId: id, GracePeriodSeconds: 0, ShardId: shard, ShardEpoch: epoch, SequenceNumber: seq,
		})
		return err
	case RPCDelete:
		_, err := h.Client.Delete(ctx, &pb.DeleteRequest{
			MachineId: id, ShardId: shard, ShardEpoch: epoch, SequenceNumber: seq,
		})
		return err
	}
	h.t.Fatalf("FencedCall: unknown rpc %d", rpc)
	return nil
}

// ShardSession models a shard's fencing identity: a stable shard_id plus a
// monotonic (epoch, seq) that auto-advances per mutating call, with NewEpoch to
// model a restart / leader change (epoch++, seq resets). Safe for concurrent
// use (the counter is mutex-guarded), so a session can drive a Fanout race.
type ShardSession struct {
	h     *H
	ID    string
	mu    sync.Mutex
	epoch int64
	seq   int64
}

// NewShard mints a run-unique shard with epoch=1, seq starting at 1.
func (h *H) NewShard(prefix string) *ShardSession {
	return &ShardSession{h: h, ID: h.UniqueShardID(prefix), epoch: 1, seq: 1}
}

// Epoch and Seq expose the token this session will send on its next Do.
func (s *ShardSession) Epoch() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.epoch }
func (s *ShardSession) Seq() int64   { s.mu.Lock(); defer s.mu.Unlock(); return s.seq }

// NewEpoch bumps the epoch and resets seq to 1 (models a shard restart).
func (s *ShardSession) NewEpoch() { s.mu.Lock(); defer s.mu.Unlock(); s.epoch++; s.seq = 1 }

// tick returns the current (epoch, seq) and advances seq for the next call.
func (s *ShardSession) tick() (int64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, q := s.epoch, s.seq
	s.seq++
	return e, q
}

// Do issues rpc with this session's next monotonic token (seq advances per CALL
// regardless of outcome — modelling a shard that always picks a fresh seq).
func (s *ShardSession) Do(rpc RPC, id string) error {
	e, q := s.tick()
	return s.h.FencedCall(rpc, id, s.ID, e, q)
}

// Stale issues rpc with an explicit (possibly stale) token WITHOUT advancing
// the session — for replaying a superseded token after the mark has moved on.
func (s *ShardSession) Stale(rpc RPC, id string, epoch, seq int64) error {
	return s.h.FencedCall(rpc, id, s.ID, epoch, seq)
}

// --- concurrency fan-out --------------------------------------------------

// AckErr pairs a mutating RPC's ack with its error, for concurrent fan-out.
type AckErr struct {
	Ack *pb.TransitionAck
	Err error
}

// Fanout runs fn(i) for i in [0,n) concurrently and returns results in order.
// The backbone of the concurrency / idempotency-under-load behaviors. fn MUST
// NOT call t.Fatalf/Skip (that runtime.Goexit's a non-test goroutine and is
// undefined) — return an error/result and assert it on the main goroutine.
func Fanout[T any](n int, fn func(i int) T) []T {
	out := make([]T, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) { defer wg.Done(); out[i] = fn(i) }(i)
	}
	wg.Wait()
	return out
}

// FanoutCreate fires n concurrent Create calls on the same machine id and
// returns each (ack, err) — the canonical idempotency-under-load probe.
func (h *H) FanoutCreate(id string, n int) []AckErr {
	return Fanout(n, func(int) AckErr {
		ack, err := h.Create(id)
		return AckErr{Ack: ack, Err: err}
	})
}

// FanoutConfigure fires n concurrent identical Configure calls on id.
func (h *H) FanoutConfigure(id, cluster string, md map[string]string, n int) []AckErr {
	return Fanout(n, func(int) AckErr {
		ack, err := h.Configure(id, cluster, md)
		return AckErr{Ack: ack, Err: err}
	})
}

// AssertSingleOperationID asserts that across concurrent idempotent retries,
// every SUCCEEDING ack carries the same non-empty operation_id (exactly one
// distinct id) — proving the provider collapsed the racing calls into a single
// effect. Returns that operation_id.
func (h *H) AssertSingleOperationID(what string, results []AckErr) string {
	h.t.Helper()
	ids := map[string]struct{}{}
	succeeded, emptyID := 0, 0
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		succeeded++
		opID := r.Ack.GetOperationId()
		if opID == "" {
			emptyID++
			continue
		}
		ids[opID] = struct{}{}
	}
	if succeeded == 0 {
		h.t.Fatalf("%s: no concurrent call succeeded", what)
	}
	if emptyID > 0 {
		h.t.Errorf("%s: %d/%d successful acks carried an empty operation_id", what, emptyID, succeeded)
	}
	if len(ids) != 1 {
		h.t.Errorf("%s: expected exactly one distinct operation_id across %d successful retries, got %d: %v",
			what, succeeded, len(ids), sortedKeys(ids))
	}
	for id := range ids {
		return id
	}
	return ""
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- bulk seed acquisition (scale) ----------------------------------------

// PickNSpeculative returns up to n distinct Speculative ids, skipping the test
// if the provider seeded fewer than n.
func (h *H) PickNSpeculative(n int) []string {
	h.t.Helper()
	if n <= 0 {
		h.t.Fatalf("PickNSpeculative needs n>=1, got %d", n)
	}
	ctx, cancel := h.Ctx()
	defer cancel()
	resp, err := h.Client.List(ctx, &pb.ListFilter{
		States:     []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE},
		MaxResults: int32(n),
	})
	if err != nil {
		h.t.Fatalf("List speculative(%d): %v", n, err)
	}
	ms := resp.GetMachines()
	// De-dup defensively (List is a set; a conformant provider won't repeat,
	// but never silently hand back fewer-than-n distinct machines).
	seen := map[string]struct{}{}
	ids := make([]string, 0, n)
	for _, m := range ms {
		id := m.GetId()
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) < n {
		h.t.Skipf("conformance: need %d distinct Speculative machines, provider has %d; seed more", n, len(ids))
	}
	return ids[:n]
}

// WalkNToIdle drives n fresh Speculative machines to Idle (Creates fan out
// concurrently) and returns their ids.
func (h *H) WalkNToIdle(n int) []string {
	h.t.Helper()
	ids := h.PickNSpeculative(n)
	errs := Fanout(n, func(i int) error {
		_, err := h.Create(ids[i])
		return err
	})
	for i, err := range errs {
		if err != nil {
			h.t.Fatalf("Create(%s): %v", ids[i], err)
		}
	}
	for _, id := range ids {
		h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 30*time.Second)
	}
	return ids
}

// --- revision / delta List ------------------------------------------------

// Revision returns the current List revision cursor.
func (h *H) Revision() []byte {
	h.t.Helper()
	ctx, cancel := h.Ctx()
	defer cancel()
	resp, err := h.Client.List(ctx, &pb.ListFilter{})
	if err != nil {
		h.t.Fatalf("Revision: %v", err)
	}
	return resp.GetRevision()
}

// ListSince returns the machines changed since rev (the delta) plus the new
// revision. Order-independent; feed back only bytes a prior List emitted (a
// non-cursor is treated as a full list, not an error). since_revision is
// optional/capability-gated — callers should Probe().SinceRevision first.
func (h *H) ListSince(rev []byte, states ...pb.MachineState) ([]*pb.Machine, []byte) {
	h.t.Helper()
	ctx, cancel := h.Ctx()
	defer cancel()
	resp, err := h.Client.List(ctx, &pb.ListFilter{States: states, SinceRevision: rev})
	if err != nil {
		h.t.Fatalf("ListSince: %v", err)
	}
	return resp.GetMachines(), resp.GetRevision()
}

// ListMax returns up to max machines — an arbitrary bounded subset, since List
// has no guaranteed order. Callers must assert membership/count, never position.
func (h *H) ListMax(max int32, states ...pb.MachineState) []*pb.Machine {
	h.t.Helper()
	ctx, cancel := h.Ctx()
	defer cancel()
	resp, err := h.Client.List(ctx, &pb.ListFilter{States: states, MaxResults: max})
	if err != nil {
		h.t.Fatalf("ListMax(%d): %v", max, err)
	}
	return resp.GetMachines()
}

// IDsOf returns the ids of a machine slice (order preserved).
func IDsOf(ms []*pb.Machine) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.GetId())
	}
	return out
}

// --- deterministic randomness (property / fuzz) ---------------------------

var (
	seedFlag    = flag.Int64("seed", 1, "deterministic RNG seed for property/fuzz conformance behaviors")
	seedLogOnce sync.Once
)

// Rand returns a deterministic RNG seeded by -seed (default 1). Call ONCE per
// test and reuse the returned *rand.Rand across iterations. The seed is logged
// once per run so a failing property run is replayable.
func (h *H) Rand() *rand.Rand {
	seedLogOnce.Do(func() {
		h.t.Logf("conformance: property RNG seed=%d (override with -seed)", *seedFlag)
	})
	return rand.New(rand.NewSource(*seedFlag))
}
