package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"

	"github.com/intUnderflow/bigfleet-providers/internal/providerkit"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestBackend(t *testing.T, seedCount int) (*awsBackend, *ec2Fake) {
	t.Helper()
	fake := newEC2Fake()
	logger := quietLogger()
	offs := defaultOfferings(seedCount, "us-east-1a", "us-east-1b")
	b, err := newAWSBackend("aws-test", "us-east-1", fake, offs, newPricing("us-east-1", fake, logger), newInterruption(), nil, logger)
	if err != nil {
		t.Fatalf("newAWSBackend: %v", err)
	}
	return b, fake
}

func newTestServer(t *testing.T, b *awsBackend) *providerkit.Server {
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

// The default offerings must seed valid field shape — and crucially, every
// SPOT slot must carry a real, non-zero interruption probability.
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
	for _, m := range resp.GetMachines() {
		if m.GetInstanceType() == "" {
			t.Errorf("%s: empty instance_type", m.GetId())
		}
		if m.GetZone() == "" {
			t.Errorf("%s: empty zone (RequireZone)", m.GetId())
		}
		if m.GetCapacityType() == pb.CapacityType_CAPACITY_TYPE_UNSPECIFIED {
			t.Errorf("%s: unspecified capacity_type", m.GetId())
		}
		if m.GetAllocatable() == nil || len(m.GetAllocatable().GetResources()) == 0 {
			t.Errorf("%s: missing allocatable", m.GetId())
		}
		if m.GetCapacityType() == pb.CapacityType_CAPACITY_TYPE_SPOT && m.GetInterruptionProbability() <= 0 {
			t.Errorf("%s: SPOT machine with interruption_probability %v (must be > 0)", m.GetId(), m.GetInterruptionProbability())
		}
		if m.GetCapacityType() == pb.CapacityType_CAPACITY_TYPE_ON_DEMAND && m.GetInterruptionProbability() != 0 {
			t.Errorf("%s: on-demand machine with non-zero interruption_probability %v", m.GetId(), m.GetInterruptionProbability())
		}
	}
}

// A full lifecycle through providerkit drives the EC2 fake: Create launches an
// instance (host appears), Configure binds it, Drain unbinds, Delete
// terminates it (slot returns to Speculative).
func TestFullLifecycle_DrivesEC2(t *testing.T) {
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
		t.Fatal("Idle machine has no host (RunInstances result not attached)")
	}
	if got := len(fake.instances); got != 1 {
		t.Fatalf("EC2 fake has %d instances after Create, want 1", got)
	}

	if _, err := s.Configure(ctx, &pb.ConfigureRequest{MachineId: id, ClusterId: "c1", BootstrapBlob: []byte("join")}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED)

	if _, err := s.Drain(ctx, &pb.DrainRequest{MachineId: id, GracePeriodSeconds: 5}); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)

	if _, err := s.Delete(ctx, &pb.DeleteRequest{MachineId: id}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	m = waitState(t, s, id, pb.MachineState_MACHINE_STATE_SPECULATIVE)
	if m.GetHost().GetRef() != "" {
		t.Error("Speculative machine still has a host after Delete")
	}
	if got := len(fake.instances); got != 0 {
		t.Errorf("EC2 fake has %d instances after Delete, want 0 (TerminateInstances)", got)
	}
}

// Describe must reconcile a running managed instance back to its offering slot
// as Idle (recovery from EC2 tags when there is no persisted store).
func TestDescribe_ReconcilesRunningInstance(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	// Pick a slot id and launch an instance tagged with it (as CreateInstance would).
	slots := b.speculativeSlots()
	slotID := slots[0].ID
	if _, err := fake.RunInstance(ctx, runSpec{MachineID: slotID, InstanceType: slots[0].InstanceType, Zone: slots[0].Zone}); err != nil {
		t.Fatalf("seed instance: %v", err)
	}

	got, err := b.Describe(ctx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	var found *providerkit.Instance
	for i := range got {
		if got[i].ID == slotID {
			found = &got[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("Describe did not return slot %s", slotID)
	}
	if found.State != providerkit.StateIdle {
		t.Errorf("backed slot state = %v, want Idle", found.State)
	}
	if found.Host.Ref == "" {
		t.Error("backed slot has no host")
	}
}

func TestInterruption_NeverZeroForSpot(t *testing.T) {
	in := newInterruption()
	// Known spot type → advisor bucket, > 0.
	if p := in.probability("m1", "c7g.xlarge", providerkit.CapacitySpot); p <= 0 {
		t.Errorf("known spot type probability = %v, want > 0", p)
	}
	// Unknown spot type → middle bucket, still > 0.
	if p := in.probability("m2", "totally-unknown.type", providerkit.CapacitySpot); p <= 0 {
		t.Errorf("unknown spot type probability = %v, want > 0 (never 0 for spot)", p)
	}
	// Non-spot → 0.
	if p := in.probability("m3", "m6i.large", providerkit.CapacityOnDemand); p != 0 {
		t.Errorf("on-demand probability = %v, want 0", p)
	}
	// Observed escalation raises it.
	in.markWarning("m1", 0.95)
	if p := in.probability("m1", "c7g.xlarge", providerkit.CapacitySpot); p < 0.95 {
		t.Errorf("after warning, probability = %v, want >= 0.95", p)
	}
}

// A managed instance still tagged with a slot's machine id must keep that slot
// occupied in ANY non-terminated state — otherwise the slot is re-seeded
// Speculative and Create launches a duplicate instance sharing one machine id.
func TestDescribe_StoppedTaggedInstanceKeepsItsSlot(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	slots := b.speculativeSlots()
	slotID := slots[0].ID

	fake.mu.Lock()
	fake.instances["i-stopped"] = &ec2Instance{
		InstanceID: "i-stopped", MachineID: slotID,
		InstanceType: slots[0].InstanceType, Zone: slots[0].Zone, Running: false, // stopped
	}
	fake.mu.Unlock()

	got, err := b.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for _, in := range got {
		if in.ID != slotID {
			continue
		}
		if in.State == providerkit.StateSpeculative {
			t.Error("stopped tagged instance's slot was re-seeded Speculative (duplicate-launch risk)")
		}
		if in.Host.Ref != "i-stopped" {
			t.Errorf("slot host = %q, want i-stopped", in.Host.Ref)
		}
	}
}

// An orphan's capacity_type comes from its bigfleet:capacity tag, so a Reserved
// instance is never mislabeled ON_DEMAND (which would make it Delete-eligible).
func TestInstanceToIdle_CapacityFromTag(t *testing.T) {
	b, _ := newTestBackend(t, 1)
	got := b.instanceToIdle("m-orphan", ec2Instance{
		InstanceID: "i-r", InstanceType: "m6i.large", Zone: "us-east-1a", Capacity: "reserved",
	})
	if got.CapacityType != providerkit.CapacityReserved {
		t.Errorf("orphan capacity = %v, want Reserved (from tag)", got.CapacityType)
	}
	// Untagged spot falls back to the spot lifecycle.
	got2 := b.instanceToIdle("m-orphan2", ec2Instance{
		InstanceID: "i-s", InstanceType: "c7g.xlarge", Zone: "us-east-1a", Spot: true,
	})
	if got2.CapacityType != providerkit.CapacitySpot {
		t.Errorf("untagged spot orphan capacity = %v, want Spot", got2.CapacityType)
	}
}

func TestNewAWSBackend_RejectsZonelessOffering(t *testing.T) {
	fake := newEC2Fake()
	offs := []offering{{InstanceType: "m6i.large", Zone: "", Capacity: "on_demand", Count: 1}}
	_, err := newAWSBackend("aws-test", "us-east-1", fake, offs, newPricing("us-east-1", fake, quietLogger()), newInterruption(), nil, quietLogger())
	if err == nil {
		t.Fatal("expected error for a zoneless offering (provider is multi-zone)")
	}
}

func TestInterruption_MarkWarningClampsToOne(t *testing.T) {
	in := newInterruption()
	in.markWarning("m1", 5.0) // out-of-range
	if p := in.probability("m1", "c7g.xlarge", providerkit.CapacitySpot); p > 1.0 {
		t.Errorf("probability = %v, want <= 1.0 after clamp", p)
	}
}

func TestPricing_OnDemandFromTableSpotFromCache(t *testing.T) {
	fake := newEC2Fake()
	fake.spotUSD = 0.0123
	p := newPricing("us-east-1", fake, quietLogger())
	if got := p.price("m6i.large", "us-east-1a", providerkit.CapacityOnDemand); got != onDemandUSEast1["m6i.large"] {
		t.Errorf("on-demand price = %v, want %v", got, onDemandUSEast1["m6i.large"])
	}
	// Cold cache spot → fallback fraction of on-demand (> 0 for a known type).
	if got := p.price("m6i.large", "us-east-1a", providerkit.CapacitySpot); got <= 0 {
		t.Errorf("cold spot price = %v, want > 0 fallback", got)
	}
	// After refresh → the fetched value.
	p.refresh(context.Background(), []spotPair{{instanceType: "m6i.large", zone: "us-east-1a"}})
	if got := p.price("m6i.large", "us-east-1a", providerkit.CapacitySpot); got != 0.0123 {
		t.Errorf("refreshed spot price = %v, want 0.0123", got)
	}
}
