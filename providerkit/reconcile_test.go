package providerkit

import (
	"context"
	"testing"
	"time"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

// Reconcile refreshes a tracked machine's mutable substrate facts (a moving spot
// price, a raised interruption probability) while preserving the kit-owned
// lifecycle/binding overlay.
func TestReconcile_RefreshesSubstratePreservesOverlay(t *testing.T) {
	b := &fakeBackend{seed: []Instance{{
		ID: "m1", State: StateSpeculative,
		InstanceType: "c7g.xlarge", Zone: "us-east-1a", CapacityType: CapacitySpot,
		PricePerHour: 0.05, InterruptionProbability: 0.05,
		Resources: map[string]string{"cpu": "1"},
	}}}
	s, err := New(b, NewMemStore(), quietOptions())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Bind m1 (so it has a lifecycle/binding overlay).
	create(t, s, "m1")
	waitState(t, s, "m1", pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)
	configure(t, s, "m1", "cluster-x", map[string]string{"k": "v"})
	waitState(t, s, "m1", pb.MachineState_MACHINE_STATE_CONFIGURED, 2*time.Second)

	// The substrate's facts move; reconcile must pick them up.
	b.seed[0].PricePerHour = 0.21
	b.seed[0].InterruptionProbability = 0.40
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	m := getMachine(t, s, "m1")
	if m.GetPricePerHour() != 0.21 {
		t.Errorf("price not refreshed: %v, want 0.21", m.GetPricePerHour())
	}
	if m.GetInterruptionProbability() != 0.40 {
		t.Errorf("interruption_probability not refreshed: %v, want 0.40", m.GetInterruptionProbability())
	}
	// Overlay preserved.
	if m.GetState() != pb.MachineState_MACHINE_STATE_CONFIGURED {
		t.Errorf("state changed to %s, want CONFIGURED", m.GetState())
	}
	if m.GetCluster() != "cluster-x" || m.GetShardMetadata()["k"] != "v" {
		t.Errorf("binding overlay clobbered: cluster=%q md=%v", m.GetCluster(), m.GetShardMetadata())
	}
}

// An idempotent reconcile (no substrate change) bumps no revision — so a
// since_revision poller isn't churned by reconcile ticks.
func TestReconcile_NoChangeNoRevisionBump(t *testing.T) {
	s, _ := newTestServer(t, 3)
	r0, _ := s.List(bg(), &pb.ListFilter{})
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	r1, _ := s.List(bg(), &pb.ListFilter{})
	if string(r0.GetRevision()) != string(r1.GetRevision()) {
		t.Errorf("idempotent reconcile bumped the revision %s -> %s", r0.GetRevision(), r1.GetRevision())
	}
}
