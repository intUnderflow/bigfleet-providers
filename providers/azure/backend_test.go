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

func newTestBackend(t *testing.T, seedCount int) (*azureBackend, *azureFake) {
	t.Helper()
	fake := newAzureFake()
	logger := quietLogger()
	offs := defaultOfferings(seedCount, "eastus-1", "eastus-2")
	b, err := newAzureBackend("azure-test", "eastus", fake, offs, newPricing("eastus", fake, logger), newInterruption(), nil, logger)
	if err != nil {
		t.Fatalf("newAzureBackend: %v", err)
	}
	return b, fake
}

func newTestServer(t *testing.T, b *azureBackend) *providerkit.Server {
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

// The default offerings must seed valid field shape. Azure offers both
// pay-as-you-go and Spot, so SPOT slots must carry a real (> 0)
// interruption_probability and ON_DEMAND slots must carry 0.
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
			if m.GetInterruptionProbability() <= 0 {
				t.Errorf("%s: SPOT machine with interruption_probability %v, want > 0", m.GetId(), m.GetInterruptionProbability())
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
		t.Error("default offerings seeded no SPOT slots; the spot path is untested")
	}
}

// A full lifecycle through providerkit drives the Azure fake: Create provisions
// a VM (host appears), Configure binds it, Drain unbinds, Delete deletes it (slot
// returns to Speculative).
func TestFullLifecycle_DrivesAzure(t *testing.T) {
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
		t.Fatal("Idle machine has no host (CreateVM result not attached)")
	}
	if got := len(fake.vms); got != 1 {
		t.Fatalf("Azure fake has %d vms after Create, want 1", got)
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
	if got := len(fake.vms); got != 0 {
		t.Errorf("Azure fake has %d vms after Delete, want 0", got)
	}
}

// Describe must reconcile a running managed VM back to its offering slot as Idle
// (recovery from Azure tags when there is no persisted store).
func TestDescribe_ReconcilesRunningVM(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slots := b.speculativeSlots()
	slot := slots[0]
	if _, err := fake.CreateVM(ctx, vmSpec{MachineID: slot.ID, VMSize: slot.InstanceType, Zone: slot.Zone}); err != nil {
		t.Fatalf("seed vm: %v", err)
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

// A tagged VM that is no longer running (deleting/evicted) must release its slot:
// Describe returns the slot as Speculative with no host, not Idle pointing at a
// vanishing resource.
func TestDescribe_DeletingVMReleasesSlot(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slot := b.speculativeSlots()[0]
	vm, err := fake.CreateVM(ctx, vmSpec{MachineID: slot.ID, VMSize: slot.InstanceType, Zone: slot.Zone})
	if err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	// Simulate the VM entering its Deleting state.
	fake.vms[vm.ResourceID].Running = false
	fake.vms[vm.ResourceID].Deleting = true

	got, err := b.Describe(ctx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for i := range got {
		if got[i].ID == slot.ID {
			if got[i].State == providerkit.StateIdle {
				t.Errorf("deleting VM's slot = Idle, want Speculative (slot should be released)")
			}
			if got[i].Host.Ref != "" {
				t.Errorf("deleting VM's slot has host ref %q, want none", got[i].Host.Ref)
			}
			return
		}
	}
	t.Fatalf("Describe did not return slot %s", slot.ID)
}

// A tagged VM that is merely stopped/deallocated (not deleting) must KEEP owning
// its slot (Idle with its host), so it is recovered and powered on rather than
// dropped — dropping it would re-seed Speculative and leak the stopped VM.
func TestDescribe_StoppedVMKeepsSlot(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slot := b.speculativeSlots()[0]
	vm, err := fake.CreateVM(ctx, vmSpec{MachineID: slot.ID, VMSize: slot.InstanceType, Zone: slot.Zone})
	if err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	fake.vms[vm.ResourceID].Running = false // stopped/deallocated, NOT deleting

	got, err := b.Describe(ctx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for i := range got {
		if got[i].ID == slot.ID {
			if got[i].State != providerkit.StateIdle {
				t.Errorf("stopped VM's slot = %v, want Idle (recoverable, must keep its slot)", got[i].State)
			}
			if got[i].Host.Ref != vm.ResourceID {
				t.Errorf("stopped VM's slot host = %q, want %q", got[i].Host.Ref, vm.ResourceID)
			}
			return
		}
	}
	t.Fatalf("Describe did not return slot %s", slot.ID)
}

// ConfigureInstance must power on a stopped host before delivering the bootstrap,
// so the CustomScript extension can run (it can't on a deallocated VM).
func TestConfigure_PowersOnStoppedHost(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slot := b.speculativeSlots()[0]
	vm, err := fake.CreateVM(ctx, vmSpec{MachineID: slot.ID, VMSize: slot.InstanceType, Zone: slot.Zone})
	if err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	fake.vms[vm.ResourceID].Running = false

	err = b.ConfigureInstance(ctx, providerkit.ConfigureInstanceRequest{
		Machine:       providerkit.Machine{ID: slot.ID, InstanceType: slot.InstanceType, Zone: slot.Zone, Host: providerkit.HostRef{Provider: b.providerName, Ref: vm.ResourceID}},
		ClusterID:     "cluster-1",
		BootstrapBlob: []byte("blob"),
	})
	if err != nil {
		t.Fatalf("ConfigureInstance: %v", err)
	}
	if !fake.vms[vm.ResourceID].Running {
		t.Error("ConfigureInstance did not power on the stopped host")
	}
}

// newAzureBackend must reject an offering whose VM size has no pinned on-demand
// price, rather than let it publish PricePerHour=0 (read as "free").
func TestNewAzureBackend_RejectsUnpricedOffering(t *testing.T) {
	fake := newAzureFake()
	logger := quietLogger()
	offs := []offering{
		{VMSize: "Standard_NOPRICE_v1", Zone: "eastus-1", Capacity: "on_demand", Count: 1, Resources: map[string]string{"cpu": "1", "memory": "2Gi"}},
	}
	_, err := newAzureBackend("azure-test", "eastus", fake, offs, newPricing("eastus", fake, logger), newInterruption(), nil, logger)
	if err == nil {
		t.Fatal("expected newAzureBackend to reject an offering with no pinned price")
	}
}

// A zoneless orphan VM must be skipped during Describe (not emitted as a
// zoneless Idle that would fatally fail the RequireZone seed validation).
func TestDescribe_SkipsZonelessOrphan(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	// An untagged (orphan) running VM with no zone.
	if _, err := fake.CreateVM(ctx, vmSpec{VMSize: "Standard_D4s_v5"}); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	got, err := b.Describe(ctx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for _, inst := range got {
		if inst.Zone == "" {
			t.Errorf("Describe emitted a zoneless instance %q — would crash RequireZone seed", inst.ID)
		}
	}
}

// Two running VMs sharing a machine id (e.g. a re-driven Create after a partial
// failure) must not silently leak: one owns the slot (Idle) and the other is
// surfaced as an orphan under its resource id so it stays tracked and reclaimable.
func TestDescribe_DuplicateMachineIDSurfacesOrphan(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slot := b.speculativeSlots()[0]
	vm1, err := fake.CreateVM(ctx, vmSpec{MachineID: slot.ID, VMSize: slot.InstanceType, Zone: slot.Zone, IdempotencyToken: "op-1"})
	if err != nil {
		t.Fatalf("seed vm1: %v", err)
	}
	vm2, err := fake.CreateVM(ctx, vmSpec{MachineID: slot.ID, VMSize: slot.InstanceType, Zone: slot.Zone, IdempotencyToken: "op-2"})
	if err != nil {
		t.Fatalf("seed vm2: %v", err)
	}
	owner, loser := vm1.ResourceID, vm2.ResourceID
	if loser < owner { // deterministic owner = smallest resource id
		owner, loser = loser, owner
	}

	got, err := b.Describe(ctx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	var slotIdle bool
	instanceIDs := map[string]bool{}
	for _, inst := range got {
		instanceIDs[inst.ID] = true
		if inst.ID == slot.ID {
			if inst.State != providerkit.StateIdle {
				t.Errorf("slot state = %v, want Idle", inst.State)
			}
			if inst.Host.Ref != owner {
				t.Errorf("slot host = %q, want deterministic owner %q", inst.Host.Ref, owner)
			}
			slotIdle = true
		}
	}
	if !slotIdle {
		t.Fatalf("slot %s not present as Idle", slot.ID)
	}
	if !instanceIDs[loser] {
		t.Errorf("duplicate VM %q was not surfaced as an orphan — it would leak", loser)
	}
}

// Create must be idempotent at the substrate level: a retried CreateVM with the
// same operation id returns the same VM, never a duplicate.
func TestCreateVM_IdempotentOnToken(t *testing.T) {
	fake := newAzureFake()
	ctx := context.Background()
	spec := vmSpec{MachineID: "m1", VMSize: "Standard_D4s_v5", Zone: "eastus-1", IdempotencyToken: "op-1"}
	a, err := fake.CreateVM(ctx, spec)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := fake.CreateVM(ctx, spec)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if a.ResourceID != b.ResourceID {
		t.Errorf("idempotent create returned distinct ids %s vs %s", a.ResourceID, b.ResourceID)
	}
	if len(fake.vms) != 1 {
		t.Errorf("idempotent create provisioned %d vms, want 1", len(fake.vms))
	}
}

// An orphan / offering-shrank VM rebinds with the per-replica resources of a
// still-configured offering for its size, so it keeps matching its demand profile.
func TestVMToIdle_RecoversResources(t *testing.T) {
	b, _ := newTestBackend(t, 4)
	got := b.vmToIdle("orphan-1", vmInstance{ResourceID: "/x/9", VMSize: "Standard_D4s_v5", Zone: "eastus-1"})
	if got.Resources["cpu"] == "" {
		t.Errorf("rebound machine has no resources; want the offering's per-replica shape, got %v", got.Resources)
	}
	// A size covered by no offering yields nil (FileStore is the recovery path).
	if r := b.resourcesForSize("Standard_unoffered", "eastus-1"); r != nil {
		t.Errorf("unoffered size resources = %v, want nil", r)
	}
}

// A Spot eviction notice must raise the published interruption_probability above
// the forecast, and clearing it (on Delete) must drop the escalation.
func TestInterruption_ObservedEscalation(t *testing.T) {
	in := newInterruption()
	const size = "Standard_F8s_v2"
	forecast := in.probability("m1", size, providerkit.CapacitySpot)
	if forecast <= 0 || forecast >= 1 {
		t.Fatalf("spot forecast = %v, want in (0,1)", forecast)
	}
	in.markWarning("m1", 0) // a Preempt notice → ~1.0
	if got := in.probability("m1", size, providerkit.CapacitySpot); got <= forecast {
		t.Errorf("observed probability %v did not exceed forecast %v", got, forecast)
	}
	in.clear("m1")
	if got := in.probability("m1", size, providerkit.CapacitySpot); got != forecast {
		t.Errorf("after clear probability = %v, want forecast %v", got, forecast)
	}
}

// A non-spot machine always forecasts exactly 0; a spot machine never does.
func TestInterruption_NonSpotIsZeroSpotNonZero(t *testing.T) {
	in := newInterruption()
	if got := in.probability("m", "Standard_D4s_v5", providerkit.CapacityOnDemand); got != 0 {
		t.Errorf("on-demand interruption_probability = %v, want 0", got)
	}
	// An unknown spot size still falls back to a non-zero band.
	if got := in.probability("m", "Standard_unknown_size", providerkit.CapacitySpot); got <= 0 {
		t.Errorf("unknown spot size interruption_probability = %v, want > 0", got)
	}
}

func TestOffering_CapacityType(t *testing.T) {
	for _, ok := range []string{"on_demand", "on-demand", "ondemand", ""} {
		if ct, err := (offering{Capacity: ok}).capacityType(); err != nil || ct != providerkit.CapacityOnDemand {
			t.Errorf("capacity_type %q: got (%v, %v), want (OnDemand, nil)", ok, ct, err)
		}
	}
	if ct, err := (offering{Capacity: "spot"}).capacityType(); err != nil || ct != providerkit.CapacitySpot {
		t.Errorf("capacity_type spot: got (%v, %v), want (Spot, nil)", ct, err)
	}
	if ct, err := (offering{Capacity: "reserved"}).capacityType(); err != nil || ct != providerkit.CapacityReserved {
		t.Errorf("capacity_type reserved: got (%v, %v), want (Reserved, nil)", ct, err)
	}
	// Azure VMs are always billed: bare_metal would mis-declare capacity and force
	// price 0, so it must be rejected.
	for _, bad := range []string{"bare_metal", "bare-metal", "metal", "nonsense"} {
		if _, err := (offering{VMSize: "Standard_D4s_v5", Zone: "eastus-1", Capacity: bad}).capacityType(); err == nil {
			t.Errorf("expected capacity_type %q to be rejected", bad)
		}
	}
}

// zoneNumber maps a BigFleet zone back to the bare Azure zone the SDK wants.
func TestZoneNumber(t *testing.T) {
	cases := map[string]string{"eastus-1": "1", "westeurope-3": "3", "2": "2", "": ""}
	for in, want := range cases {
		if got := zoneNumber(in); got != want {
			t.Errorf("zoneNumber(%q) = %q, want %q", in, got, want)
		}
	}
}

// vmName must produce a valid, deterministic, hyphen-safe Azure VM name.
func TestVMName_DeterministicAndValid(t *testing.T) {
	a := vmName("azure-eastus/Spot/Standard_F8s_v2/eastus-1/000", "op-42")
	b := vmName("azure-eastus/Spot/Standard_F8s_v2/eastus-1/000", "op-42")
	if a != b {
		t.Errorf("vmName not deterministic: %q vs %q", a, b)
	}
	if len(a) == 0 || len(a) > 64 {
		t.Errorf("vmName %q length %d out of range", a, len(a))
	}
	for _, r := range a {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			t.Errorf("vmName %q contains invalid rune %q", a, r)
		}
	}
}
