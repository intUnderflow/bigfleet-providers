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

func newTestBackend(t *testing.T, seedCount int) (*digitaloceanBackend, *doFake) {
	t.Helper()
	fake := newDOFake()
	logger := quietLogger()
	offs := defaultOfferings(seedCount, "nyc3", "sfo3")
	b, err := newDigitaloceanBackend("digitalocean-test", "ubuntu-24-04-x64", fake, offs, newPricing(fake, logger), nil, logger)
	if err != nil {
		t.Fatalf("newDigitaloceanBackend: %v", err)
	}
	return b, fake
}

func newTestServer(t *testing.T, b *digitaloceanBackend) *providerkit.Server {
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

// The default offerings must seed valid field shape. DigitalOcean is on-demand
// only, so every slot is ON_DEMAND with interruption_probability == 0.
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
		if m.GetCapacityType() != pb.CapacityType_CAPACITY_TYPE_ON_DEMAND {
			t.Errorf("%s: capacity_type = %v, want ON_DEMAND", m.GetId(), m.GetCapacityType())
		}
		if m.GetInterruptionProbability() != 0 {
			t.Errorf("%s: on-demand machine with non-zero interruption_probability %v", m.GetId(), m.GetInterruptionProbability())
		}
		if m.GetAllocatable() == nil || len(m.GetAllocatable().GetResources()) == 0 {
			t.Errorf("%s: missing allocatable", m.GetId())
		}
		if m.GetPricePerHour() <= 0 {
			t.Errorf("%s: price_per_hour = %v, want > 0", m.GetId(), m.GetPricePerHour())
		}
		// resources (per-replica) must differ from allocatable (hardware) so the
		// shard's density math is not forced to 1.
		if eq := mapsEqual(m.GetResources().GetResources(), m.GetAllocatable().GetResources()); eq {
			t.Errorf("%s: resources == allocatable (%v); density would collapse to 1", m.GetId(), m.GetResources().GetResources())
		}
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A full lifecycle through providerkit drives the DigitalOcean fake: Create
// launches a Droplet (host appears), Configure binds it, Drain unbinds, Delete
// deletes it (slot returns to Speculative).
func TestFullLifecycle_DrivesDigitalOcean(t *testing.T) {
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
		t.Fatal("Idle machine has no host (CreateDroplet result not attached)")
	}
	if got := len(fake.droplets); got != 1 {
		t.Fatalf("DigitalOcean fake has %d droplets after Create, want 1", got)
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
	if got := len(fake.droplets); got != 0 {
		t.Errorf("DigitalOcean fake has %d droplets after Delete, want 0", got)
	}
}

// Describe must reconcile a running managed Droplet back to its offering slot as
// Idle (recovery from substrate when there is no persisted store).
func TestDescribe_ReconcilesRunningDroplet(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slots := b.speculativeSlots()
	slot := slots[0]
	if _, err := fake.CreateDroplet(ctx, dropletSpec{MachineID: slot.ID, Size: slot.InstanceType, Region: slot.Zone}); err != nil {
		t.Fatalf("seed droplet: %v", err)
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

// Create must be idempotent at the substrate level: a retried CreateDroplet with
// the same operation id returns the same Droplet, never a duplicate.
func TestCreateDroplet_IdempotentOnToken(t *testing.T) {
	fake := newDOFake()
	ctx := context.Background()
	spec := dropletSpec{MachineID: "m1", Size: "s-2vcpu-4gb", Region: "nyc3", IdempotencyToken: "op-1"}
	a, err := fake.CreateDroplet(ctx, spec)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := fake.CreateDroplet(ctx, spec)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if a.DropletID != b.DropletID {
		t.Errorf("idempotent create returned distinct ids %s vs %s", a.DropletID, b.DropletID)
	}
	if len(fake.droplets) != 1 {
		t.Errorf("idempotent create launched %d droplets, want 1", len(fake.droplets))
	}
}

// An orphan / offering-shrank Droplet rebinds with the per-replica resources of a
// still-configured offering for its size, so it keeps matching its demand profile.
func TestDropletToIdle_RecoversResources(t *testing.T) {
	b, _ := newTestBackend(t, 4)
	got := b.dropletToIdle("orphan-1", dropletInstance{DropletID: "9", Size: "s-2vcpu-4gb", Region: "nyc3"})
	if got.Resources["cpu"] == "" {
		t.Errorf("rebound machine has no resources; want the offering's per-replica shape, got %v", got.Resources)
	}
	// A size covered by no offering yields nil (FileStore is the recovery path).
	if r := b.resourcesForSize("s-99vcpu-unoffered", "nyc3"); r != nil {
		t.Errorf("unoffered size resources = %v, want nil", r)
	}
}

func TestOffering_CapacityType(t *testing.T) {
	// Only on-demand is a real DigitalOcean Droplet substrate; everything else is
	// rejected so the provider can never mis-declare capacity_type.
	for _, ok := range []string{"on_demand", "on-demand", "ondemand", ""} {
		if ct, err := (offering{Capacity: ok}).capacityType(); err != nil || ct != providerkit.CapacityOnDemand {
			t.Errorf("capacity_type %q: got (%v, %v), want (OnDemand, nil)", ok, ct, err)
		}
	}
	for _, bad := range []string{"spot", "reserved", "bare_metal", "bare-metal", "metal", "nonsense"} {
		if _, err := (offering{Size: "s-2vcpu-4gb", Region: "nyc3", Capacity: bad}).capacityType(); err == nil {
			t.Errorf("expected capacity_type %q to be rejected (DigitalOcean is on-demand only)", bad)
		}
	}
}

// price falls back to the pinned table on a cold cache, and the resolver renders
// allocatable from the size catalogue.
func TestPricingAndAllocatable(t *testing.T) {
	logger := quietLogger()
	fake := newDOFake()
	pr := newPricing(fake, logger)
	if got := pr.price("s-2vcpu-4gb", providerkit.CapacityOnDemand); got <= 0 {
		t.Errorf("cold-cache price for s-2vcpu-4gb = %v, want the pinned fallback > 0", got)
	}
	// An unknown size has no live or pinned price → 0 (and warns once); a later
	// refresh fills it in and clears the warn state.
	if got := pr.price("s-99vcpu-unknown", providerkit.CapacityOnDemand); got != 0 {
		t.Errorf("unknown-size price = %v, want 0", got)
	}
	res := newSizeResolver(fake, logger)
	alloc := res.allocatable("s-4vcpu-8gb")
	if alloc["cpu"] != "4" || alloc["memory"] != "8Gi" {
		t.Errorf("allocatable(s-4vcpu-8gb) = %v, want cpu=4 memory=8Gi", alloc)
	}
}
