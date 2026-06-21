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

// newInstancesBackend builds the cloud (Instances/ON_DEMAND) backend over a fake,
// wrapped as the Deleter-capable cloudBackend exactly as main does.
func newInstancesBackend(t *testing.T, seedCount int) (*cloudBackend, *scwFake) {
	t.Helper()
	fake := newSCWFake()
	logger := quietLogger()
	offs := defaultInstanceOfferings(seedCount, "fr-par-1")
	core, err := newScalewayBackend("scaleway-test", providerkit.CapacityOnDemand, "fr-par-1", "ubuntu_jammy", fake, offs, newPricing(defaultEURtoUSD, fake, logger), nil, logger)
	if err != nil {
		t.Fatalf("newScalewayBackend: %v", err)
	}
	return &cloudBackend{scalewayBackend: core}, fake
}

// newBaremetalBackend builds the Elastic Metal (BARE_METAL) backend over a fake.
// It is the bare *scalewayBackend (no Deleter).
func newBaremetalBackend(t *testing.T, seedCount int) (*scalewayBackend, *scwFake) {
	t.Helper()
	fake := newSCWFake()
	logger := quietLogger()
	offs := defaultBaremetalOfferings(seedCount, "fr-par-1")
	core, err := newScalewayBackend("scaleway-metal-test", providerkit.CapacityBareMetal, "fr-par-1", "ubuntu_jammy", fake, offs, newPricing(defaultEURtoUSD, fake, logger), nil, logger)
	if err != nil {
		t.Fatalf("newScalewayBackend (bare metal): %v", err)
	}
	return core, fake
}

func newTestServer(t *testing.T, b providerkit.Backend) *providerkit.Server {
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

// The default Instances offerings must seed valid field shape: every slot is
// ON_DEMAND with interruption_probability == 0 (Scaleway has no spot market).
func TestDefaultInstanceOfferings_FieldShape(t *testing.T) {
	b, _ := newInstancesBackend(t, 32)
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
			t.Errorf("%s: non-zero interruption_probability %v (Scaleway has no spot market)", m.GetId(), m.GetInterruptionProbability())
		}
		if m.GetAllocatable() == nil || len(m.GetAllocatable().GetResources()) == 0 {
			t.Errorf("%s: missing allocatable", m.GetId())
		}
		if m.GetResources() == nil || len(m.GetResources().GetResources()) == 0 {
			t.Errorf("%s: missing resources", m.GetId())
		}
		if m.GetPricePerHour() <= 0 {
			t.Errorf("%s: price_per_hour = %v, want > 0", m.GetId(), m.GetPricePerHour())
		}
	}
}

// resources (per-replica) and allocatable (per-machine hardware) must be DISTINCT
// — conflating them forces density=1 and breaks the shard's packing math.
func TestResourcesAndAllocatableAreDistinct(t *testing.T) {
	b, _ := newInstancesBackend(t, 4)
	s := newTestServer(t, b)
	resp, _ := s.List(context.Background(), &pb.ListFilter{MaxResults: 1})
	m := resp.GetMachines()[0]
	res := m.GetResources().GetResources()
	alloc := m.GetAllocatable().GetResources()
	if res["cpu"] == alloc["cpu"] && res["memory"] == alloc["memory"] {
		t.Errorf("resources (%v) must not equal allocatable (%v) — that forces density=1", res, alloc)
	}
}

// The default bare-metal offerings seed BARE_METAL slots at price 0 (owned
// hardware).
func TestDefaultBaremetalOfferings_FieldShape(t *testing.T) {
	b, _ := newBaremetalBackend(t, 8)
	s := newTestServer(t, b)
	resp, err := s.List(context.Background(), &pb.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.GetMachines()) != 8 {
		t.Fatalf("seeded %d machines, want 8", len(resp.GetMachines()))
	}
	for _, m := range resp.GetMachines() {
		if m.GetCapacityType() != pb.CapacityType_CAPACITY_TYPE_BARE_METAL {
			t.Errorf("%s: capacity_type = %v, want BARE_METAL", m.GetId(), m.GetCapacityType())
		}
		if m.GetPricePerHour() != 0 {
			t.Errorf("%s: bare-metal price_per_hour = %v, want 0", m.GetId(), m.GetPricePerHour())
		}
	}
}

// A full lifecycle through providerkit drives the cloud fake: Create launches a
// server, Configure binds it, Drain unbinds, Delete deletes it (slot returns to
// Speculative).
func TestFullLifecycle_DrivesInstances(t *testing.T) {
	b, fake := newInstancesBackend(t, 8)
	s := newTestServer(t, b)
	ctx := context.Background()

	resp, _ := s.List(ctx, &pb.ListFilter{States: []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE}, MaxResults: 1})
	id := resp.GetMachines()[0].GetId()

	if _, err := s.Create(ctx, &pb.CreateRequest{MachineId: id}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m := waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)
	if m.GetHost().GetRef() == "" {
		t.Fatal("Idle machine has no host (CreateServer result not attached)")
	}
	if got := len(fake.servers); got != 1 {
		t.Fatalf("fake has %d servers after Create, want 1", got)
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
	if got := len(fake.servers); got != 0 {
		t.Errorf("fake has %d servers after Delete, want 0", got)
	}
}

// The Elastic Metal backend is a free pool: Delete is codes.Unimplemented because
// the bare *scalewayBackend does not implement providerkit.Deleter.
func TestBaremetal_DeleteUnimplemented(t *testing.T) {
	if _, ok := providerkit.Backend(&scalewayBackend{}).(providerkit.Deleter); ok {
		t.Fatal("scalewayBackend must NOT implement Deleter (bare-metal free pool)")
	}
	if _, ok := providerkit.Backend(&cloudBackend{}).(providerkit.Deleter); !ok {
		t.Fatal("cloudBackend MUST implement Deleter (Instances are deletable)")
	}
}

// Describe must reconcile a running managed server back to its offering slot as
// Idle (recovery from Scaleway tags when there is no persisted store).
func TestDescribe_ReconcilesRunningServer(t *testing.T) {
	b, fake := newInstancesBackend(t, 4)
	ctx := context.Background()

	slots := b.speculativeSlots()
	slot := slots[0]
	if _, err := fake.CreateServer(ctx, serverSpec{MachineID: slot.ID, CommercialType: slot.InstanceType, Zone: slot.Zone}); err != nil {
		t.Fatalf("seed server: %v", err)
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

// Create must be idempotent at the substrate level: a retried CreateServer with
// the same operation id returns the same server, never a duplicate.
func TestCreateServer_IdempotentOnToken(t *testing.T) {
	fake := newSCWFake()
	ctx := context.Background()
	spec := serverSpec{MachineID: "m1", CommercialType: "DEV1-S", Zone: "fr-par-1", IdempotencyToken: "op-1"}
	a, err := fake.CreateServer(ctx, spec)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := fake.CreateServer(ctx, spec)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if a.ServerID != b.ServerID {
		t.Errorf("idempotent create returned distinct ids %s vs %s", a.ServerID, b.ServerID)
	}
	if len(fake.servers) != 1 {
		t.Errorf("idempotent create launched %d servers, want 1", len(fake.servers))
	}
}

// An orphan / offering-shrank server rebinds with the per-replica resources of a
// still-configured offering for its type, so it keeps matching its demand profile.
func TestServerToIdle_RecoversResources(t *testing.T) {
	b, _ := newInstancesBackend(t, 4)
	got := b.serverToIdle("orphan-1", serverInstance{ServerID: "srv-9", CommercialType: "DEV1-S", Zone: "fr-par-1"})
	if got.Resources["cpu"] == "" {
		t.Errorf("rebound machine has no resources; want the offering's per-replica shape, got %v", got.Resources)
	}
	if r := b.resourcesForType("GP1-UNOFFERED", "fr-par-1"); r != nil {
		t.Errorf("unoffered type resources = %v, want nil", r)
	}
}

// A create double-provision (two servers tagged with the same machine id) is
// collapsed by Describe to one survivor (the lowest server id), and the extra is
// terminated — closing the duplicate-billing window.
func TestDescribe_ReapsDuplicateMachineID(t *testing.T) {
	b, fake := newInstancesBackend(t, 4)
	ctx := context.Background()

	slot := b.speculativeSlots()[0]
	// Two distinct servers carrying the SAME machine id (distinct idempotency
	// tokens defeat the fake's own create-idempotency, modelling the lost-response
	// double-provision).
	a, _ := fake.CreateServer(ctx, serverSpec{MachineID: slot.ID, CommercialType: slot.InstanceType, Zone: slot.Zone, IdempotencyToken: "op-a"})
	c, _ := fake.CreateServer(ctx, serverSpec{MachineID: slot.ID, CommercialType: slot.InstanceType, Zone: slot.Zone, IdempotencyToken: "op-b"})
	if len(fake.servers) != 2 {
		t.Fatalf("seed: have %d servers, want 2", len(fake.servers))
	}

	got, err := b.Describe(ctx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(fake.servers) != 1 {
		t.Fatalf("reaper left %d servers, want 1 (the duplicate must be terminated)", len(fake.servers))
	}
	// The survivor is the lowest server id, and the slot is reported Idle on it.
	want := a.ServerID
	if c.ServerID < want {
		want = c.ServerID
	}
	if _, ok := fake.servers[want]; !ok {
		t.Errorf("reaper kept the wrong server; want lowest id %s to survive", want)
	}
	var found *providerkit.Instance
	for i := range got {
		if got[i].ID == slot.ID {
			found = &got[i]
			break
		}
	}
	if found == nil || found.State != providerkit.StateIdle || found.Host.Ref != want {
		t.Errorf("slot not reported Idle on the surviving server %s: %+v", want, found)
	}
}

// A host recovered stopped (a Create that powered off, or an out-of-band stop)
// must be powered back on by Configure before the bootstrap is delivered —
// otherwise the on-host agent can never poll and the machine wedges at FAILED
// while storage bills. The fake's ApplyBootstrap rejects a stopped server, so
// this reaches Configured only because ConfigureInstance calls EnsureRunning
// first.
func TestConfigure_PowersOnStoppedHost(t *testing.T) {
	b, fake := newInstancesBackend(t, 4)
	s := newTestServer(t, b)
	ctx := context.Background()

	resp, _ := s.List(ctx, &pb.ListFilter{States: []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE}, MaxResults: 1})
	id := resp.GetMachines()[0].GetId()
	if _, err := s.Create(ctx, &pb.CreateRequest{MachineId: id}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m := waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)

	// Stop the underlying server out from under the kit: a recovered-stopped Idle
	// host. Without the power-on in Configure the fake would reject the bootstrap.
	fake.stop(m.GetHost().GetRef())

	if _, err := s.Configure(ctx, &pb.ConfigureRequest{MachineId: id, ClusterId: "c1", BootstrapBlob: []byte("join")}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED)
}

// A managed boot volume orphaned by an out-of-band server deletion (the server
// vanishes but its volume is left behind, still billing) is swept by
// ReapOrphanVolumes on the next Describe.
func TestDescribe_ReapsOrphanVolumes(t *testing.T) {
	b, fake := newInstancesBackend(t, 4)
	ctx := context.Background()

	slot := b.speculativeSlots()[0]
	srv, err := fake.CreateServer(ctx, serverSpec{MachineID: slot.ID, CommercialType: slot.InstanceType, Zone: slot.Zone})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}
	if len(fake.volumes) != 1 {
		t.Fatalf("expected 1 managed volume after create, got %d", len(fake.volumes))
	}

	// The server is deleted out-of-band, leaving the managed boot volume orphaned.
	fake.orphanServer(srv.ServerID)
	if len(fake.volumes) != 1 {
		t.Fatalf("orphaned volume should remain until reaped, got %d", len(fake.volumes))
	}

	if _, err := b.Describe(ctx); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(fake.volumes) != 0 {
		t.Errorf("orphan volume not reaped by Describe: %d remain", len(fake.volumes))
	}
}

// capacityType accepts on_demand and bare_metal (the two Scaleway substrates) and
// rejects spot/reserved/nonsense so the provider can never mis-declare it.
func TestOffering_CapacityType(t *testing.T) {
	onDemand := map[string]bool{"on_demand": true, "on-demand": true, "ondemand": true, "": true}
	for v := range onDemand {
		if ct, err := (offering{Capacity: v}).capacityType(); err != nil || ct != providerkit.CapacityOnDemand {
			t.Errorf("capacity_type %q: got (%v, %v), want (OnDemand, nil)", v, ct, err)
		}
	}
	for _, v := range []string{"bare_metal", "bare-metal", "metal", "baremetal"} {
		if ct, err := (offering{Capacity: v}).capacityType(); err != nil || ct != providerkit.CapacityBareMetal {
			t.Errorf("capacity_type %q: got (%v, %v), want (BareMetal, nil)", v, ct, err)
		}
	}
	for _, bad := range []string{"spot", "reserved", "nonsense"} {
		if _, err := (offering{CommercialType: "DEV1-S", Zone: "fr-par-1", Capacity: bad}).capacityType(); err == nil {
			t.Errorf("expected capacity_type %q to be rejected", bad)
		}
	}
}

// A backend rejects offerings whose declared capacity_type does not match the
// substrate the process serves (one substrate per process).
func TestBackend_RejectsMismatchedCapacity(t *testing.T) {
	fake := newSCWFake()
	logger := quietLogger()
	// A bare_metal offering handed to an ON_DEMAND (Instances) process must fail.
	offs := []offering{{CommercialType: "EM-A210R-HDD", Zone: "fr-par-1", Capacity: "bare_metal", Count: 1, Resources: map[string]string{"cpu": "1"}}}
	if _, err := newScalewayBackend("scaleway-test", providerkit.CapacityOnDemand, "fr-par-1", "img", fake, offs, newPricing(defaultEURtoUSD, fake, logger), nil, logger); err == nil {
		t.Fatal("expected a bare_metal offering to be rejected by an ON_DEMAND process")
	}
}

// A backend rejects an offering whose zone differs from the process's single zone
// (one zone per process; the real client is single-zone).
func TestBackend_RejectsMismatchedZone(t *testing.T) {
	fake := newSCWFake()
	logger := quietLogger()
	offs := []offering{{CommercialType: "DEV1-S", Zone: "nl-ams-1", Capacity: "on_demand", Count: 1, Resources: map[string]string{"cpu": "1"}}}
	if _, err := newScalewayBackend("scaleway-test", providerkit.CapacityOnDemand, "fr-par-1", "img", fake, offs, newPricing(defaultEURtoUSD, fake, logger), nil, logger); err == nil {
		t.Fatal("expected an nl-ams-1 offering to be rejected by an fr-par-1 process")
	}
}

// A backend rejects an on-demand offering whose commercial type has no pinned
// price (it would otherwise advertise price_per_hour = 0).
func TestBackend_RejectsUnpricedOnDemandType(t *testing.T) {
	fake := newSCWFake()
	logger := quietLogger()
	offs := []offering{{CommercialType: "DEV9-UNPRICED", Zone: "fr-par-1", Capacity: "on_demand", Count: 1, Resources: map[string]string{"cpu": "1"}}}
	if _, err := newScalewayBackend("scaleway-test", providerkit.CapacityOnDemand, "fr-par-1", "img", fake, offs, newPricing(defaultEURtoUSD, fake, logger), nil, logger); err == nil {
		t.Fatal("expected an unpinned on-demand type to be rejected (price would be 0)")
	}
}
