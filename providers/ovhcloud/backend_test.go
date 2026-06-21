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

func newTestBackend(t *testing.T, seedCount int) (*ovhBackend, *ovhFake) {
	t.Helper()
	fake := newOVHFake()
	logger := quietLogger()
	// One process per region: the backend is configured for GRA, so every
	// offering must be in GRA (mismatched regions are rejected at construction).
	offs := defaultOfferings(seedCount, "GRA", "GRA")
	b, err := newOVHBackend("ovh-public-test", "GRA", "img-ubuntu-2404", fake, offs, newPricing(defaultEURtoUSD), nil, logger)
	if err != nil {
		t.Fatalf("newOVHBackend: %v", err)
	}
	return b, fake
}

func newTestServer(t *testing.T, b *ovhBackend) *providerkit.Server {
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

// The default offerings must seed valid field shape. OVH Public Cloud is
// on-demand only, so every slot is ON_DEMAND with interruption_probability == 0.
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
	}
}

// resources (per-replica request shape) and allocatable (per-machine hardware)
// must be DISTINCT — conflating them forces density=1 and breaks packing.
func TestSeed_ResourcesDistinctFromAllocatable(t *testing.T) {
	b, _ := newTestBackend(t, 4)
	for _, in := range b.speculativeSlots() {
		if in.Resources["cpu"] == in.Allocatable["cpu"] && in.Resources["memory"] == in.Allocatable["memory"] {
			t.Errorf("%s: resources == allocatable (%v) — density math would collapse to 1", in.ID, in.Resources)
		}
	}
}

// A full lifecycle through providerkit drives the OVH fake: Create launches a
// server (host appears), Configure binds it, Drain unbinds, Delete deletes it
// (slot returns to Speculative).
func TestFullLifecycle_DrivesOVH(t *testing.T) {
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
		t.Fatalf("OVH fake has %d servers after Create, want 1", got)
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
		t.Errorf("OVH fake has %d servers after Delete, want 0", got)
	}
}

// Describe must reconcile a running managed server back to its offering slot as
// Idle (recovery from OpenStack metadata when there is no persisted store).
func TestDescribe_ReconcilesRunningServer(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slots := b.speculativeSlots()
	slot := slots[0]
	if _, err := fake.CreateServer(ctx, serverSpec{MachineID: slot.ID, Flavor: slot.InstanceType, Region: slot.Zone}); err != nil {
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
	fake := newOVHFake()
	ctx := context.Background()
	spec := serverSpec{MachineID: "m1", Flavor: "b2-7", Region: "GRA", IdempotencyToken: "op-1"}
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
// still-configured offering for its flavor, so it keeps matching its demand
// profile.
func TestServerToIdle_RecoversResources(t *testing.T) {
	b, _ := newTestBackend(t, 4)
	got := b.serverToIdle("orphan-1", serverInstance{ServerID: "uuid-9", Flavor: "b2-7", Region: "GRA"})
	if got.Resources["cpu"] == "" {
		t.Errorf("rebound machine has no resources; want the offering's per-replica shape, got %v", got.Resources)
	}
	// A flavor covered by no offering yields nil (FileStore is the recovery path).
	if r := b.resourcesForFlavor("r3-999-unoffered", "GRA"); r != nil {
		t.Errorf("unoffered flavor resources = %v, want nil", r)
	}
}

// Two live servers tagged with the SAME machine id must both appear in
// inventory: the first backs its slot, the extra is surfaced as an orphan under
// its server UUID — never silently dropped (a dropped paid instance is invisible
// to cleanup).
func TestDescribe_DuplicateMachineIDSurfacedAsOrphan(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()
	slot := b.speculativeSlots()[0]

	// Two distinct servers (distinct tokens => distinct UUIDs) both tagged with
	// the same machine id.
	a, err := fake.CreateServer(ctx, serverSpec{MachineID: slot.ID, Flavor: slot.InstanceType, Region: slot.Zone, IdempotencyToken: "tokA"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	c, err := fake.CreateServer(ctx, serverSpec{MachineID: slot.ID, Flavor: slot.InstanceType, Region: slot.Zone, IdempotencyToken: "tokB"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if a.ServerID == c.ServerID {
		t.Fatalf("expected two distinct servers, got %s twice", a.ServerID)
	}

	got, err := b.Describe(ctx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	refs := map[string]bool{}
	for _, in := range got {
		if in.Host.Ref != "" {
			refs[in.Host.Ref] = true
		}
	}
	if !refs[a.ServerID] || !refs[c.ServerID] {
		t.Errorf("both duplicate-machine-id servers must be surfaced; have refs %v (want %s and %s)", refs, a.ServerID, c.ServerID)
	}
}

// A real (region-scoped) backend must reject an offering whose region differs
// from --region — otherwise it would create in --region but advertise a foreign
// Machine.zone. The fake backend (empty region) accepts multi-region offerings.
func TestNewOVHBackend_RejectsForeignRegion(t *testing.T) {
	fake := newOVHFake()
	offs := []offering{
		{Flavor: "b2-7", Region: "GRA", Capacity: "on_demand", Count: 1, Resources: map[string]string{"cpu": "1", "memory": "2Gi"}},
		{Flavor: "c2-15", Region: "SBG", Capacity: "on_demand", Count: 1, Resources: map[string]string{"cpu": "1", "memory": "2Gi"}},
	}
	if _, err := newOVHBackend("ovh-public-GRA", "GRA", "img", fake, offs, newPricing(1.08), nil, quietLogger()); err == nil {
		t.Error("expected rejection of an SBG offering on a GRA-configured backend")
	}
	// Empty region (fake backend) accepts the multi-region mix.
	if _, err := newOVHBackend("ovh-public", "", "img", fake, offs, newPricing(1.08), nil, quietLogger()); err != nil {
		t.Errorf("fake backend (empty region) should accept multi-region offerings: %v", err)
	}
}

// A powered-off (non-running) server must never be advertised as a schedulable
// Idle host: Describe SKIPS it entirely (slot returns to Speculative, and it is
// not surfaced as an Idle orphan either). The real client's Create guard recovers
// and powers it back on by machine id — so this doesn't double-provision — but
// that is real-only; here we assert Describe never publishes a bindable phantom.
func TestDescribe_NonRunningServerSkipped(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()
	slot := b.speculativeSlots()[0]
	srv, err := fake.CreateServer(ctx, serverSpec{MachineID: slot.ID, Flavor: slot.InstanceType, Region: slot.Zone})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}
	fake.servers[srv.ServerID].Running = false // power it off (SHUTOFF)

	got, err := b.Describe(ctx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for i := range got {
		if got[i].Host.Ref == srv.ServerID {
			t.Errorf("powered-off server must not be published (got %s as %s)", got[i].ID, got[i].State)
		}
		if got[i].ID == slot.ID {
			if got[i].State != providerkit.StateSpeculative || got[i].Host.Ref != "" {
				t.Errorf("slot should be Speculative with no host when its server is off, got %+v", got[i])
			}
		}
	}
}

// gpuLabel must map each OVH GPU family to its ACTUAL NVIDIA model (a10 != a100,
// l40s != l4), or accelerator node-selectors would mis-schedule.
func TestGPULabel(t *testing.T) {
	cases := map[string]string{
		"t1-45":  "nvidia-v100",
		"t2-45":  "nvidia-v100s",
		"a10-45": "nvidia-a10",
		"a100-7": "nvidia-a100",
		"l4-90":  "nvidia-l4",
		"l40s-7": "nvidia-l40s",
	}
	for flavor, want := range cases {
		got, ok := gpuLabel(flavor)
		if !ok || got != want {
			t.Errorf("gpuLabel(%q) = (%q, %v), want (%q, true)", flavor, got, ok, want)
		}
	}
	if _, ok := gpuLabel("b2-7"); ok {
		t.Error("non-GPU flavor b2-7 should not get an accelerator label")
	}
}

// A machine the kit already tracks as Idle whose backing server was stopped out
// of band (or after a persisted-store restart) must be HEALED on Configure: the
// provider powers the host on before SSH rather than dialing a powered-off node.
// (The real client does this via ensureRunning; the fake models the same heal.)
func TestConfigure_HealsStoppedHost(t *testing.T) {
	b, fake := newTestBackend(t, 8)
	s := newTestServer(t, b)
	ctx := context.Background()

	resp, _ := s.List(ctx, &pb.ListFilter{States: []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE}, MaxResults: 1})
	id := resp.GetMachines()[0].GetId()
	if _, err := s.Create(ctx, &pb.CreateRequest{MachineId: id}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m := waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)
	ref := m.GetHost().GetRef()

	// Simulate an out-of-band stop of the backing server while the kit holds the
	// machine Idle.
	fake.servers[ref].Running = false

	if _, err := s.Configure(ctx, &pb.ConfigureRequest{MachineId: id, ClusterId: "c1", BootstrapBlob: []byte("join")}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED)
	if !fake.servers[ref].Running {
		t.Error("Configure must power the stopped backing server back on before SSH")
	}
}

func TestOffering_CapacityType(t *testing.T) {
	// Only on-demand is a real OVH Public Cloud substrate; everything else is
	// rejected so the provider can never mis-declare capacity_type.
	for _, ok := range []string{"on_demand", "on-demand", "ondemand", ""} {
		if ct, err := (offering{Capacity: ok}).capacityType(); err != nil || ct != providerkit.CapacityOnDemand {
			t.Errorf("capacity_type %q: got (%v, %v), want (OnDemand, nil)", ok, ct, err)
		}
	}
	for _, bad := range []string{"spot", "reserved", "bare_metal", "bare-metal", "metal", "nonsense"} {
		if _, err := (offering{Flavor: "b2-7", Region: "GRA", Capacity: bad}).capacityType(); err == nil {
			t.Errorf("expected capacity_type %q to be rejected (OVH Public Cloud is on-demand only)", bad)
		}
	}
}
