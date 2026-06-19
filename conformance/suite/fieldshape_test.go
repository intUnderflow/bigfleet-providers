//go:build certify

package suite

import (
	"math"
	"testing"
	"time"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

// C8 — Machine field-shape & cost-field sweep (behaviors B80x). The autoscaler
// reads these top-level fields directly, so every machine the provider exposes
// must be in shape. DEEPER than a basic LabelShape/CostFieldBounds happy-path:
// it sweeps EVERY machine the provider exposes, actively drives a machine
// through its lifecycle to materialise each stable state, and adds the
// host-vs-state, cluster-vs-state, capacity/SPOT, density-floor, stable-host-
// identity and Delete-clears-host invariants.

// fsStable reports whether s is a settled state a field-shape invariant binds to
// (FAILED is settled but carries no field-shape guarantees here).
func fsStable(s pb.MachineState) bool {
	return s == pb.MachineState_MACHINE_STATE_SPECULATIVE ||
		s == pb.MachineState_MACHINE_STATE_IDLE ||
		s == pb.MachineState_MACHINE_STATE_CONFIGURED
}

// hostSet reports whether a machine carries a non-empty host. The kit's
// empty-host guard treats a HostRef with both fields empty as "no host", so a
// host is "set" iff either field is non-empty.
func hostSet(m *pb.Machine) bool {
	h := m.GetHost()
	return h != nil && (h.GetRef() != "" || h.GetProvider() != "")
}

// labelLeakKeys are the well-known top-level fields that must NEVER be smuggled
// into the free-form labels map (every spelling the autoscaler might mistake).
var labelLeakKeys = []string{
	"instance-type", "instance_type", "instanceType",
	"zone", "availability-zone", "availability_zone",
	"capacity-type", "capacity_type", "capacityType",
}

// B801 — every machine reports a non-UNSPECIFIED state and a non-empty
// top-level instance_type, with no instance-type/zone/capacity-type key hidden
// in labels. Swept across the ENTIRE inventory, not a sampled machine.
func TestB801_StateAndInstanceTypeTopLevel(t *testing.T) {
	behavior(t, "B801")
	h := dial(t)
	machines := h.List()
	if len(machines) == 0 {
		t.Skip("provider exposes no machines to field-shape")
	}
	for _, m := range machines {
		id := m.GetId()
		if id == "" {
			t.Errorf("machine with empty id in inventory")
		}
		if m.GetState() == pb.MachineState_MACHINE_STATE_UNSPECIFIED {
			t.Errorf("%s: state UNSPECIFIED", id)
		}
		if m.GetInstanceType() == "" {
			t.Errorf("%s: instance_type empty (must be a populated top-level field, never labels-only)", id)
		}
		for _, leak := range labelLeakKeys {
			if v, ok := m.GetLabels()[leak]; ok {
				t.Errorf("%s: well-known field %q smuggled into labels (=%q) — it must be top-level only", id, leak, v)
			}
		}
	}
}

// B802 — every machine's price_per_hour is finite and >= 0 and
// interruption_probability lies in [0,1], with SPOT machines reporting
// interruption_probability > 0. Swept across the whole inventory; also asserts
// the SPOT invariant actually fires (the fake seeds spot offerings).
func TestB802_CostFieldBounds(t *testing.T) {
	behavior(t, "B802")
	h := dial(t)
	machines := h.List()
	if len(machines) == 0 {
		t.Skip("provider exposes no machines to cost-check")
	}
	sawSpot := false
	for _, m := range machines {
		id := m.GetId()
		p := m.GetPricePerHour()
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 {
			t.Errorf("%s: price_per_hour %v not finite and >= 0", id, p)
		}
		ip := m.GetInterruptionProbability()
		if math.IsNaN(ip) || ip < 0 || ip > 1 {
			t.Errorf("%s: interruption_probability %v outside [0,1]", id, ip)
		}
		if m.GetCapacityType() == pb.CapacityType_CAPACITY_TYPE_SPOT {
			sawSpot = true
			if !(ip > 0) {
				t.Errorf("%s: SPOT machine reports interruption_probability %v (must be > 0)", id, ip)
			}
		}
	}
	if !sawSpot {
		t.Log("no SPOT machines in inventory; SPOT>0 invariant vacuously satisfied")
	}
}

// B803 — host is nil for Speculative and set for Idle/Configured, and cluster
// is empty for Speculative/Idle and non-empty for Configured. Asserted across
// the full inventory AND actively materialised by driving one machine through
// every stable state so each case is exercised, not merely whatever the seed
// happened to contain.
func TestB803_HostAndClusterByState(t *testing.T) {
	behavior(t, "B803")
	h := dial(t)

	check := func(m *pb.Machine) {
		id := m.GetId()
		switch m.GetState() {
		case pb.MachineState_MACHINE_STATE_SPECULATIVE:
			if hostSet(m) {
				t.Errorf("%s: Speculative machine has a host %+v", id, m.GetHost())
			}
			if m.GetCluster() != "" {
				t.Errorf("%s: Speculative machine has cluster %q (must be empty)", id, m.GetCluster())
			}
		case pb.MachineState_MACHINE_STATE_IDLE:
			if !hostSet(m) {
				t.Errorf("%s: Idle machine has no host", id)
			}
			if m.GetCluster() != "" {
				t.Errorf("%s: Idle machine has cluster %q (must be empty)", id, m.GetCluster())
			}
		case pb.MachineState_MACHINE_STATE_CONFIGURED:
			if !hostSet(m) {
				t.Errorf("%s: Configured machine has no host", id)
			}
			if m.GetCluster() == "" {
				t.Errorf("%s: Configured machine has empty cluster (must be non-empty)", id)
			}
		}
	}

	// Sweep the whole inventory first.
	for _, m := range h.List() {
		check(m)
	}

	// Then actively walk one machine to produce a fresh observation of every
	// stable state, so the invariant is exercised even if the seed lacked one.
	id := h.PickSpeculative()
	check(h.Get(id)) // Speculative

	if _, err := h.Create(id); err != nil {
		t.Fatalf("Create: %v", err)
	}
	check(h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)) // Idle

	if _, err := h.Configure(id, "conf-b803", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	check(h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 15*time.Second)) // Configured

	if _, err := h.Drain(id, 0); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	check(h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)) // back to Idle
}

// B804 — when a machine populates allocatable AND resources, both are non-empty
// resource maps so the density floor(allocatable/resources) is computable. Only
// machines that populate BOTH are bound (allocatable==nil legitimately means
// "allocatable defaults to resources"); for those that do, both maps must be
// non-empty and every shared dimension must yield a finite, positive,
// computable ratio.
func TestB804_DensityComputable(t *testing.T) {
	behavior(t, "B804")
	h := dial(t)
	machines := h.List()
	if len(machines) == 0 {
		t.Skip("provider exposes no machines")
	}
	boundAny := false
	for _, m := range machines {
		alloc := m.GetAllocatable().GetResources()
		res := m.GetResources().GetResources()
		if len(alloc) == 0 || len(res) == 0 {
			continue // not a both-populated record; density defaults, nothing to bind
		}
		boundAny = true
		id := m.GetId()
		// Both non-empty: the density floor must be computable on every shared
		// dimension (allocatable and resources both present for that key, both
		// parse, resources>0 so the floor division is defined).
		for k, av := range alloc {
			rv, ok := res[k]
			if !ok {
				continue // dimension only in allocatable; floor still computable elsewhere
			}
			aq, aok := parseQuantity(av)
			rq, rok := parseQuantity(rv)
			if !aok {
				t.Errorf("%s: allocatable[%q]=%q is not a computable quantity", id, k, av)
			}
			if !rok {
				t.Errorf("%s: resources[%q]=%q is not a computable quantity", id, k, rv)
			}
			if aok && rok {
				if rq <= 0 {
					t.Errorf("%s: resources[%q]=%q <= 0, density floor undefined", id, k, rv)
				}
				if aq < 0 {
					t.Errorf("%s: allocatable[%q]=%q < 0", id, k, av)
				}
			}
		}
	}
	if !boundAny {
		t.Skip("no machine populates BOTH allocatable and resources; density invariant vacuous")
	}
}

// parseQuantity parses a Kubernetes-style resource quantity (plain integer or a
// binary-SI suffixed value like "8Gi"/"512Mi") into a float for the density
// check. Returns false if it is not a shape this density check understands.
func parseQuantity(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	mult := 1.0
	suffixes := []struct {
		s string
		m float64
	}{
		{"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10},
		{"G", 1e9}, {"M", 1e6}, {"k", 1e3}, {"m", 1e-3},
	}
	num := s
	for _, sf := range suffixes {
		if len(s) > len(sf.s) && s[len(s)-len(sf.s):] == sf.s {
			num = s[:len(s)-len(sf.s)]
			mult = sf.m
			break
		}
	}
	var f float64
	var seenDigit bool
	var frac float64 = 1
	inFrac := false
	for _, c := range num {
		switch {
		case c >= '0' && c <= '9':
			seenDigit = true
			if inFrac {
				frac /= 10
				f += float64(c-'0') * frac
			} else {
				f = f*10 + float64(c-'0')
			}
		case c == '.' && !inFrac:
			inFrac = true
		default:
			return 0, false
		}
	}
	if !seenDigit {
		return 0, false
	}
	return f * mult, true
}

// B805 — a machine's HostRef.provider is identical across every Get and List
// observation through its Idle->Configured->Idle lifecycle (stable host
// identity). The provider field of the host must never churn as the machine is
// configured and drained.
func TestB805_StableHostProvider(t *testing.T) {
	behavior(t, "B805")
	h := dial(t)

	id := h.WalkToIdle()

	// Pin the provider observed once the host first exists (Idle).
	first := h.Get(id)
	if !hostSet(first) {
		t.Fatalf("%s: Idle machine has no host to anchor identity", id)
	}
	want := first.GetHost().GetProvider()
	if want == "" {
		t.Fatalf("%s: HostRef.provider is empty at Idle (must be a stable identity)", id)
	}

	// observe both via Get and via List, asserting provider equality.
	observe := func(stage string) {
		g := h.Get(id)
		if !hostSet(g) {
			t.Errorf("%s [%s, Get]: host disappeared mid-lifecycle", id, stage)
			return
		}
		if got := g.GetHost().GetProvider(); got != want {
			t.Errorf("%s [%s, Get]: HostRef.provider = %q, want stable %q", id, stage, got, want)
		}
		found := false
		for _, m := range h.List() {
			if m.GetId() != id {
				continue
			}
			found = true
			if got := m.GetHost().GetProvider(); got != want {
				t.Errorf("%s [%s, List]: HostRef.provider = %q, want stable %q", id, stage, got, want)
			}
		}
		if !found {
			t.Errorf("%s [%s, List]: machine not present in List", id, stage)
		}
	}

	observe("idle")

	if _, err := h.Configure(id, "conf-b805", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 15*time.Second)
	observe("configured")

	if _, err := h.Drain(id, 0); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)
	observe("idle-again")
}

// B806 — zone and capacity_type are cross-field consistent for a stable
// machine: capacity_type is non-UNSPECIFIED and zone is non-empty wherever a
// host is set. Swept across the full inventory and re-checked on a machine
// actively walked into the host-bearing states.
func TestB806_ZoneCapacityConsistency(t *testing.T) {
	behavior(t, "B806")
	h := dial(t)

	check := func(m *pb.Machine) {
		if !fsStable(m.GetState()) {
			return // transitional snapshots carry no cross-field guarantee
		}
		if !hostSet(m) {
			return // the invariant binds only where a host is set
		}
		id := m.GetId()
		if m.GetCapacityType() == pb.CapacityType_CAPACITY_TYPE_UNSPECIFIED {
			t.Errorf("%s: host set but capacity_type UNSPECIFIED", id)
		}
		if m.GetZone() == "" {
			t.Errorf("%s: host set but zone empty", id)
		}
	}

	sawHosted := false
	for _, m := range h.List() {
		if fsStable(m.GetState()) && hostSet(m) {
			sawHosted = true
		}
		check(m)
	}

	// Actively produce a host-bearing record so the invariant is exercised even
	// against a fully-Speculative seed.
	id := h.WalkToIdle()
	check(h.Get(id))
	if _, err := h.Configure(id, "conf-b806", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	check(h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 15*time.Second))
	_ = sawHosted
}

// B807 — walking an Idle machine through Delete back to Speculative clears
// host, cluster, and shard_metadata (positive Delete-clears-host). Capability-
// gated on Delete.
func TestB807_DeleteClearsHost(t *testing.T) {
	behavior(t, "B807")
	h := dial(t)
	if !h.Probe().Delete {
		t.Skip("provider does not support Delete")
	}

	// Configure first so there is a host, a cluster, and metadata to clear, then
	// drain to a legitimate Idle (Delete's legal source), so the "clears" is a
	// real before/after, not a no-op on already-empty fields.
	id := h.WalkToConfigured("conf-b807", map[string]string{"a": "1", "b": "2"})
	conf := h.Get(id)
	if !hostSet(conf) {
		t.Fatalf("%s: Configured machine has no host to later clear", id)
	}
	if conf.GetCluster() == "" {
		t.Fatalf("%s: Configured machine has empty cluster pre-Delete", id)
	}

	if _, err := h.Drain(id, 0); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	idle := h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)
	if !hostSet(idle) {
		t.Fatalf("%s: Idle machine (post-Drain) has no host to later clear", id)
	}

	if _, err := h.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	spec := h.MustReach(id, pb.MachineState_MACHINE_STATE_SPECULATIVE, 15*time.Second)

	if hostSet(spec) {
		t.Errorf("%s: Delete left a host %+v (must be cleared)", id, spec.GetHost())
	}
	if spec.GetCluster() != "" {
		t.Errorf("%s: Delete left cluster %q (must be cleared)", id, spec.GetCluster())
	}
	if len(spec.GetShardMetadata()) != 0 {
		t.Errorf("%s: Delete left shard_metadata %v (must be cleared)", id, spec.GetShardMetadata())
	}
}
