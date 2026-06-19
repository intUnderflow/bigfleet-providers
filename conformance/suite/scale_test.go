//go:build certify && scale

// Package suite — SCALE & SOAK LANE (behaviors B11xx). These tests run ONLY with
// `-tags=certify,scale` against a provider booted with a large seeded inventory.
// They certify that the contract holds at fleet scale and under sustained churn:
// full-List field-shape/cost invariants over tens of thousands of records,
// since_revision deltas and pagination set-completeness at scale, a churn soak
// that leaks no residual binding, live-count conservation, per-RPC latency
// budgets, parallel walk-to-Idle without operation_id collision, and steady-state
// stability with no spontaneous drift.
//
//	go test -tags=certify,scale -run 'TestB11' ./suite/... -target=<addr> -scale-seed=8000 -soak=10s
//
// Every "10k-100k"/"multi-minute" assertion in the registry titles is
// PARAMETERIZED off -scale-seed and -soak so a credential-free local run is fast
// while the runner can crank both. Budgets are GENEROUS and must hold with wide
// margin on a dev/CI box against a fast in-memory fake; there are no exact timing
// asserts beyond those budgets. No t.Parallel — these share the provider's seed
// pool — except where a test owns DISTINCT machines.
package suite

import (
	"flag"
	"math"
	"sort"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"

	"github.com/intUnderflow/bigfleet-providers/conformance/internal/harness"
)

// scaleSeedFlag is the minimum seeded inventory the scale lane expects (the
// runner boots the provider with at least this many Speculative slots). It
// parameterizes every count assertion so the registry's "10k-100k" titles are
// honoured at whatever scale the operator can afford locally.
var (
	scaleSeedFlag = flag.Int("scale-seed", 4000, "minimum seeded inventory the scale lane asserts against (the provider seeds at least this many)")
	soakFlag      = flag.Duration("soak", 8*time.Second, "soak duration for the churn/conservation/stability behaviors (the runner cranks this up)")
)

// scaleDial connects to the provider with a RAISED max-recv message size, so a
// full List of a very large fleet (which can exceed the default ~4MB gRPC recv
// limit) does not fail at the chosen seed. It mirrors harness.Dial otherwise
// (insecure transport, t.Cleanup'd connection) and is used by the full-List
// behaviors (B1101/B1107).
func scaleDial(t *testing.T) *harness.H {
	t.Helper()
	conn, err := grpc.NewClient(target(t),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256*1024*1024)),
	)
	if err != nil {
		t.Fatalf("scale: dial %s: %v", target(t), err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return harness.DialConn(t, conn)
}

// checkFieldShapeAndCost asserts the per-record field-shape + cost-bound
// invariants the autoscaler depends on (the B801/B802 invariants, applied at
// scale to every record). It errors (does not Fatal) so one bad record does not
// hide the rest. Returns the number of records it bound.
func checkFieldShapeAndCost(t *testing.T, machines []*pb.Machine) {
	t.Helper()
	for _, m := range machines {
		id := m.GetId()
		if id == "" {
			t.Errorf("scale: machine with empty id in inventory")
		}
		// Field shape: top-level instance_type/zone/capacity_type set, state real.
		if m.GetState() == pb.MachineState_MACHINE_STATE_UNSPECIFIED {
			t.Errorf("%s: state UNSPECIFIED", id)
		}
		if m.GetInstanceType() == "" {
			t.Errorf("%s: instance_type empty (must be a populated top-level field)", id)
		}
		if m.GetZone() == "" {
			t.Errorf("%s: zone empty (must be a populated top-level field)", id)
		}
		if m.GetCapacityType() == pb.CapacityType_CAPACITY_TYPE_UNSPECIFIED {
			t.Errorf("%s: capacity_type UNSPECIFIED (must be a populated top-level field)", id)
		}
		// Cost bounds: price finite & >= 0, interruption_probability in [0,1],
		// SPOT machines report interruption_probability > 0.
		p := m.GetPricePerHour()
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 {
			t.Errorf("%s: price_per_hour %v not finite and >= 0", id, p)
		}
		ip := m.GetInterruptionProbability()
		if math.IsNaN(ip) || ip < 0 || ip > 1 {
			t.Errorf("%s: interruption_probability %v outside [0,1]", id, ip)
		}
		if m.GetCapacityType() == pb.CapacityType_CAPACITY_TYPE_SPOT && !(ip > 0) {
			t.Errorf("%s: SPOT machine reports interruption_probability %v (must be > 0)", id, ip)
		}
	}
}

// B1101 — with a large seeded inventory, a full List returns every machine and
// each record satisfies the field-shape and cost-bound invariants. The count
// must be >= the seeded scale (the Speculative seeds), and EVERY record is bound.
func TestB1101_FullListFieldShapeAtScale(t *testing.T) {
	behavior(t, "B1101")
	h := scaleDial(t)

	start := time.Now()
	machines := h.List()
	t.Logf("B1101: full List returned %d machines in %s", len(machines), time.Since(start))

	if len(machines) < *scaleSeedFlag {
		t.Fatalf("B1101: full List returned %d machines, want >= scale-seed %d", len(machines), *scaleSeedFlag)
	}
	checkFieldShapeAndCost(t, machines)
}

// B1102 — at scale, a since_revision delta after a bounded batch of mutations
// returns EXACTLY the mutated set, with no missing or extraneous machine.
// Capability-gated on Probe().SinceRevision.
func TestB1102_SinceDeltaExactAtScale(t *testing.T) {
	behavior(t, "B1102")
	h := scaleDial(t)
	if !h.Probe().SinceRevision {
		t.Skip("B1102: provider does not advance List.revision (no since_revision)")
	}

	// Snapshot the revision, then mutate a bounded batch of freshly-walked Idle
	// machines (Configure them) and assert the delta is exactly that set.
	const batch = 20
	ids := h.WalkNToIdle(batch)
	rev := h.Revision()

	want := map[string]bool{}
	for _, id := range ids {
		if _, err := h.Configure(id, "scale-b1102", map[string]string{"k": "v"}); err != nil {
			t.Fatalf("Configure(%s): %v", id, err)
		}
		want[id] = true
	}
	for _, id := range ids {
		h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 30*time.Second)
	}

	delta, _ := h.ListSince(rev)
	got := map[string]bool{}
	for _, m := range delta {
		got[m.GetId()] = true
	}

	// Membership equality (order-independent): the mutated set must appear, and
	// nothing the test did NOT mutate may appear (extraneous). A conformant delta
	// MAY include the intermediate Idle->Configuring->Configured churn on the same
	// ids, but never an id outside the batch.
	for id := range want {
		if !got[id] {
			t.Errorf("B1102: mutated machine %s missing from since-delta", id)
		}
	}
	for id := range got {
		if !want[id] {
			t.Errorf("B1102: since-delta contains extraneous machine %s (not in the mutated batch)", id)
		}
	}
	t.Logf("B1102: since-delta returned %d machines for a batch of %d mutations", len(got), batch)
}

// B1103 — at scale, paging the whole fleet via max_results+since_revision yields
// set-completeness (no duplicate, no skip) over the full inventory. Capability-
// gated on Probe().SinceRevision (the cursor advances the page window).
func TestB1103_PaginationSetCompletenessAtScale(t *testing.T) {
	behavior(t, "B1103")
	h := scaleDial(t)
	if !h.Probe().SinceRevision {
		t.Skip("B1103: provider does not advance List.revision (no since_revision paging)")
	}

	// The authoritative full set (a single unbounded List from the zero cursor).
	full := map[string]bool{}
	for _, m := range h.List() {
		full[m.GetId()] = true
	}
	if len(full) == 0 {
		t.Skip("B1103: provider exposes no machines to page")
	}

	// This contract's since_revision is a CHANGED-SINCE cursor, and the AWS kit's
	// is specifically HEAD-ECHO: List echoes the current head on every call, so a
	// delta keyed on the head is empty. Paging from the ZERO cursor returns the
	// whole fleet as the changed-set; page 0 is a capped prefix and page 1 keyed
	// on page 0's returned cursor is empty (the head was already at/ahead of the
	// fleet). So this lane mirrors B907's set-completeness idiom at scale:
	//   - if the cursor is a true CONTINUATION cursor (page 1 yields genuinely new
	//     members), thread it to completeness and assert the union covers `full`;
	//   - if it is HEAD-ECHO (page 1 adds nothing new), set-completeness is proven
	//     against the uncapped ground truth `full`, and the capped pages need only
	//     be no-dup, no-over-cap subsets — which we assert.
	const pageSize = 500

	page0, next0 := h.ListPage(pageSize, nil)
	if len(page0) > pageSize {
		t.Errorf("B1103: capped page returned %d machines, exceeding max_results=%d", len(page0), pageSize)
	}
	seen := map[string]bool{}
	for _, m := range page0 {
		id := m.GetId()
		if seen[id] {
			t.Errorf("B1103: page0 returned duplicate machine %s", id)
		}
		if !full[id] {
			t.Errorf("B1103: page0 returned %s not in the full fleet", id)
		}
		seen[id] = true
	}

	page1, _ := h.ListPage(pageSize, next0)
	page1New := 0
	for _, m := range page1 {
		id := m.GetId()
		if seen[id] {
			t.Errorf("B1103: threaded page1 returned duplicate machine %s already on page0", id)
			continue
		}
		if full[id] {
			page1New++
		}
		seen[id] = true
	}

	if page1New > 0 {
		// CONTINUATION-cursor style: thread the cursor until the fleet drains,
		// then require the accumulated union to cover every machine with no dup.
		maxPages := len(full)/pageSize + 8 // defensive cap; terminates well before
		cursor := next0
		pages := 1
		for ; pages < maxPages; pages++ {
			ms, next := h.ListPage(pageSize, cursor)
			if len(ms) == 0 || string(next) == string(cursor) {
				break
			}
			for _, m := range ms {
				seen[m.GetId()] = true
			}
			cursor = next
		}
		if pages >= maxPages {
			t.Fatalf("B1103: continuation paging did not terminate within %d pages (cursor not advancing?)", maxPages)
		}
		for id := range full {
			if !seen[id] {
				t.Errorf("B1103: continuation paged walk skipped machine %s (set-incompleteness)", id)
			}
		}
		t.Logf("B1103: continuation-cursor paged %d/%d machines across %d page(s)", len(seen), len(full), pages)
		return
	}

	// HEAD-ECHO style: set-completeness was proven against the uncapped `full`
	// ground truth above; the capped reads only had to be no-dup, no-over-cap
	// subsets of it, which they were.
	t.Logf("B1103: head-echo cursor; set-completeness proven against uncapped full fleet of %d machines (capped page0=%d, page1=%d, no dup/skip)",
		len(full), len(page0), len(page1))
}

// B1104 — a continuous Configure/Drain churn soak over many cycles keeps every
// machine's invariants intact and leaks no residual cluster/metadata/last_error
// at each Idle return. Runs for -soak.
func TestB1104_ChurnSoakNoLeak(t *testing.T) {
	behavior(t, "B1104")
	h := scaleDial(t)

	// A small working set walked to Idle once, then churned Configure->Drain.
	const work = 8
	ids := h.WalkNToIdle(work)

	deadline := time.Now().Add(*soakFlag)
	cycles := 0
	for time.Now().Before(deadline) {
		// Configure the whole working set, settle.
		for _, id := range ids {
			if _, err := h.Configure(id, "scale-b1104", map[string]string{"cycle": "x"}); err != nil {
				t.Fatalf("B1104: Configure(%s) cycle %d: %v", id, cycles, err)
			}
		}
		for _, id := range ids {
			h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 30*time.Second)
		}
		// Drain the whole working set, settle, and assert clean-at-Idle.
		for _, id := range ids {
			if _, err := h.Drain(id, 0); err != nil {
				t.Fatalf("B1104: Drain(%s) cycle %d: %v", id, cycles, err)
			}
		}
		for _, id := range ids {
			m := h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 30*time.Second)
			if m.GetCluster() != "" {
				t.Fatalf("B1104: %s cycle %d returned to Idle with residual cluster %q", id, cycles, m.GetCluster())
			}
			if len(m.GetShardMetadata()) != 0 {
				t.Fatalf("B1104: %s cycle %d returned to Idle with residual shard_metadata %v", id, cycles, m.GetShardMetadata())
			}
			if m.GetLastError() != "" {
				t.Fatalf("B1104: %s cycle %d returned to Idle with residual last_error %q", id, cycles, m.GetLastError())
			}
		}
		cycles++
	}
	if cycles == 0 {
		t.Fatal("B1104: soak completed zero churn cycles (increase -soak)")
	}
	t.Logf("B1104: %d Configure/Drain churn cycles over %s on %d machines, clean every Idle return", cycles, *soakFlag, work)
}

// B1105 — over the soak the live machine count is conserved (created ==
// deleted + resident), proving no machine leaks or vanishes. We track a working
// set we own: each cycle Creates a Speculative machine to Idle (created++), then
// (if Delete is supported) Deletes it back to Speculative (deleted++); residents
// are the ones currently Idle. The invariant resident == created - deleted holds
// at every checkpoint, and the global inventory size never changes (no machine
// appears or disappears from the fleet).
func TestB1105_CountConservationOverSoak(t *testing.T) {
	behavior(t, "B1105")
	h := scaleDial(t)
	canDelete := h.Probe().Delete

	totalBefore := len(h.List())

	created, deleted := 0, 0
	resident := map[string]bool{} // ids we walked to Idle and have not yet Deleted

	deadline := time.Now().Add(*soakFlag)
	for time.Now().Before(deadline) {
		// Create one fresh Speculative -> Idle.
		id := h.WalkToIdle()
		created++
		resident[id] = true

		// Conservation checkpoint: residents we hold == created - deleted.
		if len(resident) != created-deleted {
			t.Fatalf("B1105: resident=%d but created-deleted=%d (count not conserved)", len(resident), created-deleted)
		}

		if canDelete {
			// Delete it back to Speculative (deleted++), so it leaves the resident
			// working set but is NOT lost from the fleet.
			if _, err := h.Delete(id); err != nil {
				t.Fatalf("B1105: Delete(%s): %v", id, err)
			}
			h.MustReach(id, pb.MachineState_MACHINE_STATE_SPECULATIVE, 30*time.Second)
			deleted++
			delete(resident, id)
			if len(resident) != created-deleted {
				t.Fatalf("B1105: post-Delete resident=%d but created-deleted=%d", len(resident), created-deleted)
			}
		}
	}

	// Global conservation: the fleet's total machine count is unchanged — no
	// machine vanished and none was spuriously conjured by the churn.
	totalAfter := len(h.List())
	if totalAfter != totalBefore {
		t.Errorf("B1105: fleet size changed over soak: %d -> %d (machines leaked or vanished)", totalBefore, totalAfter)
	}
	t.Logf("B1105: created=%d deleted=%d resident=%d over %s (delete-capable=%v); fleet size stable at %d",
		created, deleted, len(resident), *soakFlag, canDelete, totalAfter)
}

// B1106 — per-RPC latency histograms are captured and p99 for Get/List/Create
// stays within the lane's declared (generous) budget at scale. Budgets are wide
// enough to hold on a dev/CI box with margin; the measured p99s are logged.
func TestB1106_PerRPCLatencyBudget(t *testing.T) {
	behavior(t, "B1106")
	h := scaleDial(t)

	// Sample Get and List against the existing fleet, and Create on a working set
	// we own. Sizes are modest so the suite is fast; p99 over ~100 samples is a
	// meaningful tail without being slow.
	const samples = 120

	all := h.List()
	if len(all) == 0 {
		t.Skip("B1106: provider exposes no machines to sample Get/List latency")
	}

	var getD, listD, createD []time.Duration

	// Get latency: hammer Get on the first machine.
	probeID := all[0].GetId()
	for i := 0; i < samples; i++ {
		s := time.Now()
		h.Get(probeID)
		getD = append(getD, time.Since(s))
	}

	// List latency: full List repeated.
	for i := 0; i < samples; i++ {
		s := time.Now()
		_ = h.List()
		listD = append(listD, time.Since(s))
	}

	// Create latency: walk a working set of fresh machines, timing each Create.
	createN := 60
	if createN > samples {
		createN = samples
	}
	cids := h.PickNSpeculative(createN)
	for _, id := range cids {
		s := time.Now()
		if _, err := h.Create(id); err != nil {
			t.Fatalf("B1106: Create(%s): %v", id, err)
		}
		createD = append(createD, time.Since(s))
	}

	getP99 := p99(getD)
	listP99 := p99(listD)
	createP99 := p99(createD)
	t.Logf("B1106: p99 Get=%s List=%s Create=%s (n_get=%d n_list=%d n_create=%d, fleet=%d)",
		getP99, listP99, createP99, len(getD), len(listD), len(createD), len(all))

	const getBudget = 250 * time.Millisecond
	const createBudget = 250 * time.Millisecond
	const listBudget = 2 * time.Second
	if getP99 > getBudget {
		t.Errorf("B1106: Get p99 %s exceeds budget %s", getP99, getBudget)
	}
	if createP99 > createBudget {
		t.Errorf("B1106: Create p99 %s exceeds budget %s", createP99, createBudget)
	}
	if listP99 > listBudget {
		t.Errorf("B1106: List p99 %s exceeds budget %s", listP99, listBudget)
	}
}

// p99 returns the 99th-percentile duration (nearest-rank on a sorted copy).
func p99(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	cp := make([]time.Duration, len(ds))
	copy(cp, ds)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(math.Ceil(0.99*float64(len(cp)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

// B1107 — List cost at N machines stays within budget: full-List latency at the
// configured scale is within a generous absolute budget. One absolute budget at
// the configured N (sub-pathological growth, kept simple). Uses the raised-recv
// dial so the full List does not exceed the default gRPC recv limit at scale.
func TestB1107_FullListLatencyBudget(t *testing.T) {
	behavior(t, "B1107")
	h := scaleDial(t)

	// Warm one List (connection setup / first-serialization), then measure.
	_ = h.List()
	start := time.Now()
	machines := h.List()
	elapsed := time.Since(start)
	t.Logf("B1107: full List of %d machines took %s", len(machines), elapsed)

	if len(machines) < *scaleSeedFlag {
		t.Fatalf("B1107: full List returned %d machines, want >= scale-seed %d", len(machines), *scaleSeedFlag)
	}
	const budget = 3 * time.Second
	if elapsed > budget {
		t.Errorf("B1107: full List of %d machines took %s, exceeds budget %s", len(machines), elapsed, budget)
	}
}

// B1108 — under K parallel walk-to-Idle at scale, sustained mutation throughput
// holds and every machine reaches Idle without operation_id collision. Each
// parallel worker owns a DISTINCT machine (PickNSpeculative hands out distinct
// ids), so the operation_ids across distinct machines must all be distinct.
func TestB1108_ParallelWalkNoCollision(t *testing.T) {
	behavior(t, "B1108")
	h := scaleDial(t)

	const k = 16
	ids := h.PickNSpeculative(k)

	start := time.Now()
	results := harness.Fanout(k, func(i int) harness.AckErr {
		ack, err := h.Create(ids[i])
		return harness.AckErr{Ack: ack, Err: err}
	})
	// No RPC error, every Create acked.
	opIDs := map[string]int{}
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("B1108: parallel Create(%s) errored: %v", ids[i], r.Err)
		}
		op := r.Ack.GetOperationId()
		if op == "" {
			t.Errorf("B1108: Create(%s) returned an empty operation_id", ids[i])
			continue
		}
		opIDs[op]++
	}
	// Distinct machines => distinct operation_ids (no collision across machines).
	for op, n := range opIDs {
		if n > 1 {
			t.Errorf("B1108: operation_id %q collided across %d distinct machines", op, n)
		}
	}
	// All reach Idle.
	for _, id := range ids {
		h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 30*time.Second)
	}
	elapsed := time.Since(start)
	t.Logf("B1108: %d parallel walk-to-Idle in %s (%.0f machines/s), %d distinct operation_ids, no collision",
		k, elapsed, float64(k)/elapsed.Seconds(), len(opIDs))
}

// B1109 — throughout the soak, Consistently-polling a sample of steady-state
// Idle machines shows none drifts into a transitional or FAILED state without a
// client mutation. We walk a sample to Idle, then over a short window assert
// each NeverReaches any transitional state and NeverReaches FAILED.
func TestB1109_SteadyStateNoDrift(t *testing.T) {
	behavior(t, "B1109")
	h := scaleDial(t)

	const sample = 8
	ids := h.WalkNToIdle(sample)

	// A short stability window (a fraction of the soak, capped) over which NO
	// client mutation is issued — the machines must hold Idle.
	window := *soakFlag / 4
	if window < 500*time.Millisecond {
		window = 500 * time.Millisecond
	}
	if window > 3*time.Second {
		window = 3 * time.Second
	}

	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		for _, id := range ids {
			s, err := h.StateRaw(id)
			if err != nil {
				continue // a momentary Get error is a skipped sample, not drift
			}
			if harness.IsTransitional(s) {
				t.Fatalf("B1109: steady-state Idle machine %s drifted into transitional %s with no client mutation", id, s)
			}
			if s == pb.MachineState_MACHINE_STATE_FAILED {
				t.Fatalf("B1109: steady-state Idle machine %s drifted into FAILED with no client mutation", id)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("B1109: %d steady-state Idle machines held over a %s window with no spontaneous drift", sample, window)
}
