package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestBackend(t *testing.T, seedCount int) (*gcpBackend, *gceFake) {
	t.Helper()
	fake := newGCEFake()
	logger := quietLogger()
	offs := defaultOfferings(seedCount, "us-central1-a", "us-central1-b")
	b, err := newGCPBackend("gcp-test", "us-central1", fake, offs, newPricing("us-central1", newStaticPricer("us-central1"), logger), newInterruption(), nil, logger)
	if err != nil {
		t.Fatalf("newGCPBackend: %v", err)
	}
	return b, fake
}

func newTestServer(t *testing.T, b *gcpBackend) *providerkit.Server {
	t.Helper()
	s, err := providerkit.New(b, providerkit.NewMemStore(), providerkit.Options{
		RequireZone: true,
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("providerkit.New: %v", err)
	}
	return s
}

func waitState(t *testing.T, s *providerkit.Server, id string, want pb.MachineState) *pb.Machine {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m, err := s.Get(context.Background(), &pb.MachineRef{Id: id})
		if err == nil && m.GetState() == want {
			return m
		}
		time.Sleep(2 * time.Millisecond)
	}
	m, _ := s.Get(context.Background(), &pb.MachineRef{Id: id})
	t.Fatalf("machine %s did not reach %s (final %s)", id, want, m.GetState())
	return nil
}

// The default offerings must seed valid field shape, including SPOT machines
// that declare a real (> 0) interruption_probability, while on-demand machines
// declare exactly 0.
func TestDefaultOfferings_FieldShape(t *testing.T) {
	b, _ := newTestBackend(t, 32)
	s := newTestServer(t, b)
	resp, err := s.List(context.Background(), &pb.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.GetMachines()) != 32 {
		t.Fatalf("seeded %d machines, want 32", len(resp.GetMachines()))
	}
	sawSpot := false
	for _, m := range resp.GetMachines() {
		if m.GetInstanceType() == "" {
			t.Errorf("%s: empty instance_type", m.GetId())
		}
		if m.GetZone() == "" {
			t.Errorf("%s: empty zone (RequireZone)", m.GetId())
		}
		if m.GetAllocatable() == nil || len(m.GetAllocatable().GetResources()) == 0 {
			t.Errorf("%s: missing allocatable", m.GetId())
		}
		if m.GetPricePerHour() <= 0 {
			t.Errorf("%s: price_per_hour = %v, want > 0", m.GetId(), m.GetPricePerHour())
		}
		switch m.GetCapacityType() {
		case pb.CapacityType_CAPACITY_TYPE_SPOT:
			sawSpot = true
			if !(m.GetInterruptionProbability() > 0) {
				t.Errorf("%s: SPOT machine with interruption_probability %v (must be > 0)", m.GetId(), m.GetInterruptionProbability())
			}
		case pb.CapacityType_CAPACITY_TYPE_ON_DEMAND:
			if m.GetInterruptionProbability() != 0 {
				t.Errorf("%s: on-demand machine with non-zero interruption_probability %v", m.GetId(), m.GetInterruptionProbability())
			}
		default:
			t.Errorf("%s: unexpected capacity_type %v", m.GetId(), m.GetCapacityType())
		}
	}
	if !sawSpot {
		t.Error("default offerings seeded no SPOT machines; the SPOT invariant would never fire")
	}
}

// A full lifecycle through providerkit drives the GCE fake: Create launches an
// instance (host appears), Configure binds it + echoes shard_metadata, Drain
// unbinds + clears it, Delete deletes it (slot returns to Speculative).
func TestFullLifecycle_DrivesGCE(t *testing.T) {
	b, fake := newTestBackend(t, 8)
	s := newTestServer(t, b)
	ctx := context.Background()

	resp, _ := s.List(ctx, &pb.ListFilter{States: []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE}, MaxResults: 1})
	id := resp.GetMachines()[0].GetId()

	if _, err := s.Create(ctx, &pb.CreateRequest{MachineId: id}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m := waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)
	if m.GetHost().GetRef() == "" {
		t.Fatal("Idle machine has no host (Insert result not attached)")
	}
	if got := len(fake.instances); got != 1 {
		t.Fatalf("GCE fake has %d instances after Create, want 1", got)
	}

	if _, err := s.Configure(ctx, &pb.ConfigureRequest{MachineId: id, ClusterId: "c1", BootstrapBlob: []byte("join"), ShardMetadata: map[string]string{"k": "v"}}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	m = waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED)
	if m.GetCluster() != "c1" {
		t.Errorf("cluster = %q, want c1", m.GetCluster())
	}
	if m.GetShardMetadata()["k"] != "v" {
		t.Errorf("shard_metadata not echoed: %v", m.GetShardMetadata())
	}

	if _, err := s.Drain(ctx, &pb.DrainRequest{MachineId: id, GracePeriodSeconds: 5}); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	m = waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)
	if m.GetCluster() != "" || len(m.GetShardMetadata()) != 0 {
		t.Errorf("cluster/shard_metadata not cleared on Drain to Idle: cluster=%q meta=%v", m.GetCluster(), m.GetShardMetadata())
	}

	if _, err := s.Delete(ctx, &pb.DeleteRequest{MachineId: id}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	m = waitState(t, s, id, pb.MachineState_MACHINE_STATE_SPECULATIVE)
	if m.GetHost().GetRef() != "" {
		t.Error("Speculative machine still has a host after Delete")
	}
	if got := len(fake.instances); got != 0 {
		t.Errorf("GCE fake has %d instances after Delete, want 0", got)
	}
}

// Describe must reconcile a running managed instance back to its offering slot as
// Idle (recovery from GCE labels when there is no persisted store).
func TestDescribe_ReconcilesRunningInstance(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slot := b.speculativeSlots()[0]
	if _, err := fake.Insert(ctx, instanceSpec{MachineID: slot.ID, MachineType: slot.InstanceType, Zone: slot.Zone}); err != nil {
		t.Fatalf("seed instance: %v", err)
	}

	got, err := b.Describe(ctx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	var found *providerkit.Instance
	for i := range got {
		if got[i].ID == slot.ID {
			found = &got[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("Describe did not return slot %s", slot.ID)
	}
	if found.State != providerkit.StateIdle {
		t.Errorf("backed slot state = %v, want Idle", found.State)
	}
	if found.Host.Ref == "" {
		t.Error("backed slot has no host")
	}
}

// Insert must be idempotent at the substrate level: a retried Insert with the
// same operation id returns the same instance, never a duplicate.
func TestInsert_IdempotentOnToken(t *testing.T) {
	fake := newGCEFake()
	ctx := context.Background()
	spec := instanceSpec{MachineID: "m1", MachineType: "n2-standard-4", Zone: "us-central1-a", IdempotencyToken: "op-1"}
	a, err := fake.Insert(ctx, spec)
	if err != nil {
		t.Fatalf("insert a: %v", err)
	}
	b, err := fake.Insert(ctx, spec)
	if err != nil {
		t.Fatalf("insert b: %v", err)
	}
	if a.Name != b.Name {
		t.Errorf("idempotent insert returned distinct names %s vs %s", a.Name, b.Name)
	}
	if len(fake.instances) != 1 {
		t.Errorf("idempotent insert launched %d instances, want 1", len(fake.instances))
	}
}

// An orphan / offering-shrank instance rebinds with the per-replica resources of
// a still-configured offering for its type, so it keeps matching its profile.
func TestInstanceToIdle_RecoversResources(t *testing.T) {
	b, _ := newTestBackend(t, 4)
	got := b.instanceToIdle("orphan-1", gceInstance{Name: "x", Zone: "us-central1-a", MachineType: "n2-standard-4"})
	if got.Resources["cpu"] == "" {
		t.Errorf("rebound machine has no resources; want the offering's per-replica shape, got %v", got.Resources)
	}
	if r := b.resourcesForType("zz-unoffered", "us-central1-a"); r != nil {
		t.Errorf("unoffered type resources = %v, want nil", r)
	}
}

// A simulated GCE preemption of a SPOT instance must be observed and raise that
// slot's interruption probability above its forecast (the "observed" half of the
// field-shape contract), while leaving non-preempted slots on the forecast.
func TestObservePreemptions_RaisesSpotProbability(t *testing.T) {
	b, fake := newTestBackend(t, 8)
	ctx := context.Background()

	var spot providerkit.Instance
	for _, s := range b.speculativeSlots() {
		if s.CapacityType == providerkit.CapacitySpot {
			spot = s
			break
		}
	}
	if spot.ID == "" {
		t.Fatal("default offerings seeded no SPOT slot")
	}
	inst, err := fake.Insert(ctx, instanceSpec{MachineID: spot.ID, MachineType: spot.InstanceType, Zone: spot.Zone, Spot: true})
	if err != nil {
		t.Fatalf("insert spot instance: %v", err)
	}

	before := b.interruption.probability(spot.ID, spot.InstanceType, providerkit.CapacitySpot)

	// No preemption yet: observing finds nothing.
	if n, err := b.observePreemptions(ctx); err != nil || n != 0 {
		t.Fatalf("observePreemptions before preemption = (%d, %v), want (0, nil)", n, err)
	}

	// Simulate GCE preempting the spot VM, then observe it.
	fake.preempt(inst.Zone, inst.Name)
	n, err := b.observePreemptions(ctx)
	if err != nil {
		t.Fatalf("observePreemptions: %v", err)
	}
	if n != 1 {
		t.Fatalf("observed %d preemptions, want 1", n)
	}

	after := b.interruption.probability(spot.ID, spot.InstanceType, providerkit.CapacitySpot)
	if !(after > before) {
		t.Errorf("interruption probability did not rise after preemption: before=%v after=%v", before, after)
	}
	if after != observedPreemptionProbability {
		t.Errorf("observed probability = %v, want %v", after, observedPreemptionProbability)
	}
}

// An on-demand instance is never treated as preempted (preemption is a SPOT-only
// signal), so observing leaves its (zero) probability untouched.
func TestObservePreemptions_IgnoresOnDemand(t *testing.T) {
	b, fake := newTestBackend(t, 8)
	ctx := context.Background()

	var od providerkit.Instance
	for _, s := range b.speculativeSlots() {
		if s.CapacityType == providerkit.CapacityOnDemand {
			od = s
			break
		}
	}
	if od.ID == "" {
		t.Fatal("default offerings seeded no on-demand slot")
	}
	inst, err := fake.Insert(ctx, instanceSpec{MachineID: od.ID, MachineType: od.InstanceType, Zone: od.Zone, Spot: false})
	if err != nil {
		t.Fatalf("insert on-demand instance: %v", err)
	}
	fake.preempt(inst.Zone, inst.Name) // a no-op for non-spot in the simulator
	n, err := b.observePreemptions(ctx)
	if err != nil {
		t.Fatalf("observePreemptions: %v", err)
	}
	if n != 0 {
		t.Errorf("observed %d preemptions for an on-demand instance, want 0", n)
	}
	if p := b.interruption.probability(od.ID, od.InstanceType, providerkit.CapacityOnDemand); p != 0 {
		t.Errorf("on-demand interruption probability = %v, want 0", p)
	}
}

func TestOffering_CapacityType(t *testing.T) {
	cases := map[string]providerkit.CapacityType{
		"on_demand": providerkit.CapacityOnDemand,
		"":          providerkit.CapacityOnDemand,
		"spot":      providerkit.CapacitySpot,
		"reserved":  providerkit.CapacityReserved,
	}
	for in, want := range cases {
		if ct, err := (offering{Capacity: in}).capacityType(); err != nil || ct != want {
			t.Errorf("capacity_type %q: got (%v, %v), want (%v, nil)", in, ct, err, want)
		}
	}
	for _, bad := range []string{"bare_metal", "metal", "nonsense"} {
		if _, err := (offering{MachineType: "n2-standard-4", Zone: "us-central1-a", Capacity: bad}).capacityType(); err == nil {
			t.Errorf("expected capacity_type %q to be rejected", bad)
		}
	}
}

func TestHostRef_RoundTrip(t *testing.T) {
	ref := hostRef(gceInstance{Zone: "us-central1-a", Name: "bf-abc"})
	zone, name, err := parseHostRef(ref)
	if err != nil || zone != "us-central1-a" || name != "bf-abc" {
		t.Fatalf("round-trip %q -> (%q,%q,%v)", ref, zone, name, err)
	}
	if _, _, err := parseHostRef("garbage"); err == nil {
		t.Error("parseHostRef accepted a malformed ref")
	}
}
