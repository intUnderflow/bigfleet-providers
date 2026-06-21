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

func newTestBackend(t *testing.T, seedCount int) (*upcloudBackend, *upcloudFake) {
	t.Helper()
	fake := newUpcloudFake()
	logger := quietLogger()
	offs := defaultOfferings(seedCount, "fi-hel1", "de-fra1")
	b, err := newUpcloudBackend("upcloud-test", "01000000-0000-4000-8000-000000000000", fake, offs, newPricing(defaultEURtoUSD, logger), nil, logger)
	if err != nil {
		t.Fatalf("newUpcloudBackend: %v", err)
	}
	return b, fake
}

func newTestServer(t *testing.T, b *upcloudBackend) *providerkit.Server {
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

// The default offerings must seed valid field shape. UpCloud is on-demand only,
// so every slot is ON_DEMAND with interruption_probability == 0.
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
		// resources (per-replica) MUST be distinct from allocatable (hardware
		// total) — equal maps force density=1 and break the shard's packing math.
		alloc := m.GetAllocatable().GetResources()
		res := m.GetResources().GetResources()
		if alloc["cpu"] == res["cpu"] && alloc["memory"] == res["memory"] {
			t.Errorf("%s: resources == allocatable (%v) — must be distinct", m.GetId(), res)
		}
	}
}

// A full lifecycle through providerkit drives the UpCloud fake: Create launches a
// server (host appears), Configure binds it, Drain unbinds, Delete deletes it
// (slot returns to Speculative).
func TestFullLifecycle_DrivesUpcloud(t *testing.T) {
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
		t.Fatalf("UpCloud fake has %d servers after Create, want 1", got)
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
		t.Errorf("UpCloud fake has %d servers after Delete, want 0", got)
	}
	// The leak-free Delete path must also remove the attached storage.
	if got := len(fake.storages); got != 0 {
		t.Errorf("UpCloud fake leaked %d storage devices after Delete, want 0", got)
	}
}

// Describe must reconcile a running managed server back to its offering slot as
// Idle (recovery from UpCloud labels when there is no persisted store).
func TestDescribe_ReconcilesRunningServer(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slots := b.speculativeSlots()
	slot := slots[0]
	if _, err := fake.CreateServer(ctx, serverSpec{MachineID: slot.ID, Plan: slot.InstanceType, Zone: slot.Zone}); err != nil {
		t.Fatalf("seed server: %v", err)
	}

	got := describeByID(t, b, slot.ID)
	if got.State != providerkit.StateIdle {
		t.Errorf("backed slot state = %v, want Idle", got.State)
	}
	if got.Host.Ref == "" {
		t.Error("backed slot has no host")
	}
}

// §4.6: a tagged server stopped out-of-band still owns its slot — Describe must
// report it as Idle (reapable), not drop it and not provision a second server.
func TestDescribe_StoppedTaggedServerStaysIdle(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slot := b.speculativeSlots()[0]
	srv, err := fake.CreateServer(ctx, serverSpec{MachineID: slot.ID, Plan: slot.InstanceType, Zone: slot.Zone})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}
	fake.stopOutOfBand(srv.UUID)

	got := describeByID(t, b, slot.ID)
	if got.State != providerkit.StateIdle {
		t.Errorf("stopped tagged slot state = %v, want Idle (still owns its slot)", got.State)
	}
	if got.Host.Ref != srv.UUID {
		t.Errorf("stopped tagged slot host = %q, want %q", got.Host.Ref, srv.UUID)
	}
}

// §4.6: Configure against a server stopped out-of-band must power it on
// (EnsureRunning) and then bootstrap — not hang against the stopped host.
func TestConfigure_PowersOnStoppedServer(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	s := newTestServer(t, b)
	ctx := context.Background()

	id := firstSpeculative(t, s)
	mustCreate(t, s, id)
	m := waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)

	fake.stopOutOfBand(m.GetHost().GetRef())

	if _, err := s.Configure(ctx, &pb.ConfigureRequest{MachineId: id, ClusterId: "c1", BootstrapBlob: []byte("join")}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	m = waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED)
	if m.GetCluster() != "c1" {
		t.Errorf("cluster = %q, want c1 (Configure must power on then bootstrap)", m.GetCluster())
	}
	if srv := fake.servers[m.GetHost().GetRef()]; srv == nil || !srv.Running {
		t.Error("server not running after Configure powered it on")
	}
}

// §4.6: Drain against a server stopped out-of-band must power it on
// (EnsureRunning) and then drain to Idle.
func TestDrain_PowersOnStoppedServer(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	s := newTestServer(t, b)
	ctx := context.Background()

	id := firstSpeculative(t, s)
	mustCreate(t, s, id)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)
	if _, err := s.Configure(ctx, &pb.ConfigureRequest{MachineId: id, ClusterId: "c1", BootstrapBlob: []byte("join")}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	m := waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED)

	fake.stopOutOfBand(m.GetHost().GetRef())

	if _, err := s.Drain(ctx, &pb.DrainRequest{MachineId: id, GracePeriodSeconds: 5}); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	m = waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)
	if m.GetCluster() != "" {
		t.Errorf("cluster = %q, want cleared (Drain must power on then drain)", m.GetCluster())
	}
	if srv := fake.servers[m.GetHost().GetRef()]; srv == nil || !srv.Running {
		t.Error("server not running after Drain powered it on")
	}
}

// Create must be idempotent at the substrate level: a retried CreateServer with
// the same operation id returns the same server, never a duplicate.
func TestCreateServer_IdempotentOnToken(t *testing.T) {
	fake := newUpcloudFake()
	ctx := context.Background()
	spec := serverSpec{MachineID: "m1", Plan: "2xCPU-4GB", Zone: "fi-hel1", IdempotencyToken: "op-1"}
	a, err := fake.CreateServer(ctx, spec)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := fake.CreateServer(ctx, spec)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if a.UUID != b.UUID {
		t.Errorf("idempotent create returned distinct UUIDs %s vs %s", a.UUID, b.UUID)
	}
	if len(fake.servers) != 1 {
		t.Errorf("idempotent create launched %d servers, want 1", len(fake.servers))
	}
}

// An orphan / offering-shrank server rebinds with the per-replica resources of a
// still-configured offering for its plan, so it keeps matching its demand profile.
func TestServerToIdle_RecoversResources(t *testing.T) {
	b, _ := newTestBackend(t, 4)
	got := b.serverToIdle("orphan-1", serverInstance{UUID: "u9", Plan: "2xCPU-4GB", Zone: "fi-hel1"})
	if got.Resources["cpu"] == "" {
		t.Errorf("rebound machine has no resources; want the offering's per-replica shape, got %v", got.Resources)
	}
	// A plan covered by no offering yields nil (FileStore is the recovery path).
	if r := b.resourcesForPlan("99xCPU-unoffered", "fi-hel1"); r != nil {
		t.Errorf("unoffered plan resources = %v, want nil", r)
	}
}

func TestOffering_CapacityType(t *testing.T) {
	// Only on-demand is a real UpCloud substrate; everything else is rejected so
	// the provider can never mis-declare capacity_type.
	for _, ok := range []string{"on_demand", "on-demand", "ondemand", ""} {
		if ct, err := (offering{Capacity: ok}).capacityType(); err != nil || ct != providerkit.CapacityOnDemand {
			t.Errorf("capacity_type %q: got (%v, %v), want (OnDemand, nil)", ok, ct, err)
		}
	}
	for _, bad := range []string{"spot", "reserved", "bare_metal", "bare-metal", "metal", "nonsense"} {
		if _, err := (offering{Plan: "2xCPU-4GB", Zone: "fi-hel1", Capacity: bad}).capacityType(); err == nil {
			t.Errorf("expected capacity_type %q to be rejected (UpCloud is on-demand only)", bad)
		}
	}
}

// --- helpers --------------------------------------------------------------

func describeByID(t *testing.T, b *upcloudBackend, id string) providerkit.Instance {
	t.Helper()
	got, err := b.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for i := range got {
		if got[i].ID == id {
			return got[i]
		}
	}
	t.Fatalf("Describe did not return slot %s", id)
	return providerkit.Instance{}
}

func firstSpeculative(t *testing.T, s *providerkit.Server) string {
	t.Helper()
	resp, err := s.List(context.Background(), &pb.ListFilter{States: []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE}, MaxResults: 1})
	if err != nil || len(resp.GetMachines()) == 0 {
		t.Fatalf("no speculative slot: %v", err)
	}
	return resp.GetMachines()[0].GetId()
}

func mustCreate(t *testing.T, s *providerkit.Server, id string) {
	t.Helper()
	if _, err := s.Create(context.Background(), &pb.CreateRequest{MachineId: id}); err != nil {
		t.Fatalf("Create: %v", err)
	}
}
