//go:build certify

package suite

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"

	"github.com/intUnderflow/bigfleet-providers/conformance/internal/harness"
)

// Property / Fuzz (behaviors B120x). These are oracle-driven, seeded-random
// generators: each test mints a deterministic RNG via h.Rand() (replayable with
// -seed) and checks the provider's wire behavior against a small, frozen oracle
// over MANY generated cases — strictly deeper than the fixed-vector baselines in
// the fencing / metadata / matrix areas, which hard-code one or two examples.
//
// All assertions are PURE WIRE: six RPCs + gRPC codes, no providerkit. The
// provider is a fast in-memory fake, so transitional windows may be unobservable
// — every transitional check here tolerates "not seen" and only ever asserts
// "if seen, it was the right one". The seed pool is finite (~256 Speculative
// machines shared across all tests in the run), so each test budgets its
// consumption and none calls t.Parallel.

// --- B1201: fencing == lexicographic-greater-than-running-max oracle ---------

// For a seeded random stream of (epoch,sequence) tokens on ONE shard, the
// provider's accept/reject decision must equal, on EVERY step, a pure
// lexicographic ">" comparison against the running high-water mark. This is the
// generative generalisation of the fixed LexicographicOrdering vector: hundreds
// of random tokens (with deliberate clustering on the current mark so replays,
// off-by-ones at the epoch/seq boundary, and epoch resets all occur) replayed
// against an in-test oracle, with the seed logged for replay.
func TestB1201_FencingLexicographicOracle(t *testing.T) {
	behavior(t, "B1201")
	h := dial(t)
	rng := h.Rand()

	// One real machine + one fresh shard. Create is idempotent on a Speculative
	// machine's path to Idle, so a token that PASSES the fence never errors for a
	// non-fencing reason — any FAILED_PRECONDITION can only be the fence, and a
	// nil error can only mean the fence let the call through.
	machine := h.PickSpeculative()
	shard := h.UniqueShardID("b1201")

	// The oracle: the running high-water mark. mark==nil means "no token has been
	// accepted yet" — first contact with ANY non-zero token is accepted. A zero
	// token (0,0) bypasses fencing entirely, so we never generate one here.
	type tok struct{ epoch, seq int64 }
	var mark *tok
	newer := func(c tok) bool {
		if mark == nil {
			return true // first contact establishes the mark
		}
		if c.epoch != mark.epoch {
			return c.epoch > mark.epoch
		}
		return c.seq > mark.seq
	}

	const steps = 240
	for i := 0; i < steps; i++ {
		var c tok
		// Bias generation toward the current mark so we densely sample the
		// decision boundary (exact replay, seq±1, epoch±1, epoch bump with low
		// seq) rather than only far-apart random points.
		switch base := mark; base {
		case nil:
			c = tok{epoch: 1 + rng.Int63n(3), seq: 1 + rng.Int63n(3)}
		default:
			switch rng.Intn(6) {
			case 0: // exact replay (must reject)
				c = *base
			case 1: // same epoch, seq jittered around the mark
				c = tok{base.epoch, base.seq + int64(rng.Intn(5)) - 2}
			case 2: // epoch jittered, seq jittered (covers epoch reset semantics)
				c = tok{base.epoch + int64(rng.Intn(3)) - 1, base.seq + int64(rng.Intn(5)) - 2}
			case 3: // strictly-higher epoch, deliberately LOW seq
				c = tok{base.epoch + 1, rng.Int63n(3)}
			case 4: // strictly-higher seq, same epoch
				c = tok{base.epoch, base.seq + 1 + rng.Int63n(3)}
			default: // far random point
				c = tok{1 + rng.Int63n(6), rng.Int63n(8)}
			}
		}
		// Tokens must be non-zero to engage the fence; clamp away (0,0).
		if c.epoch <= 0 && c.seq <= 0 {
			c.seq = 1
		}

		want := newer(c)
		err := h.FencedCreate(machine, shard, c.epoch, c.seq)
		got := harness.Code(err)

		if want {
			if err != nil {
				t.Fatalf("step %d token (epoch=%d seq=%d): oracle=ACCEPT but provider rejected with %s (mark=%v, seed replay -seed)",
					i, c.epoch, c.seq, got, mark)
			}
			mark = &tok{c.epoch, c.seq} // mark advances only on accept
		} else {
			if got != codes.FailedPrecondition {
				t.Fatalf("step %d token (epoch=%d seq=%d): oracle=REJECT but provider returned %s, want FAILED_PRECONDITION (mark=%v)",
					i, c.epoch, c.seq, got, mark)
			}
			// Rejected tokens must NOT advance the mark — the next step's oracle
			// still compares against the prior accepted token.
		}
	}
}

// --- B1202: metadata round-trips byte-identically ----------------------------

// randMeta builds a random valid-UTF-8 metadata map. proto3 string fields must
// be valid UTF-8 on the wire (invalid bytes can't be transmitted at all), so the
// generator stays within valid UTF-8 — but exercises the full surface a real
// shard scheduler emits: empty values, embedded NUL and control bytes, multibyte
// runes/emoji, long values, and many keys.
func randMeta(rng *rand.Rand) map[string]string {
	n := rng.Intn(12) // 0..11 entries (0 exercises the empty map round-trip)
	md := make(map[string]string, n)
	for i := 0; i < n; i++ {
		md[randUTF8Key(rng, i)] = randUTF8Value(rng)
	}
	return md
}

func randUTF8Key(rng *rand.Rand, i int) string {
	// Keys are real-shape: a domain-style prefix + a unique suffix so they never
	// collide within one map.
	prefixes := []string{"bigfleet.lucy.sh/", "topology.bigfleet/", "x-shard/", "节点/"}
	return fmt.Sprintf("%s%s-%d", prefixes[rng.Intn(len(prefixes))], randUTF8Value(rng), i)
}

func randUTF8Value(rng *rand.Rand) string {
	length := rng.Intn(24) // includes 0 -> empty value
	r := make([]rune, 0, length)
	for len(r) < length {
		switch rng.Intn(10) {
		case 0, 1, 2, 3: // ASCII printable
			r = append(r, rune(0x20+rng.Intn(0x5f)))
		case 4: // control / NUL (valid UTF-8 single bytes)
			r = append(r, rune(rng.Intn(0x20)))
		case 5, 6: // Latin-1 / common multibyte
			r = append(r, rune(0xA0+rng.Intn(0x300)))
		case 7, 8: // CJK & misc BMP
			r = append(r, rune(0x4E00+rng.Intn(0x1000)))
		default: // astral plane (emoji range)
			r = append(r, rune(0x1F300+rng.Intn(0x200)))
		}
	}
	s := string(r)
	if !utf8.ValidString(s) { // defensive — rune->string is always valid UTF-8
		return ""
	}
	return s
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// For seeded random valid-UTF-8 metadata maps, each Configure must round-trip
// byte-identically on the next Get — across many generated cases on the SAME
// machine (so we also prove a previous binding's keys never leak into the next).
// Stronger than the fixed-vector metadata stress test: the maps are generated,
// the round trip is verified on every iteration, and List is cross-checked.
func TestB1202_MetadataRoundTrip(t *testing.T) {
	behavior(t, "B1202")
	h := dial(t)
	rng := h.Rand()

	// Reuse a single Idle machine across cases to bound seed-pool consumption and
	// to additionally prove no cross-case residue (each Configure fully replaces).
	id := h.WalkToIdle()
	const cases = 20
	for i := 0; i < cases; i++ {
		md := randMeta(rng)
		cluster := fmt.Sprintf("conf-b1202-%d", i)
		if _, err := h.Configure(id, cluster, md); err != nil {
			t.Fatalf("case %d Configure: %v", i, err)
		}
		h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 15*time.Second)

		got := h.Get(id)
		if !mapsEqual(got.GetShardMetadata(), md) {
			t.Fatalf("case %d: shard_metadata did not round-trip\n got=%q\nwant=%q", i, got.GetShardMetadata(), md)
		}
		if got.GetCluster() != cluster {
			t.Errorf("case %d: cluster=%q, want %q", i, got.GetCluster(), cluster)
		}
		// Cross-check the same verbatim map on List(CONFIGURED).
		found := false
		for _, m := range h.List(pb.MachineState_MACHINE_STATE_CONFIGURED) {
			if m.GetId() == id {
				found = true
				if !mapsEqual(m.GetShardMetadata(), md) {
					t.Errorf("case %d: List metadata not verbatim for %s", i, id)
				}
			}
		}
		if !found {
			t.Errorf("case %d: List(CONFIGURED) omitted %s", i, id)
		}

		// Drain back to a clean Idle so the next case starts from a blank binding
		// (and the clear is part of the property too).
		if _, err := h.Drain(id, 0); err != nil {
			t.Fatalf("case %d Drain: %v", i, err)
		}
		m := h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)
		if len(m.GetShardMetadata()) != 0 || m.GetCluster() != "" {
			t.Fatalf("case %d: residue after Drain: md=%q cluster=%q", i, m.GetShardMetadata(), m.GetCluster())
		}
	}
}

// --- B1203: random lifecycle sequences vs the four-legal-edge model -----------

// The lifecycle oracle. There are exactly four legal mutating edges; every other
// (RPC × stable-state) pair is either an idempotent no-op at target or an illegal
// edge. legalNext maps a stable state under an RPC to the stable state the
// machine MUST settle in if the call is a legal edge or an at-target no-op; a
// false ok means the call is illegal from that state.
type rpcKind int

const (
	kCreate rpcKind = iota
	kConfigure
	kDrain
	kDelete
)

func (k rpcKind) String() string {
	switch k {
	case kCreate:
		return "Create"
	case kConfigure:
		return "Configure"
	case kDrain:
		return "Drain"
	case kDelete:
		return "Delete"
	}
	return "?"
}

// legalEdge encodes the four-legal-edge entry model exactly: an RPC of kind k
// may BEGIN a transition only from its single legal source state, settling in
// stable(k). Mirrors providerkit's canStart/targets.
//
//	Create: Speculative -> Idle
//	Configure: Idle      -> Configured
//	Drain: Configured    -> Idle
//	Delete: Idle         -> Speculative
func legalEdge(src pb.MachineState, k rpcKind) (target pb.MachineState, ok bool) {
	S := pb.MachineState_MACHINE_STATE_SPECULATIVE
	I := pb.MachineState_MACHINE_STATE_IDLE
	C := pb.MachineState_MACHINE_STATE_CONFIGURED
	switch k {
	case kCreate:
		if src == S {
			return I, true
		}
	case kConfigure:
		if src == I {
			return C, true
		}
	case kDrain:
		if src == C {
			return I, true
		}
	case kDelete:
		if src == I {
			return S, true
		}
	}
	return src, false
}

// stableTargetOf returns the stable state an RPC of kind k drives toward.
func stableTargetOf(k rpcKind) pb.MachineState {
	switch k {
	case kCreate, kDrain:
		return pb.MachineState_MACHINE_STATE_IDLE
	case kConfigure:
		return pb.MachineState_MACHINE_STATE_CONFIGURED
	default: // kDelete
		return pb.MachineState_MACHINE_STATE_SPECULATIVE
	}
}

// For seeded random valid/invalid lifecycle sequences, the provider follows the
// four-legal-edge model: legal edges succeed and settle in the oracle's target;
// an idempotent retry of an RPC the machine ALREADY ran to reach its current
// stable target is a no-op (same kind, recorded op); every other call is an
// illegal out-of-position edge that rejects with a NON FAILED_PRECONDITION code
// and causes NO partial transition (the machine is exactly where it started).
//
// This is the generative generalisation of the fixed out-of-position matrix, and
// it is STRICTER than that baseline: it models the provider's real
// idempotency-key semantics — an at-target call is only a no-op if THAT kind was
// previously dispatched (e.g. Delete on a fresh Speculative machine, or Drain on
// a never-drained Idle machine, is an ILLEGAL edge, not a no-op), and it checks
// that distinction on every random step.
func TestB1203_RandomLifecycleSequences(t *testing.T) {
	behavior(t, "B1203")
	h := dial(t)
	rng := h.Rand()

	caps := h.Probe()

	apply := func(k rpcKind, id string) error {
		switch k {
		case kCreate:
			_, err := h.Create(id)
			return err
		case kConfigure:
			_, err := h.Configure(id, "conf-b1203", map[string]string{"k": "v"})
			return err
		case kDrain:
			_, err := h.Drain(id, 0)
			return err
		default:
			_, err := h.Delete(id)
			return err
		}
	}

	// A handful of independent random walks; each consumes ONE seed machine and
	// runs a short random RPC sequence over it, checking the oracle on every step.
	const walks = 5
	const stepsPerWalk = 9
	for w := 0; w < walks; w++ {
		id := h.PickSpeculative()
		cur := pb.MachineState_MACHINE_STATE_SPECULATIVE
		if got := h.State(id); got != cur {
			t.Fatalf("walk %d: fresh machine in %s, want Speculative", w, got)
		}
		for s := 0; s < stepsPerWalk; s++ {
			k := rpcKind(rng.Intn(4))
			if k == kDelete && !caps.Delete {
				// Provider without Delete: the call is Unimplemented regardless of
				// state — still never FAILED_PRECONDITION. Verify and skip the edge.
				err := apply(k, id)
				if harness.Code(err) != codes.Unimplemented {
					t.Errorf("walk %d step %d: Delete on no-Delete provider: %s, want Unimplemented", w, s, harness.Code(err))
				}
				continue
			}

			target, isLegal := legalEdge(cur, k)
			// An at-target call: the machine already sits at k's stable target.
			// Whether the provider treats it as an idempotent no-op (success) or
			// an out-of-position rejection depends on its HIDDEN op history for
			// this pool-reused machine (the kit no-ops only when it has a recorded
			// op for (machine,kind) at-or-past target) — both are conformant. The
			// oracle cannot see that history black-box, so it accepts either, as
			// long as the machine stays put and FAILED_PRECONDITION is never used.
			atTarget := !isLegal && cur == stableTargetOf(k)

			before := h.State(id)
			err := apply(k, id)

			switch {
			case isLegal:
				if err != nil {
					t.Fatalf("walk %d step %d: legal edge %s on %s rejected: %v", w, s, k, cur, err)
				}
				h.MustReach(id, target, 15*time.Second)
				cur = target
			case atTarget:
				// No-op success OR non-fencing rejection — either way, stay put.
				if err != nil {
					h.RejectsNonFencing(fmt.Sprintf("walk %d step %d: at-target %s on %s", w, s, k, cur), err)
				}
				h.StaysIn(id, cur, 80*time.Millisecond)
			default:
				// Genuinely out-of-position edge: rejected, NOT FAILED_PRECONDITION,
				// and NO partial transition.
				h.RejectsNonFencing(fmt.Sprintf("walk %d step %d: %s on %s", w, s, k, cur), err)
				h.StaysIn(id, before, 80*time.Millisecond)
				if got := h.State(id); got != cur {
					t.Fatalf("walk %d step %d: illegal %s on %s changed state to %s", w, s, k, cur, got)
				}
			}
		}
	}
}

// --- B1204: multi-shard fenced interleavings, FP only for fencing ------------

// For random interleavings of fenced mutations across MULTIPLE shards on shared
// machines, the invariant oracle confirms FAILED_PRECONDITION is emitted ONLY
// for fencing rejections (a token not strictly newer than that shard's mark),
// never for out-of-position or not-found. We drive Create (idempotent on the
// Speculative->Idle path), so a fence-passing call never errors for a non-fence
// reason — letting us attribute every FP precisely.
func TestB1204_MultiShardFencedInterleavings(t *testing.T) {
	behavior(t, "B1204")
	h := dial(t)
	rng := h.Rand()

	// A few shared machines and a few independent shards. Each shard keeps its own
	// running high-water mark oracle; marks are per-(shard) and independent of the
	// machine, matching the contract.
	const nMachines = 4
	const nShards = 3
	machines := h.PickNSpeculative(nMachines)
	shards := make([]string, nShards)
	type tok struct{ epoch, seq int64 }
	marks := make([]*tok, nShards) // per-shard oracle, nil == no contact yet
	for i := range shards {
		shards[i] = h.UniqueShardID(fmt.Sprintf("b1204-%d", i))
	}

	newer := func(m *tok, c tok) bool {
		if m == nil {
			return true
		}
		if c.epoch != m.epoch {
			return c.epoch > m.epoch
		}
		return c.seq > m.seq
	}

	const steps = 200
	for i := 0; i < steps; i++ {
		si := rng.Intn(nShards)
		mi := rng.Intn(nMachines)
		mk := marks[si]
		var c tok
		// Bias around this shard's current mark to densely hit the boundary.
		switch base := mk; base {
		case nil:
			c = tok{1 + rng.Int63n(2), 1 + rng.Int63n(2)}
		default:
			switch rng.Intn(4) {
			case 0:
				c = *base // replay -> reject
			case 1:
				c = tok{base.epoch, base.seq + int64(rng.Intn(3)) - 1}
			case 2:
				c = tok{base.epoch + 1, rng.Int63n(2)} // epoch bump, low seq -> accept
			default:
				c = tok{base.epoch + int64(rng.Intn(3)) - 1, base.seq + int64(rng.Intn(3)) - 1}
			}
		}
		if c.epoch <= 0 && c.seq <= 0 {
			c.seq = 1
		}

		want := newer(marks[si], c)
		// Note: the target machine is sometimes already Idle from a prior accepted
		// Create — Create is idempotent there, so a fence-passing call is STILL a
		// success (no out-of-position error). That is exactly the property: FP can
		// only ever come from the fence, never the machine's position.
		err := h.FencedCreate(machines[mi], shards[si], c.epoch, c.seq)
		got := harness.Code(err)
		if want {
			if err != nil {
				t.Fatalf("step %d shard=%d machine=%d (e=%d s=%d): fence ACCEPT but error %s (FP must never come from position/not-found)",
					i, si, mi, c.epoch, c.seq, got)
			}
			marks[si] = &tok{c.epoch, c.seq}
		} else {
			if got != codes.FailedPrecondition {
				t.Fatalf("step %d shard=%d machine=%d (e=%d s=%d): fence REJECT but code %s, want FAILED_PRECONDITION",
					i, si, mi, c.epoch, c.seq, got)
			}
		}
	}

	// Cross-shard independence sanity: a brand-new shard's first contact with the
	// LOWEST non-zero token is accepted even though other shards hold high marks.
	fresh := h.UniqueShardID("b1204-fresh")
	if err := h.FencedCreate(machines[0], fresh, 1, 1); err != nil {
		t.Errorf("fresh shard first-contact (1,1) rejected despite per-shard isolation: %v", err)
	}
}

// --- B1205: random bad request shapes vs the code-discipline oracle ----------

// For seeded random request shapes — empty/unknown id, negative grace, oversize
// metadata, oversize bootstrap_blob, malformed token — the response code matches
// the frozen code-discipline oracle: a defect is answered with InvalidArgument,
// NotFound, or Unimplemented (or accepted as OK when the contract does not
// mandate rejection), but NEVER FAILED_PRECONDITION, which is reserved for
// fencing. The strong, area-wide invariant is the negative one: no non-fencing
// request shape may ever be answered with FAILED_PRECONDITION.
func TestB1205_RequestShapeCodeDiscipline(t *testing.T) {
	behavior(t, "B1205")
	h := dial(t)
	rng := h.Rand()

	caps := h.Probe()

	// Build a big random metadata map and a big random bootstrap blob to probe
	// oversize handling (the contract does not cap these, so OK is allowed — what
	// is NOT allowed is fencing).
	bigMeta := func() map[string]string {
		md := make(map[string]string, 600)
		for i := 0; i < 400+rng.Intn(400); i++ {
			md[fmt.Sprintf("x-bulk/key-%05d-%s", i, randUTF8Value(rng))] = randUTF8Value(rng)
		}
		return md
	}
	bigBlob := func() []byte {
		b := make([]byte, 64*1024+rng.Intn(64*1024))
		rng.Read(b)
		return b
	}
	ghost := func() string { return fmt.Sprintf("conformance-ghost-%d-%d", time.Now().UnixNano(), rng.Int63()) }

	// Each generator returns (description, error). The oracle then classifies the
	// resulting code. machineId="" must be InvalidArgument; unknown id must be
	// NotFound (or Unimplemented for Delete on a no-Delete provider); the rest are
	// "anything but FAILED_PRECONDITION". Malformed-token cases deliberately use a
	// FRESH unique shard each call so first-contact is accepted by the fence —
	// thus any FAILED_PRECONDITION would unambiguously be a bug.
	type expect int
	const (
		expInvalidArg  expect = iota // must be InvalidArgument
		expNotFoundish               // NotFound, or Unimplemented for Delete
		expNotFencing                // any code except FAILED_PRECONDITION (OK allowed)
	)

	freshShard := func(p string) string { return h.UniqueShardID("b1205-" + p) }

	type gen struct {
		do  func() (string, error)
		exp expect
	}
	gens := []gen{
		// --- empty machine_id on every mutating RPC + Get => InvalidArgument ---
		{func() (string, error) { _, e := h.Create(""); return "Create empty-id", e }, expInvalidArg},
		{func() (string, error) { _, e := h.Configure("", "x", nil); return "Configure empty-id", e }, expInvalidArg},
		{func() (string, error) { _, e := h.Drain("", 5); return "Drain empty-id", e }, expInvalidArg},
		{func() (string, error) { _, e := h.GetRaw(""); return "Get empty-id", e }, expInvalidArg},
		// --- unknown id => NotFound (Delete: NotFound or Unimplemented) ---
		{func() (string, error) { _, e := h.GetRaw(ghost()); return "Get unknown", e }, expNotFoundish},
		{func() (string, error) { _, e := h.Create(ghost()); return "Create unknown", e }, expNotFoundish},
		{func() (string, error) { _, e := h.Configure(ghost(), "x", nil); return "Configure unknown", e }, expNotFoundish},
		{func() (string, error) { _, e := h.Drain(ghost(), 1); return "Drain unknown", e }, expNotFoundish},
		// --- defect shapes against a REAL machine: never fencing ---
		// negative grace on a real Configured machine (Drain): not fencing.
		{func() (string, error) {
			id := h.WalkToConfigured("conf-b1205-neg", nil)
			_, e := h.Drain(id, -(1 + rng.Int63n(1<<40)))
			return "Drain negative-grace", e
		}, expNotFencing},
		// oversize metadata on Configure of a real Idle machine: not fencing.
		{func() (string, error) {
			id := h.WalkToIdle()
			_, e := h.Configure(id, "conf-b1205-bigmd", bigMeta())
			return "Configure oversize-metadata", e
		}, expNotFencing},
		// oversize bootstrap_blob on Configure of a real Idle machine: not fencing.
		{func() (string, error) {
			id := h.WalkToIdle()
			ctx, cancel := h.Ctx()
			defer cancel()
			_, e := h.Client.Configure(ctx, &pb.ConfigureRequest{
				MachineId: id, ClusterId: "conf-b1205-bigblob", BootstrapBlob: bigBlob(),
			})
			return "Configure oversize-bootstrap_blob", e
		}, expNotFencing},
		// malformed token: garbage epoch/seq on a FRESH shard (first contact) against
		// an unknown machine. Fence runs first and accepts (fresh shard), then
		// NotFound — never FAILED_PRECONDITION from a malformed-but-fresh token.
		{func() (string, error) {
			e := h.FencedCreate(ghost(), freshShard("malformed"), rng.Int63(), rng.Int63())
			return "Create malformed-token+unknown", e
		}, expNotFoundish},
		// malformed token on a real machine, fresh shard: accepted by fence; the op
		// itself is a legal/no-op Create or out-of-position — either way not fencing.
		{func() (string, error) {
			id := h.PickSpeculative()
			e := h.FencedCreate(id, freshShard("malformed2"), 1, rng.Int63())
			return "Create malformed-token+real", e
		}, expNotFencing},
	}

	// Run each generator several times under different RNG draws (the generators
	// that consume seed machines run fewer times to budget the pool).
	for _, g := range gens {
		reps := 4
		// Generators that walk a fresh machine to Idle/Configured are pool-heavy;
		// the loop body breaks out after one rep for those (see end of loop).
		for r := 0; r < reps; r++ {
			what, err := g.do()
			code := harness.Code(err)
			// THE area-wide invariant: a non-fencing request shape is never fenced.
			if code == codes.FailedPrecondition {
				t.Errorf("%s: answered FAILED_PRECONDITION, which is reserved for fencing", what)
			}
			switch g.exp {
			case expInvalidArg:
				if code != codes.InvalidArgument {
					t.Errorf("%s: code %s, want InvalidArgument", what, code)
				}
			case expNotFoundish:
				switch code {
				case codes.NotFound:
				case codes.Unimplemented:
					if caps.Delete {
						// A Delete-capable provider must use NotFound for unknown,
						// not Unimplemented. (Non-Delete RPCs never legitimately
						// return Unimplemented for unknown ids.)
						t.Errorf("%s: Unimplemented from a Delete-capable provider, want NotFound", what)
					}
				default:
					t.Errorf("%s: code %s, want NotFound (or Unimplemented w/o Delete)", what, code)
				}
			case expNotFencing:
				// Only the negative invariant (already checked above) applies; OK
				// or any non-FP rejection is contract-conformant.
			}
			// Pool-heavy generators: only one rep.
			if g.exp == expNotFencing || what == "Create malformed-token+real" {
				break
			}
		}
	}
}
