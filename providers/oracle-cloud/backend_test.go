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

func testPricing(t *testing.T) *pricing {
	t.Helper()
	pr, err := newPricing("")
	if err != nil {
		t.Fatalf("newPricing: %v", err)
	}
	return pr
}

func newTestBackend(t *testing.T, seedCount int) (*ociBackend, *ociFake) {
	t.Helper()
	fake := newOCIFake()
	logger := quietLogger()
	offs := defaultOfferings(seedCount, "Uocm:PHX-AD-1", "Uocm:PHX-AD-2")
	b, err := newOCIBackend("oci-test", fake, offs, testPricing(t), newInterruption(), nil, logger)
	if err != nil {
		t.Fatalf("newOCIBackend: %v", err)
	}
	return b, fake
}

func newTestServer(t *testing.T, b *ociBackend) *providerkit.Server {
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

// The default offerings must seed valid field shape, and — load-bearing for the
// spot certification lane — at least one SPOT machine with a non-zero
// interruption_probability.
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
		ip := m.GetInterruptionProbability()
		if ip < 0 || ip > 1 {
			t.Errorf("%s: interruption_probability %v out of [0,1]", m.GetId(), ip)
		}
		if m.GetCapacityType() == pb.CapacityType_CAPACITY_TYPE_SPOT {
			sawSpot = true
			if !(ip > 0) {
				t.Errorf("%s: SPOT machine reports interruption_probability %v (must be > 0)", m.GetId(), ip)
			}
			if m.GetPricePerHour() <= 0 {
				t.Errorf("%s: SPOT price_per_hour = %v, want > 0", m.GetId(), m.GetPricePerHour())
			}
		}
	}
	if !sawSpot {
		t.Fatal("default offerings seeded no SPOT machines; the spot certification lane would not fire")
	}
}

// A full lifecycle through providerkit drives the OCI fake: Create launches an
// instance (host appears), Configure binds it, Drain unbinds, Delete terminates
// it (slot returns to Speculative).
func TestFullLifecycle_DrivesOCI(t *testing.T) {
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
		t.Fatal("Idle machine has no host (LaunchInstance result not attached)")
	}
	if got := len(fake.instances); got != 1 {
		t.Fatalf("OCI fake has %d instances after Create, want 1", got)
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
		t.Errorf("OCI fake has %d instances after Delete, want 0", got)
	}
}

// Describe must reconcile a running managed instance back to its offering slot as
// Idle (recovery from OCI freeform tags when there is no persisted store).
func TestDescribe_ReconcilesRunningInstance(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slots := b.speculativeSlots()
	slot := slots[0]
	if _, err := fake.LaunchInstance(ctx, launchSpec{MachineID: slot.ID, Shape: slot.InstanceType, AvailabilityDomain: slot.Zone, OCPUs: 2, MemoryGB: 16}); err != nil {
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

// Create must be idempotent at the substrate level: a retried LaunchInstance with
// the same operation id returns the same instance, never a duplicate.
func TestLaunchInstance_IdempotentOnToken(t *testing.T) {
	fake := newOCIFake()
	ctx := context.Background()
	spec := launchSpec{MachineID: "m1", Shape: "VM.Standard.E5.Flex", AvailabilityDomain: "Uocm:PHX-AD-1", OCPUs: 2, MemoryGB: 16, IdempotencyToken: "op-1"}
	a, err := fake.LaunchInstance(ctx, spec)
	if err != nil {
		t.Fatalf("launch a: %v", err)
	}
	b, err := fake.LaunchInstance(ctx, spec)
	if err != nil {
		t.Fatalf("launch b: %v", err)
	}
	if a.InstanceID != b.InstanceID {
		t.Errorf("idempotent launch returned distinct ids %s vs %s", a.InstanceID, b.InstanceID)
	}
	if len(fake.instances) != 1 {
		t.Errorf("idempotent launch created %d instances, want 1", len(fake.instances))
	}
}

// An orphan / offering-shrank instance rebinds with the per-replica resources of
// a still-configured offering for its shape, so it keeps matching its demand
// profile; a preemptible instance rebinds SPOT with a non-zero probability.
func TestInstanceToIdle_RecoversFields(t *testing.T) {
	b, _ := newTestBackend(t, 4)
	got := b.instanceToIdle("orphan-1", ociInstance{
		InstanceID: "ocid1.instance.oc1..x", Shape: "VM.Standard.E5.Flex",
		AvailabilityDomain: "Uocm:PHX-AD-1", OCPUs: 2, MemoryGB: 16, Preemptible: true,
	})
	if got.CapacityType != providerkit.CapacitySpot {
		t.Errorf("preemptible instance rebound as %v, want Spot", got.CapacityType)
	}
	if !(got.InterruptionProbability > 0) {
		t.Errorf("rebound SPOT machine has interruption_probability %v, want > 0", got.InterruptionProbability)
	}
	if got.Resources["cpu"] == "" {
		t.Errorf("rebound machine has no resources; want the offering's per-replica shape, got %v", got.Resources)
	}
	// A shape covered by no offering yields nil resources (FileStore is recovery).
	if r := b.resourcesForShape("VM.Unoffered.Flex", "Uocm:PHX-AD-1"); r != nil {
		t.Errorf("unoffered shape resources = %v, want nil", r)
	}
}

// Capacity is mapped by the declared capacity_type, not the shape prefix: a BM.*
// shape declared on_demand is ON_DEMAND (priced), while capacity_type=bare_metal
// is BARE_METAL (held, price 0).
func TestOffering_BareMetalShape(t *testing.T) {
	// Capacity is mapped by the DECLARED capacity_type, not the shape prefix: a
	// BM.* shape declared on_demand is genuine (hourly-billed) on-demand capacity.
	if ct, err := (offering{Shape: "BM.Standard.E5.192", Capacity: "on_demand"}).capacityType(); err != nil || ct != providerkit.CapacityOnDemand {
		t.Fatalf("BM shape on_demand: got (%v, %v), want (OnDemand, nil)", ct, err)
	}
	// An explicit bare_metal declaration is BARE_METAL (held, price 0).
	if ct, err := (offering{Shape: "BM.Standard.E5.192", Capacity: "bare_metal"}).capacityType(); err != nil || ct != providerkit.CapacityBareMetal {
		t.Fatalf("BM shape bare_metal: got (%v, %v), want (BareMetal, nil)", ct, err)
	}
	pr := testPricing(t)
	if p := pr.price("BM.Standard.E5.192", 0, 0, providerkit.CapacityBareMetal); p != 0 {
		t.Errorf("bare-metal price = %v, want 0", p)
	}
}

// instanceToIdle maps capacity from the recorded bigfleet-capacity tag, not the
// shape prefix: a BM.* instance launched on-demand recovers as ON_DEMAND.
func TestInstanceToIdle_CapacityFromTag(t *testing.T) {
	b, _ := newTestBackend(t, 4)
	got := b.instanceToIdle("m-bm", ociInstance{
		InstanceID: "ocid1.instance.oc1..bm", Shape: "BM.Standard.E5.192",
		AvailabilityDomain: "Uocm:PHX-AD-1", Capacity: "on_demand",
	})
	if got.CapacityType != providerkit.CapacityOnDemand {
		t.Errorf("BM instance tagged on_demand recovered as %v, want OnDemand", got.CapacityType)
	}
	// An explicit bare_metal tag recovers as BARE_METAL at price 0.
	bm := b.instanceToIdle("m-bm2", ociInstance{
		InstanceID: "ocid1.instance.oc1..bm2", Shape: "BM.Standard.E5.192",
		AvailabilityDomain: "Uocm:PHX-AD-1", Capacity: "bare_metal",
	})
	if bm.CapacityType != providerkit.CapacityBareMetal || bm.PricePerHour != 0 {
		t.Errorf("BM instance tagged bare_metal recovered as (%v, $%v), want (BareMetal, 0)", bm.CapacityType, bm.PricePerHour)
	}
}

// A stopped/migrating tagged instance still OWNS its slot (surfaced Idle with its
// host), so it stays reapable via Delete and Create can't launch a duplicate —
// rather than being dropped (leaked) and the slot re-seeded Speculative.
func TestDescribe_StoppedInstanceOwnsSlot(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	slot := b.speculativeSlots()[0]
	fake.instances["stopped"] = &ociInstance{
		InstanceID: "stopped", MachineID: slot.ID, Shape: slot.InstanceType,
		AvailabilityDomain: slot.Zone, Capacity: "on_demand", Running: false,
	}
	got, err := b.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	var found *providerkit.Instance
	for i := range got {
		if got[i].ID == slot.ID {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("slot %s missing from Describe", slot.ID)
	}
	if found.State != providerkit.StateIdle || found.Host.Ref != "stopped" {
		t.Errorf("stopped tagged instance = (%v, host %q), want (Idle, stopped) — owns its slot, reapable", found.State, found.Host.Ref)
	}
}

// An orphan/recovered instance with no availability domain is skipped, not fatally
// crashed through the RequireZone seed validation.
func TestDescribe_SkipsADlessOrphan(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	fake.instances["noad"] = &ociInstance{InstanceID: "noad", Shape: "VM.Standard.E5.Flex", Running: true} // untagged, no AD
	got, err := b.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for _, in := range got {
		if in.Host.Ref == "noad" {
			t.Errorf("AD-less orphan should be skipped, but was surfaced: %+v", in)
		}
	}
}

// Duplicate (shape, AD, capacity) offerings are rejected up front (they would
// otherwise generate colliding slot IDs and crash the kit's seed).
func TestNewOCIBackend_RejectsDuplicateOfferings(t *testing.T) {
	dup := []offering{
		{Shape: "VM.Standard.E5.Flex", AvailabilityDomain: "AD-1", Capacity: "on_demand", Count: 1, OCPUs: 2, MemoryGB: 16},
		{Shape: "VM.Standard.E5.Flex", AvailabilityDomain: "AD-1", Capacity: "on_demand", Count: 1, OCPUs: 4, MemoryGB: 32},
	}
	if _, err := newOCIBackend("oci-test", newOCIFake(), dup, testPricing(t), newInterruption(), nil, quietLogger()); err == nil {
		t.Fatal("expected duplicate (shape,AD,capacity) offerings to be rejected")
	}
}

func TestOffering_CapacityType(t *testing.T) {
	for _, ok := range []string{"on_demand", "on-demand", "ondemand", ""} {
		if ct, err := (offering{Shape: "VM.Standard.E5.Flex", Capacity: ok}).capacityType(); err != nil || ct != providerkit.CapacityOnDemand {
			t.Errorf("capacity_type %q: got (%v, %v), want (OnDemand, nil)", ok, ct, err)
		}
	}
	if ct, err := (offering{Shape: "VM.Standard.E5.Flex", Capacity: "spot"}).capacityType(); err != nil || ct != providerkit.CapacitySpot {
		t.Errorf("capacity_type spot: got (%v, %v), want (Spot, nil)", ct, err)
	}
	for _, bad := range []string{"reserved", "nonsense"} {
		if _, err := (offering{Shape: "VM.Standard.E5.Flex", Capacity: bad}).capacityType(); err == nil {
			t.Errorf("expected capacity_type %q to be rejected", bad)
		}
	}
}
