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

func newTestBackend(t *testing.T, seedCount int) (*latitudeBackend, *latitudeFake) {
	t.Helper()
	fake := newLatitudeFake()
	logger := quietLogger()
	offs := defaultOfferings(seedCount, "ASH", "NYC")
	b, err := newLatitudeBackend("latitude-test", "ubuntu_22_04_x64_lts", fake, offs, newPricing(fake, logger), nil, logger)
	if err != nil {
		t.Fatalf("newLatitudeBackend: %v", err)
	}
	// Keep EnsureRunning's poll snappy in tests.
	b.ensureRunningPoll = time.Millisecond
	b.ensureRunningTimeout = 2 * time.Second
	return b, fake
}

func newTestServer(t *testing.T, b *latitudeBackend) *providerkit.Server {
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
	deadline := time.Now().Add(3 * time.Second)
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

// The default offerings must seed valid field shape. Latitude is on-demand bare
// metal, so every slot is ON_DEMAND with interruption_probability == 0, and the
// per-replica resources are distinct from (much smaller than) the plan's
// allocatable hardware so density >> 1.
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
		alloc := m.GetAllocatable().GetResources()
		res := m.GetResources().GetResources()
		if len(alloc) == 0 {
			t.Errorf("%s: missing allocatable", m.GetId())
		}
		if len(res) == 0 {
			t.Errorf("%s: missing resources", m.GetId())
		}
		// resources MUST be distinct from allocatable (density > 1).
		if alloc["cpu"] == res["cpu"] && alloc["memory"] == res["memory"] {
			t.Errorf("%s: resources == allocatable (%v) — density forced to 1, breaks packing", m.GetId(), res)
		}
		if m.GetPricePerHour() <= 0 {
			t.Errorf("%s: price_per_hour = %v, want > 0", m.GetId(), m.GetPricePerHour())
		}
	}
}

// A full lifecycle through providerkit drives the Latitude fake: Create deploys a
// server (host appears), Configure binds it, Drain unbinds, Delete deprovisions
// it (slot returns to Speculative).
func TestFullLifecycle_DrivesLatitude(t *testing.T) {
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
		t.Fatal("Idle machine has no host (CreateServer result not attached)")
	}
	if got := len(fake.servers); got != 1 {
		t.Fatalf("Latitude fake has %d servers after Create, want 1", got)
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
		t.Errorf("Latitude fake has %d servers after Delete, want 0", got)
	}
}

// EnsureRunning regression (Configure): a tracked server powered off out-of-band
// must be powered ON by the backend BEFORE the bootstrap is delivered. The fake's
// ApplyBootstrap fails on a powered-off server, so reaching CONFIGURED proves the
// backend powered it on first.
func TestConfigure_PowersOnStoppedServer(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	s := newTestServer(t, b)
	ctx := context.Background()

	resp, _ := s.List(ctx, &pb.ListFilter{States: []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE}, MaxResults: 1})
	id := resp.GetMachines()[0].GetId()
	if _, err := s.Create(ctx, &pb.CreateRequest{MachineId: id}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m := waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)

	// Simulate an out-of-band power-off of the deployed server.
	fake.setPowerState(m.GetHost().GetRef(), false)

	if _, err := s.Configure(ctx, &pb.ConfigureRequest{MachineId: id, ClusterId: "c1", BootstrapBlob: []byte("join")}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	// Reaching CONFIGURED is only possible if EnsureRunning powered the server on
	// before ApplyBootstrap (which fails on a stopped server).
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED)
	srv, err := fake.GetServer(ctx, m.GetHost().GetRef())
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if !srv.PoweredOn {
		t.Error("server was not powered on by Configure's EnsureRunning")
	}
}

// EnsureRunning regression (Drain): same as above, for Drain. A tracked-bound
// server powered off out-of-band must be powered on before the drain.
func TestDrain_PowersOnStoppedServer(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	s := newTestServer(t, b)
	ctx := context.Background()

	resp, _ := s.List(ctx, &pb.ListFilter{States: []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE}, MaxResults: 1})
	id := resp.GetMachines()[0].GetId()
	if _, err := s.Create(ctx, &pb.CreateRequest{MachineId: id}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m := waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)
	if _, err := s.Configure(ctx, &pb.ConfigureRequest{MachineId: id, ClusterId: "c1", BootstrapBlob: []byte("join")}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED)

	// Simulate an out-of-band power-off of the bound server.
	fake.setPowerState(m.GetHost().GetRef(), false)

	if _, err := s.Drain(ctx, &pb.DrainRequest{MachineId: id, GracePeriodSeconds: 5}); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)
	srv, err := fake.GetServer(ctx, m.GetHost().GetRef())
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if !srv.PoweredOn {
		t.Error("server was not powered on by Drain's EnsureRunning")
	}
}

// Describe must reconcile a running managed server back to its offering slot as
// Idle (recovery from Latitude tags when there is no persisted store), and must
// NOT power on a stopped server.
func TestDescribe_ReconcilesRunningServer(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slots := b.speculativeSlots()
	slot := slots[0]
	srv, err := fake.CreateServer(ctx, serverSpec{MachineID: slot.ID, Plan: slot.InstanceType, Site: slot.Zone})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}
	// A tagged-but-stopped server must remain Idle and reapable (not dropped, not
	// powered on by Describe).
	fake.setPowerState(srv.ServerID, false)

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
	cur, _ := fake.GetServer(ctx, srv.ServerID)
	if cur.PoweredOn {
		t.Error("Describe powered on a stopped server; it must not")
	}
}

// Create must be idempotent at the substrate level: a retried CreateServer with
// the same operation id adopts the same server, never deploying a duplicate.
func TestCreateServer_IdempotentOnToken(t *testing.T) {
	fake := newLatitudeFake()
	ctx := context.Background()
	spec := serverSpec{MachineID: "m1", Plan: "c2-small-x86", Site: "ASH", IdempotencyToken: "op-1"}
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
		t.Errorf("idempotent create deployed %d servers, want 1", len(fake.servers))
	}
}

// An orphan / offering-shrank server rebinds with the per-replica resources of a
// still-configured offering for its plan, so it keeps matching its demand
// profile.
func TestServerToIdle_RecoversResources(t *testing.T) {
	b, _ := newTestBackend(t, 4)
	got := b.serverToIdle("orphan-1", serverInstance{ServerID: "sv_9", Plan: "c2-small-x86", Site: "ASH"})
	if got.Resources["cpu"] == "" {
		t.Errorf("rebound machine has no resources; want the offering's per-replica shape, got %v", got.Resources)
	}
	// A plan covered by no offering yields nil (FileStore is the recovery path).
	if r := b.resourcesForPlan("zz-unoffered-x86", "ASH"); r != nil {
		t.Errorf("unoffered plan resources = %v, want nil", r)
	}
}

func TestOffering_CapacityType(t *testing.T) {
	// Only on-demand is a valid Latitude capacity_type; spot and bare_metal are
	// rejected so the provider can never mis-declare it (bare_metal would suppress
	// the shard's Delete and leak servers).
	for _, ok := range []string{"on_demand", "on-demand", "ondemand", ""} {
		if ct, err := (offering{Capacity: ok}).capacityType(); err != nil || ct != providerkit.CapacityOnDemand {
			t.Errorf("capacity_type %q: got (%v, %v), want (OnDemand, nil)", ok, ct, err)
		}
	}
	for _, bad := range []string{"spot", "reserved", "bare_metal", "bare-metal", "metal", "nonsense"} {
		if _, err := (offering{Plan: "c2-small-x86", Site: "ASH", Capacity: bad}).capacityType(); err == nil {
			t.Errorf("expected capacity_type %q to be rejected", bad)
		}
	}
}
