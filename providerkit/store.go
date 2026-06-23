package providerkit

import "sync"

// Store is the durable home for the three pieces of state a provider must
// survive a restart with:
//
//   - the per-shard_id fence high-water marks — lose them and the
//     zombie-shard window re-opens until every live shard makes contact
//     again;
//   - the (machine_id, kind) → operation_id idempotency map — lose it and a
//     retried lifecycle call mints a fresh operation_id, breaking the
//     idempotency contract;
//   - the machine inventory, including the cluster binding and
//     shard_metadata — lose those and a restarted shard silently drops
//     preemption protection fleet-wide, because the provider's store is the
//     only durable copy of that assignment state.
//
// The kit treats the Store as authoritative on startup ([Server.New] calls
// Load) and writes the full state through on every mutation (Save). The
// snapshot model keeps the interface tiny — two methods plus Close — at the
// cost of an O(N) write per mutation, which is fine up to the conformance
// threshold of ~10k machines per shard. A provider that exposes far more
// should supply a delta-oriented Store backed by an embedded KV store
// (bbolt); the interface is small enough to reimplement.
type Store interface {
	// Load returns the persisted state, or an empty Snapshot on first boot.
	Load() (Snapshot, error)
	// Save durably persists the full state. It must be atomic: a crash
	// mid-Save must leave the prior snapshot intact, never a torn one.
	Save(Snapshot) error
	// Close releases any resources (file handles, db). Safe to call once.
	Close() error
}

// Snapshot is the complete persisted state of a provider. A Store must treat
// it as read-only for the duration of a Save call and copy anything it
// retains beyond that — the kit reuses the live records to avoid an extra
// O(N) copy per mutation.
type Snapshot struct {
	Machines []*Machine    `json:"machines"`
	Fences   []FenceRecord `json:"fences"`
	Ops      []OpRecord    `json:"ops"`
	// Rev is the monotonic revision counter List exposes as its opaque
	// cursor. Persisted so a provider restart does not reissue revisions a
	// shard already holds.
	Rev int64 `json:"rev"`
	// NextOp is the operation_id counter, persisted so post-restart ops
	// never collide with pre-restart ones.
	NextOp int64 `json:"next_op"`
}

// OpRecord is one idempotency entry: the operation_id minted for a given
// (machine_id, kind). Kind is the operation's string form (Create and Drain
// both target Idle, so the kind — not the target state — disambiguates).
type OpRecord struct {
	MachineID   string `json:"machine_id"`
	Kind        string `json:"kind"`
	OperationID string `json:"operation_id"`
}

// FenceRecord is one persisted fencing high-water mark, keyed by
// (shard_id, machine_id). The mark is per-(shard, machine), not per-shard:
// a shard's concurrent execute pool draws monotonic sequence numbers but
// races the sends, so a per-shard mark fences a single live shard against
// its own out-of-order arrivals on DIFFERENT machines. Per-machine keying
// stays monotonic (the shard serializes transitions per machine) while
// letting concurrent ops on different machines proceed. See the bigfleet
// fencing ADR.
type FenceRecord struct {
	ShardID   string `json:"shard_id"`
	MachineID string `json:"machine_id"`
	Epoch     int64  `json:"epoch"`
	Sequence  int64  `json:"sequence"`
}

func (s Snapshot) clone() Snapshot {
	out := Snapshot{
		Fences: make([]FenceRecord, len(s.Fences)),
		Ops:    make([]OpRecord, len(s.Ops)),
		Rev:    s.Rev,
		NextOp: s.NextOp,
	}
	out.Machines = make([]*Machine, 0, len(s.Machines))
	for _, m := range s.Machines {
		out.Machines = append(out.Machines, m.clone())
	}
	copy(out.Fences, s.Fences)
	copy(out.Ops, s.Ops)
	return out
}

// MemStore is an in-memory Store for tests and ephemeral providers. It does
// retain the last saved snapshot (rather than discarding it), so a new
// Server constructed from the same MemStore reloads the persisted fence
// marks, idempotency map and inventory — exactly the restart path real
// providers depend on, without touching disk.
type MemStore struct {
	mu   sync.Mutex
	snap Snapshot
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() *MemStore { return &MemStore{} }

func (m *MemStore) Load() (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snap.clone(), nil
}

func (m *MemStore) Save(s Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snap = s.clone()
	return nil
}

func (m *MemStore) Close() error { return nil }

// Compile-time check.
var _ Store = (*MemStore)(nil)
