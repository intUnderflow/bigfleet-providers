package providerkit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

// Server is a complete, contract-correct pb.CapacityProviderServer built
// around a substrate-specific [Backend]. It owns every cross-cutting
// obligation — fencing, idempotency, async dispatch, transition timeouts,
// the shard_metadata lifecycle, field-shape — so the Backend only ever
// speaks substrate. Construct it with [New] and register it on a gRPC
// server with [Server.Register].
type Server struct {
	pb.UnimplementedCapacityProviderServer

	backend   Backend
	deleter   Deleter
	canDelete bool
	store     Store
	opts      Options
	logger    *slog.Logger

	// mu guards every field below it. Reads (Get/List) take RLock; mutating
	// RPCs and async completions take Lock. Store.Save runs under the lock,
	// so a mutation and its durable record are atomic with respect to other
	// RPCs.
	mu       sync.RWMutex
	machines map[string]*Machine
	fences   map[fenceKey]FenceMark
	ops      map[opKey]string
	lastMod  map[string]int64
	rev      int64
	nextOp   int64
}

// opKey identifies the most recent transition of a given kind on a machine.
// Idempotency is keyed here: (machine_id, kind) — Create and Drain both
// target Idle, so the kind disambiguates them.
type opKey struct {
	id string
	k  kind
}

// Timeouts bounds how long each transition may take before the kit gives up
// and moves the machine to Failed. Set them to the backend's worst case (a
// cloud Create of 30–90s is not a 5s request timeout; a Drain of a strict-PDB
// workload can take hours).
type Timeouts struct {
	Create    time.Duration
	Configure time.Duration
	Drain     time.Duration
	Delete    time.Duration
}

// Options configures a [Server]. The zero value is valid; every field has a
// sensible default.
type Options struct {
	// Timeouts bounds each transition (default: Create/Configure/Delete 5m,
	// Drain 10m).
	Timeouts Timeouts
	// RequireZone rejects seeded machines that omit a zone. Leave false for
	// single-zone providers (the conformance contract permits an empty
	// zone); set true for multi-zone providers so a missing zone is caught
	// at startup rather than mis-placing pods later.
	RequireZone bool
	// ReconcileTimeout bounds the backend Describe call used at startup
	// seeding and by Reconcile (default 30s).
	ReconcileTimeout time.Duration
	// OperationIDPrefix prefixes minted operation ids (default "op").
	OperationIDPrefix string
	// Logger receives kit-level warnings (persist failures, skipped
	// reconcile records). Defaults to slog.Default().
	Logger *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Timeouts.Create == 0 {
		o.Timeouts.Create = 5 * time.Minute
	}
	if o.Timeouts.Configure == 0 {
		o.Timeouts.Configure = 5 * time.Minute
	}
	if o.Timeouts.Drain == 0 {
		o.Timeouts.Drain = 10 * time.Minute
	}
	if o.Timeouts.Delete == 0 {
		o.Timeouts.Delete = 5 * time.Minute
	}
	if o.ReconcileTimeout == 0 {
		o.ReconcileTimeout = 30 * time.Second
	}
	if o.OperationIDPrefix == "" {
		o.OperationIDPrefix = "op"
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// New constructs a Server around backend and store. On a fresh store it
// seeds the authoritative inventory from backend.Describe (validating every
// record against the field-shape contract — a missing instance_type /
// capacity_type, an out-of-bounds cost input, or a SPOT machine with no
// interruption probability fails startup loudly). On a non-empty store it
// reloads the persisted fence marks, idempotency map and inventory verbatim,
// so a restart loses nothing.
func New(backend Backend, store Store, opts Options) (*Server, error) {
	if backend == nil {
		return nil, errors.New("providerkit: nil backend")
	}
	if store == nil {
		return nil, errors.New("providerkit: nil store")
	}
	opts = opts.withDefaults()
	s := &Server{
		backend:  backend,
		store:    store,
		opts:     opts,
		logger:   opts.Logger,
		machines: make(map[string]*Machine),
		fences:   make(map[fenceKey]FenceMark),
		ops:      make(map[opKey]string),
		lastMod:  make(map[string]int64),
	}
	s.deleter, s.canDelete = backend.(Deleter)
	if _, ok := backend.(ReadinessChecker); !ok {
		// ADR-0056: without a readiness gate the kit reports Configured as soon
		// as ConfigureInstance returns. That is correct only if ConfigureInstance
		// itself blocks until the node is Ready; otherwise the provider can credit
		// phantom capacity. Surface it once so the operator knows the posture.
		s.logger.Warn("providerkit: backend does not implement ReadinessChecker; the ADR-0056 node-join readiness gate is NOT enforced by the kit — Configured is reported as soon as ConfigureInstance returns")
	}

	snap, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("providerkit: load store: %w", err)
	}
	if isFresh(snap) {
		ctx, cancel := context.WithTimeout(context.Background(), opts.ReconcileTimeout)
		defer cancel()
		if err := s.seedFromBackend(ctx); err != nil {
			return nil, err
		}
	} else {
		s.loadSnapshot(snap)
		s.recoverInterrupted()
	}
	return s, nil
}

// recoverInterrupted handles transitions that were in flight when the process
// died. Their state is persisted, but the goroutine driving them — and the
// timeout that would have moved them to Failed — did not survive the restart,
// so without this they would sit in a transitional state forever (and an
// idempotent retry would short-circuit on the persisted operation_id without
// re-dispatching). The kit does not persist the per-call inputs needed to
// re-invoke a backend actuator (notably Configure's bootstrap blob), so the
// honest, contract-aligned move is to surface each interrupted transition as
// MACHINE_STATE_FAILED with last_error — the designed signal for "this
// transition did not complete; intervene" — and let the shard take corrective
// action. Called once, single-threaded, from New.
func (s *Server) recoverInterrupted() {
	changed := false
	for _, m := range s.machines {
		switch m.State {
		case StateCreating, StateConfiguring, StateDraining, StateDeleting:
			m.LastError = fmt.Sprintf("%s transition interrupted by a provider restart; needs re-drive", transitionalKind(m.State))
			m.State = StateFailed
			s.touchLocked(m.ID)
			changed = true
		}
	}
	if changed {
		s.persistLocked()
	}
}

// transitionalKind names the operation that leaves a machine in the given
// transitional state, for recovery diagnostics.
func transitionalKind(s State) string {
	switch s {
	case StateCreating:
		return "create"
	case StateConfiguring:
		return "configure"
	case StateDraining:
		return "drain"
	case StateDeleting:
		return "delete"
	default:
		return "unknown"
	}
}

// isFresh reports whether a loaded snapshot represents an un-initialised
// store (first boot), in which case the kit seeds from the backend.
func isFresh(s Snapshot) bool {
	return len(s.Machines) == 0 && len(s.Fences) == 0 && len(s.Ops) == 0 && s.Rev == 0
}

func (s *Server) seedFromBackend(ctx context.Context) error {
	instances, err := s.backend.Describe(ctx)
	if err != nil {
		return fmt.Errorf("providerkit: backend Describe (seed): %w", err)
	}
	for _, in := range instances {
		if err := in.validate(s.opts.RequireZone); err != nil {
			return err
		}
		if _, dup := s.machines[in.ID]; dup {
			return fmt.Errorf("%w: backend Describe returned duplicate id %q", ErrInvalidMachine, in.ID)
		}
		s.rev++
		s.machines[in.ID] = in.toMachine()
		s.lastMod[in.ID] = s.rev
	}
	s.persistLocked()
	return nil
}

func (s *Server) loadSnapshot(snap Snapshot) {
	s.rev = snap.Rev
	s.nextOp = snap.NextOp
	for _, m := range snap.Machines {
		s.machines[m.ID] = m.clone()
		// Reset every record's modification revision to the loaded revision
		// so a stale List cursor (issued before the restart) sees a full
		// re-list rather than silently missing records.
		s.lastMod[m.ID] = snap.Rev
	}
	for _, r := range snap.Fences {
		s.fences[fenceKey{ShardID: r.ShardID, MachineID: r.MachineID}] = FenceMark{Epoch: r.Epoch, Sequence: r.Sequence}
	}
	for _, r := range snap.Ops {
		s.ops[opKey{id: r.MachineID, k: kindFromString(r.Kind)}] = r.OperationID
	}
}

// Register registers the server on a gRPC service registrar.
func (s *Server) Register(reg grpc.ServiceRegistrar) {
	pb.RegisterCapacityProviderServer(reg, s)
}

// Reconcile re-reads the backend's substrate inventory: it adds machines the
// kit does not yet track (new Speculative quota or a pre-existing free pool),
// and REFRESHES the mutable substrate facts of machines it already tracks —
// price_per_hour, interruption_probability, resources, allocatable, labels —
// which change over time (a spot price moves; an interruption signal raises the
// probability). It never touches the kit-owned overlay (state, host, cluster,
// shard_metadata, last_error), so the lifecycle/binding always wins and it is
// safe to call periodically. Invalid records are skipped with a warning rather
// than crashing a running provider; an unchanged reconcile bumps no revision.
func (s *Server) Reconcile(ctx context.Context) error {
	instances, err := s.backend.Describe(ctx)
	if err != nil {
		return fmt.Errorf("providerkit: backend Describe (reconcile): %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, in := range instances {
		if err := in.validate(s.opts.RequireZone); err != nil {
			s.logger.Warn("providerkit: reconcile skipping invalid instance", "id", in.ID, "err", err)
			continue
		}
		cur, exists := s.machines[in.ID]
		if !exists {
			s.machines[in.ID] = in.toMachine()
			s.touchLocked(in.ID)
			changed = true
			continue
		}
		if refreshSubstrate(cur, in) {
			s.touchLocked(in.ID)
			changed = true
		}
	}
	if changed {
		s.persistLocked()
	}
	return nil
}

// refreshSubstrate updates a tracked machine's mutable substrate facts from a
// fresh Describe, preserving the kit-owned lifecycle/binding fields. It returns
// whether anything actually changed (so an idempotent reconcile bumps no
// revision). instance_type and zone are treated as immutable identity and are
// not refreshed.
func refreshSubstrate(cur *Machine, in Instance) bool {
	changed := false
	if cur.PricePerHour != in.PricePerHour {
		cur.PricePerHour = in.PricePerHour
		changed = true
	}
	if cur.InterruptionProbability != in.InterruptionProbability {
		cur.InterruptionProbability = in.InterruptionProbability
		changed = true
	}
	if !mapsEqual(cur.Resources, in.Resources) {
		cur.Resources = cloneMap(in.Resources)
		changed = true
	}
	if !mapsEqual(cur.Allocatable, in.Allocatable) {
		cur.Allocatable = cloneMap(in.Allocatable)
		changed = true
	}
	if !mapsEqual(cur.Labels, in.Labels) {
		cur.Labels = cloneMap(in.Labels)
		changed = true
	}
	return changed
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// --- the six RPCs ---------------------------------------------------------

// Create implements pb.CapacityProviderServer: Speculative → Creating → Idle.
func (s *Server) Create(_ context.Context, req *pb.CreateRequest) (*pb.TransitionAck, error) {
	if req.GetMachineId() == "" {
		return nil, status.Error(codes.InvalidArgument, "machine_id required")
	}
	op := func(ctx context.Context, m Machine, opID string) (func(*Machine), error) {
		res, err := s.backend.CreateInstance(ctx, CreateInstanceRequest{Machine: m, OperationID: opID})
		if err != nil {
			return nil, err
		}
		if res.Host.empty() {
			return nil, errors.New("CreateInstance returned no host for a created machine")
		}
		return func(dst *Machine) {
			dst.Host = res.Host
			if res.Resources != nil {
				dst.Resources = cloneMap(res.Resources)
			}
			if res.Allocatable != nil {
				dst.Allocatable = cloneMap(res.Allocatable)
			}
		}, nil
	}
	return s.dispatch(kindCreate, req.GetMachineId(), fenceOf(req.GetShardId(), req.GetShardEpoch(), req.GetSequenceNumber()), s.opts.Timeouts.Create, nil, op)
}

// Configure implements pb.CapacityProviderServer: Idle → Configuring →
// Configured. It stores the cluster binding and shard_metadata verbatim on
// success.
func (s *Server) Configure(_ context.Context, req *pb.ConfigureRequest) (*pb.TransitionAck, error) {
	if req.GetMachineId() == "" {
		return nil, status.Error(codes.InvalidArgument, "machine_id required")
	}
	clusterID := req.GetClusterId()
	blob := req.GetBootstrapBlob()
	// Store-and-echo, never interpret: copy the map verbatim, unknown keys
	// included, so a later caller mutation can't reach the stored record.
	md := cloneMap(req.GetShardMetadata())
	// The binding is established at accept time, not at completion: the proto
	// says cluster/shard_metadata are populated for the whole CONFIGURING →
	// CONFIGURED span, so they must be visible the moment the machine enters
	// Configuring (a CONFIGURING record with no cluster trips the shard's
	// M70b structural-rejection tripwire). The backend op then only confirms
	// the substrate side; the success post-effect is a no-op.
	pre := func(m *Machine) {
		m.Cluster = clusterID
		m.ShardMetadata = md
	}
	op := func(ctx context.Context, m Machine, opID string) (func(*Machine), error) {
		if err := s.backend.ConfigureInstance(ctx, ConfigureInstanceRequest{
			Machine: m, ClusterID: clusterID, BootstrapBlob: blob, OperationID: opID,
		}); err != nil {
			return nil, err
		}
		// ADR-0056: a machine must not be reported Configured until its node is
		// observed Ready on the target cluster. If the backend supplies a
		// readiness gate, run it under the remaining Configure timeout while the
		// machine is still Configuring; an error (or the timeout) sends the
		// machine to Failed via runTransition, so it never settles Configured on
		// a node that has not joined.
		if rc, ok := s.backend.(ReadinessChecker); ok {
			if err := rc.ConfirmNodeReady(ctx, ConfirmNodeReadyRequest{Machine: m, ClusterID: clusterID, OperationID: opID}); err != nil {
				return nil, fmt.Errorf("node readiness: %w", err)
			}
		}
		return nil, nil
	}
	return s.dispatch(kindConfigure, req.GetMachineId(), fenceOf(req.GetShardId(), req.GetShardEpoch(), req.GetSequenceNumber()), s.opts.Timeouts.Configure, pre, op)
}

// Drain implements pb.CapacityProviderServer: Configured → Draining → Idle.
// It clears the cluster binding and shard_metadata together when the drain
// completes — they are per-assignment state, not per-machine state.
func (s *Server) Drain(_ context.Context, req *pb.DrainRequest) (*pb.TransitionAck, error) {
	if req.GetMachineId() == "" {
		return nil, status.Error(codes.InvalidArgument, "machine_id required")
	}
	grace := req.GetGracePeriodSeconds()
	op := func(ctx context.Context, m Machine, opID string) (func(*Machine), error) {
		if err := s.backend.DrainInstance(ctx, DrainInstanceRequest{Machine: m, GracePeriodSeconds: grace, OperationID: opID}); err != nil {
			return nil, err
		}
		return func(dst *Machine) {
			dst.Cluster = ""
			dst.ShardMetadata = nil
		}, nil
	}
	return s.dispatch(kindDrain, req.GetMachineId(), fenceOf(req.GetShardId(), req.GetShardEpoch(), req.GetSequenceNumber()), s.opts.Timeouts.Drain, nil, op)
}

// Delete implements pb.CapacityProviderServer: Idle → Deleting →
// Speculative. A backend that does not implement [Deleter] (bare-metal free
// pool) makes this return codes.Unimplemented synchronously, before any
// state is touched.
func (s *Server) Delete(_ context.Context, req *pb.DeleteRequest) (*pb.TransitionAck, error) {
	if req.GetMachineId() == "" {
		return nil, status.Error(codes.InvalidArgument, "machine_id required")
	}
	if !s.canDelete {
		return nil, status.Error(codes.Unimplemented, "this provider does not support Delete (bare-metal free-pool semantics)")
	}
	op := func(ctx context.Context, m Machine, opID string) (func(*Machine), error) {
		if err := s.deleter.DeleteInstance(ctx, DeleteInstanceRequest{Machine: m, OperationID: opID}); err != nil {
			return nil, err
		}
		return func(dst *Machine) {
			dst.Host = HostRef{}
		}, nil
	}
	return s.dispatch(kindDelete, req.GetMachineId(), fenceOf(req.GetShardId(), req.GetShardEpoch(), req.GetSequenceNumber()), s.opts.Timeouts.Delete, nil, op)
}

// Get implements pb.CapacityProviderServer. Reads carry no fencing token.
func (s *Server) Get(_ context.Context, req *pb.MachineRef) (*pb.Machine, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.machines[req.GetId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "machine %q not found", req.GetId())
	}
	return machineToProto(m), nil
}

// List implements pb.CapacityProviderServer. Reads carry no fencing token. It
// honours the state filter, max_results cap, and since_revision cursor, and
// echoes the current revision so a caller can poll incrementally.
func (s *Server) List(_ context.Context, req *pb.ListFilter) (*pb.MachineList, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var since int64
	hasSince := false
	if b := req.GetSinceRevision(); len(b) > 0 {
		if v, err := strconv.ParseInt(string(b), 10, 64); err == nil {
			since, hasSince = v, true
		}
	}
	states := req.GetStates()
	maxResults := int(req.GetMaxResults())

	out := &pb.MachineList{Revision: []byte(strconv.FormatInt(s.rev, 10))}
	for id, m := range s.machines {
		if hasSince && s.lastMod[id] <= since {
			continue
		}
		if !stateMatches(m.State, states) {
			continue
		}
		out.Machines = append(out.Machines, machineToProto(m))
		if maxResults > 0 && len(out.Machines) >= maxResults {
			break
		}
	}
	return out, nil
}

func stateMatches(s State, want []pb.MachineState) bool {
	if len(want) == 0 {
		return true
	}
	for _, w := range want {
		if stateFromProto(w) == s {
			return true
		}
	}
	return false
}

// --- the shared lifecycle machinery --------------------------------------

// backendOp runs the substrate work for one transition and returns the
// post-effect to apply when (and only when) it succeeds. It runs on a
// background goroutine under a per-transition timeout. opID is the kit's
// idempotency key for this operation, handed to the backend so it can use it
// as a substrate idempotency token.
type backendOp func(ctx context.Context, m Machine, opID string) (apply func(*Machine), err error)

// dispatch is the shared body of all four mutating RPCs. It enforces the
// fence-then-idempotency-then-validate ordering, accepts the transition,
// records it durably, and kicks off the async backend work. It returns the
// TransitionAck immediately with the machine already in its transitional
// state. preEffect, when non-nil, runs under the lock the moment the machine
// enters the transitional state — used by Configure to make the cluster
// binding visible for the whole CONFIGURING span, not just on completion.
func (s *Server) dispatch(k kind, id string, f Fence, timeout time.Duration, preEffect func(*Machine), op backendOp) (*pb.TransitionAck, error) {
	s.mu.Lock()

	// 1. Fence FIRST — before not-found, before the idempotency
	//    short-circuit. A zombie must not be applied, must not get a cached
	//    operation_id, and must not learn whether the machine exists.
	advanced, err := s.checkFenceLocked(id, f)
	if err != nil {
		s.mu.Unlock()
		return nil, mapErr(err)
	}

	// 2. Not found.
	m, ok := s.machines[id]
	if !ok {
		s.persistIf(advanced)
		s.mu.Unlock()
		return nil, mapErr(fmt.Errorf("%w: %s", ErrNotFound, id))
	}

	transitional, stable := k.targets()

	// 3. Idempotent retry — same (machine, kind) while at-or-progressing-to
	//    the target returns the same operation_id without re-dispatching.
	if opID, exists := s.ops[opKey{id, k}]; exists && (m.State == transitional || m.State == stable) {
		ack := &pb.TransitionAck{OperationId: opID, Machine: machineToProto(m)}
		s.persistIf(advanced)
		s.mu.Unlock()
		return ack, nil
	}

	// 4. Position check — an out-of-position lifecycle call (Drain on
	//    Speculative, Delete on Configured) is rejected, never with
	//    FAILED_PRECONDITION (reserved for fencing).
	if !canStart(m.State, k) {
		s.persistIf(advanced)
		s.mu.Unlock()
		return nil, mapErr(fmt.Errorf("%w: cannot %s a machine in state %s", ErrInvalidTransition, k, m.State))
	}

	// 5. Accept: mint+record the op, enter the transitional state, persist.
	opID := s.mintOpLocked()
	s.ops[opKey{id, k}] = opID
	m.State = transitional
	if preEffect != nil {
		preEffect(m)
	}
	s.touchLocked(id)
	ack := &pb.TransitionAck{OperationId: opID, Machine: machineToProto(m)}
	snapshot := *m.clone()
	s.persistLocked()
	s.mu.Unlock()

	// 6. Do the substrate work in the background; progress is observed via
	//    Get/List.
	go s.runTransition(k, id, timeout, op, snapshot, opID)
	return ack, nil
}

// runTransition performs the async backend work for one accepted transition,
// racing it against the per-transition timeout. On success it advances the
// machine to its stable target and applies the post-effect; on error or
// timeout it moves the machine to Failed with last_error set.
func (s *Server) runTransition(k kind, id string, timeout time.Duration, op backendOp, snapshot Machine, opID string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	type result struct {
		apply func(*Machine)
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		apply, err := op(ctx, snapshot, opID)
		ch <- result{apply, err}
	}()

	var apply func(*Machine)
	var err error
	select {
	case r := <-ch:
		apply, err = r.apply, r.err
	case <-ctx.Done():
		// Timeout fired. A backend that respects ctx will also be returning
		// about now; either way the result is a Failed transition. The late
		// send (if any) lands in the buffered channel and is discarded.
		err = fmt.Errorf("%s transition timed out after %s: %w", k, timeout, ctx.Err())
	}

	transitional, stable := k.targets()
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.machines[id]
	if !ok || m.State != transitional {
		// The machine was deleted or moved on by some other path; the
		// completion is stale. Nothing to apply.
		return
	}
	if err != nil {
		m.State = StateFailed
		m.LastError = err.Error()
		s.logger.Warn("providerkit: transition failed", "machine", id, "kind", k.String(), "err", err)
	} else {
		m.State = stable
		if apply != nil {
			apply(m)
		}
	}
	s.touchLocked(id)
	s.persistLocked()
}

// checkFenceLocked enforces the paper §11 fencing contract and reports
// whether the high-water mark advanced (so the caller can persist it). A
// zero token bypasses fencing (unfenced in-process / test caller). The mark
// is kept per (shard_id, machine_id) — see fenceKey: a per-shard mark would
// fence a single live shard against its own concurrent execute pool, whose
// monotonic sequence numbers race the sends and arrive out of order across
// DIFFERENT machines. A token not strictly newer than THIS machine's mark
// for THIS shard is rejected with ErrFenced and the mark is left untouched.
// A token that passes advances the mark even though the operation may still
// fail downstream. A true zombie still fails on epoch (strictly lower).
func (s *Server) checkFenceLocked(machineID string, f Fence) (advanced bool, err error) {
	if f.zero() {
		return false, nil
	}
	key := fenceKey{ShardID: f.ShardID, MachineID: machineID}
	mark, known := s.fences[key]
	if known && !mark.newer(f) {
		return false, fmt.Errorf("%w: shard %q sent (epoch=%d seq=%d) for machine %q; high-water mark is (epoch=%d seq=%d)",
			ErrFenced, f.ShardID, f.ShardEpoch, f.SequenceNumber, machineID, mark.Epoch, mark.Sequence)
	}
	s.fences[key] = FenceMark{Epoch: f.ShardEpoch, Sequence: f.SequenceNumber}
	return true, nil
}

func (s *Server) touchLocked(id string) {
	s.rev++
	s.lastMod[id] = s.rev
}

func (s *Server) mintOpLocked() string {
	s.nextOp++
	return s.opts.OperationIDPrefix + "-" + strconv.FormatInt(s.nextOp, 10)
}

func (s *Server) persistIf(cond bool) {
	if cond {
		s.persistLocked()
	}
}

// persistLocked writes the full state through to the store. A failure is
// logged, not returned: the in-memory state has already advanced, and
// reporting failure to the caller would claim the op did not happen when it
// did. Operators should alert on this log line — a persistent Save failure
// re-opens the durability guarantees the store exists to provide.
func (s *Server) persistLocked() {
	if err := s.store.Save(s.snapshotLocked()); err != nil {
		s.logger.Error("providerkit: store save failed (durability at risk)", "err", err)
	}
}

// snapshotLocked builds a Snapshot referencing the live records. The store
// must treat it as read-only for the duration of Save (the built-in stores
// do: MemStore deep-copies, FileStore marshals immediately).
func (s *Server) snapshotLocked() Snapshot {
	snap := Snapshot{
		Rev:      s.rev,
		NextOp:   s.nextOp,
		Fences:   make([]FenceRecord, 0, len(s.fences)),
		Machines: make([]*Machine, 0, len(s.machines)),
		Ops:      make([]OpRecord, 0, len(s.ops)),
	}
	for _, m := range s.machines {
		snap.Machines = append(snap.Machines, m)
	}
	for key, mark := range s.fences {
		snap.Fences = append(snap.Fences, FenceRecord{
			ShardID:   key.ShardID,
			MachineID: key.MachineID,
			Epoch:     mark.Epoch,
			Sequence:  mark.Sequence,
		})
	}
	for k, opID := range s.ops {
		snap.Ops = append(snap.Ops, OpRecord{MachineID: k.id, Kind: k.k.String(), OperationID: opID})
	}
	return snap
}

func fenceOf(shardID string, epoch, seq int64) Fence {
	return Fence{ShardID: shardID, ShardEpoch: epoch, SequenceNumber: seq}
}

func kindFromString(s string) kind {
	switch s {
	case "configure":
		return kindConfigure
	case "drain":
		return kindDrain
	case "delete":
		return kindDelete
	default:
		return kindCreate
	}
}

// mapErr translates the kit's sentinel errors into gRPC status codes.
// FAILED_PRECONDITION is reserved for fencing; invalid transitions and
// everything else unmapped become Internal (matching pkg/provider/fake).
func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrFenced):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, ErrInvalidTransition):
		return status.Error(codes.Internal, err.Error())
	case errors.Is(err, ErrInvalidMachine):
		return status.Error(codes.Internal, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// Compile-time check that Server satisfies the generated server interface.
var _ pb.CapacityProviderServer = (*Server)(nil)
