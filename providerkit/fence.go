package providerkit

// Fence is the shard's fencing token (BigFleet paper §11). It rides on
// every mutating RPC and lets a provider refuse a zombie shard — an old
// process, or a duplicate of the same shard identity — whose view of the
// fleet is stale.
//
// The epoch is persisted shard-side and increments on every shard restart;
// the sequence number is a per-process monotonic counter, freshly stamped
// on every call attempt (so a transport retry is never mistaken for a
// replay — idempotency is keyed on the operation, never on the token).
type Fence struct {
	ShardID        string
	ShardEpoch     int64
	SequenceNumber int64
}

// zero reports whether the token is entirely unset. A zero token is treated
// as "unfenced": an in-process or test caller that opts out of fencing
// (and exactly the shape the conformance suite's non-fencing tests send).
// A real shard always stamps a non-empty shard_id.
func (f Fence) zero() bool {
	return f.ShardID == "" && f.ShardEpoch == 0 && f.SequenceNumber == 0
}

// fenceKey identifies the scope of one high-water mark: a (shard_id,
// machine_id) pair, NOT a bare shard_id. A shard's concurrent execute pool
// draws monotonic sequence numbers but races the sends (stamp-then-send is
// not atomic, and a gRPC server dispatches each RPC on its own goroutine),
// so a per-shard mark fences a single live shard against its own
// out-of-order arrivals on DIFFERENT machines — a false zombie. The shard
// serializes transitions per machine (one in-flight op per machine), so a
// per-(shard, machine) mark stays monotonic for real traffic while letting
// concurrent ops on different machines proceed. A true zombie is still
// caught: it carries a strictly LOWER epoch, rejected per machine. See the
// bigfleet fencing ADR.
type fenceKey struct {
	ShardID   string
	MachineID string
}

// FenceMark is the highest (epoch, sequence_number) pair accepted so far
// for one (shard_id, machine_id), compared lexicographically.
type FenceMark struct {
	Epoch    int64
	Sequence int64
}

// newer reports whether the token is strictly newer than the mark,
// lexicographically: (e1,s1) is newer than (e2,s2) iff e1 > e2, or e1 == e2
// and s1 > s2. A new epoch resets the sequence space — any sequence number
// under a higher epoch is newer.
func (m FenceMark) newer(f Fence) bool {
	if f.ShardEpoch != m.Epoch {
		return f.ShardEpoch > m.Epoch
	}
	return f.SequenceNumber > m.Sequence
}
