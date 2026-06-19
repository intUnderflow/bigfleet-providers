//go:build certify

package suite

import (
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/intUnderflow/bigfleet-providers/conformance/internal/harness"
)

// C3 — fencing depth (behaviors B30x), beyond the upstream stale-epoch /
// stale-seq / new-epoch / unknown-shard / reads-unaffected baseline.

// Fence runs BEFORE the not-found check: a stale token aimed at a non-existent
// machine is rejected with FAILED_PRECONDITION, not NotFound — a zombie must
// not even learn whether a machine exists.
func TestFencing_BeforeNotFound(t *testing.T) {
	h := dial(t)
	shard := h.UniqueShardID("before-notfound")
	real := h.PickSpeculative()

	// Establish a high-water mark for this shard against a real machine.
	if err := h.FencedCreate(real, shard, 5, 5); err != nil {
		t.Fatalf("establish mark: %v", err)
	}
	// A STALE token for the same shard, aimed at a machine that does not exist.
	err := h.FencedCreate("conformance-ghost-fence", shard, 1, 1)
	if harness.Code(err) != codes.FailedPrecondition {
		t.Errorf("stale token on unknown machine: code %s, want FAILED_PRECONDITION (fence before not-found)", harness.Code(err))
	}
}

// Fencing marks are isolated per shard_id: one shard's high mark never fences
// another shard's first contact, and a shard's own stale token is still
// rejected.
func TestFencing_PerShardIsolation(t *testing.T) {
	h := dial(t)
	shardA := h.UniqueShardID("iso-a")
	shardB := h.UniqueShardID("iso-b")
	mA := h.PickSpeculative()
	mB := h.PickSpeculative()

	if err := h.FencedCreate(mA, shardA, 9, 9); err != nil {
		t.Fatalf("shardA establish: %v", err)
	}
	// shardB first contact with a LOW token is accepted — independent mark.
	if err := h.FencedCreate(mB, shardB, 1, 1); err != nil {
		t.Errorf("shardB first contact should be accepted despite shardA's high mark: %v", err)
	}
	// shardA's own stale token is still rejected.
	if err := h.FencedCreate(mA, shardA, 1, 1); harness.Code(err) != codes.FailedPrecondition {
		t.Errorf("shardA stale token: code %s, want FAILED_PRECONDITION", harness.Code(err))
	}
}

// Exhaustive lexicographic ordering of the (epoch, sequence) high-water mark on
// a single shard: every not-strictly-newer token is rejected; strictly-newer
// advances the mark; a new epoch resets the sequence space.
func TestFencing_LexicographicOrdering(t *testing.T) {
	h := dial(t)
	shard := h.UniqueShardID("lexico")
	m := h.PickSpeculative()

	// Establish the mark at (epoch=2, seq=5).
	if err := h.FencedCreate(m, shard, 2, 5); err != nil {
		t.Fatalf("establish (2,5): %v", err)
	}
	type step struct {
		epoch, seq int64
		accept     bool
		why        string
	}
	steps := []step{
		{2, 5, false, "replay equal token"},
		{2, 4, false, "lower sequence, same epoch"},
		{1, 9, false, "lower epoch dominates regardless of sequence"},
		{2, 6, true, "higher sequence, same epoch -> advances to (2,6)"},
		{2, 6, false, "replay of the just-accepted token"},
		{3, 1, true, "higher epoch with low sequence -> new epoch resets seq, advances to (3,1)"},
		{3, 1, false, "replay after the epoch bump"},
		{3, 0, false, "lower sequence within the new epoch"},
		{4, 0, true, "higher epoch again -> advances to (4,0)"},
	}
	for _, s := range steps {
		err := h.FencedCreate(m, shard, s.epoch, s.seq)
		if s.accept {
			if err != nil {
				t.Errorf("(%d,%d) %s: expected accept, got %v", s.epoch, s.seq, s.why, err)
			}
		} else if harness.Code(err) != codes.FailedPrecondition {
			t.Errorf("(%d,%d) %s: code %s, want FAILED_PRECONDITION", s.epoch, s.seq, s.why, harness.Code(err))
		}
	}
}

// Reads never fence, even right after a fenced-out mutation (deeper than the
// upstream check: interleave several fenced rejections and confirm Get/List
// keep working throughout).
func TestFencing_ReadsNeverFence(t *testing.T) {
	h := dial(t)
	shard := h.UniqueShardID("reads")
	m := h.PickSpeculative()
	if err := h.FencedCreate(m, shard, 7, 7); err != nil {
		t.Fatalf("establish: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := h.FencedCreate(m, shard, 1, 1); harness.Code(err) != codes.FailedPrecondition {
			t.Fatalf("stale token: %s", harness.Code(err))
		}
		if _, err := h.GetRaw(m); err != nil {
			t.Errorf("Get after fenced mutation #%d: %v", i, err)
		}
		if got := h.List(); len(got) == 0 {
			t.Errorf("List after fenced mutation #%d returned nothing", i)
		}
	}
}
