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

func newTestBackend(t *testing.T, seedCount int) (*libvirtBackend, *libvirtFake) {
	t.Helper()
	fake := newLibvirtFake()
	logger := quietLogger()
	catalog := newInstanceCatalog(nil)
	offs := defaultOfferings(seedCount, "rack1", "rack2", "on_demand", catalog.names())
	pr := newPricing(catalog, 0, 0, nil)
	b, err := newLibvirtBackend("libvirt-test", "ubuntu-24.04.qcow2", fake, offs, catalog, pr, nil, logger)
	if err != nil {
		t.Fatalf("newLibvirtBackend: %v", err)
	}
	return b, fake
}

func newTestServer(t *testing.T, b *libvirtBackend) *providerkit.Server {
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

// The default offerings must seed valid field shape. libvirt VMs are on-demand
// with interruption_probability == 0, allocatable from the catalog, resources
// from the offering, and a synthetic price > 0.
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
		if m.GetResources() == nil || len(m.GetResources().GetResources()) == 0 {
			t.Errorf("%s: missing resources", m.GetId())
		}
		if m.GetPricePerHour() <= 0 {
			t.Errorf("%s: price_per_hour = %v, want > 0", m.GetId(), m.GetPricePerHour())
		}
		// allocatable must not equal resources (density would be forced to 1).
		if mapsEq(m.GetAllocatable().GetResources(), m.GetResources().GetResources()) {
			t.Errorf("%s: allocatable == resources (breaks density math)", m.GetId())
		}
	}
}

func mapsEq(a, b map[string]string) bool {
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

// A full lifecycle through providerkit drives the libvirt fake: Create
// defines+starts a domain (host appears), Configure binds it, Drain unbinds,
// Delete destroys it (slot returns to Speculative).
func TestFullLifecycle_DrivesLibvirt(t *testing.T) {
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
		t.Fatal("Idle machine has no host (CreateDomain result not attached)")
	}
	if got := len(fake.domains); got != 1 {
		t.Fatalf("libvirt fake has %d domains after Create, want 1", got)
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
	if got := len(fake.domains); got != 0 {
		t.Errorf("libvirt fake has %d domains after Delete, want 0", got)
	}
}

// Describe must reconcile a running managed domain back to its offering slot as
// Idle (recovery from libvirt metadata when there is no persisted store).
func TestDescribe_ReconcilesRunningDomain(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slots := b.speculativeSlots()
	slot := slots[0]
	if _, err := fake.CreateDomain(ctx, domainSpec{MachineID: slot.ID, InstanceType: slot.InstanceType, Zone: slot.Zone}); err != nil {
		t.Fatalf("seed domain: %v", err)
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

// Create must be idempotent at the substrate level: a retried CreateDomain with
// the same operation id returns the same domain, never a duplicate.
func TestCreateDomain_IdempotentOnToken(t *testing.T) {
	fake := newLibvirtFake()
	ctx := context.Background()
	spec := domainSpec{MachineID: "m1", InstanceType: "kvm.small", Zone: "rack1", IdempotencyToken: "op-1"}
	a, err := fake.CreateDomain(ctx, spec)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	bb, err := fake.CreateDomain(ctx, spec)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if a.DomainName != bb.DomainName {
		t.Errorf("idempotent create returned distinct domains %s vs %s", a.DomainName, bb.DomainName)
	}
	if len(fake.domains) != 1 {
		t.Errorf("idempotent create defined %d domains, want 1", len(fake.domains))
	}
}

func TestOffering_CapacityType(t *testing.T) {
	for _, ok := range []string{"on_demand", "on-demand", "ondemand", ""} {
		if ct, err := (offering{Capacity: ok}).capacityType(); err != nil || ct != providerkit.CapacityOnDemand {
			t.Errorf("capacity_type %q: got (%v, %v), want (OnDemand, nil)", ok, ct, err)
		}
	}
	if ct, err := (offering{Capacity: "bare_metal"}).capacityType(); err != nil || ct != providerkit.CapacityBareMetal {
		t.Errorf("bare_metal: got (%v, %v), want (BareMetal, nil)", ct, err)
	}
	// spot and reserved have no meaning for a local libvirt host and must be
	// rejected (no preemption market, no reservation/commitment billing).
	for _, bad := range []string{"spot", "reserved", "nonsense"} {
		if _, err := (offering{Capacity: bad}).capacityType(); err == nil {
			t.Errorf("expected capacity_type %q to be rejected", bad)
		}
	}
}

// Bare-metal offerings price at 0 (owned hardware) and omit Delete semantics in
// the shard, but the kit still serves the field shape correctly.
func TestPricing_BareMetalZero(t *testing.T) {
	catalog := newInstanceCatalog(nil)
	pr := newPricing(catalog, 0, 0, nil)
	if got := pr.price("kvm.large", providerkit.CapacityBareMetal); got != 0 {
		t.Errorf("bare-metal price = %v, want 0", got)
	}
	if got := pr.price("kvm.large", providerkit.CapacityOnDemand); got <= 0 {
		t.Errorf("on-demand price = %v, want > 0", got)
	}
	// Explicit override wins.
	pr2 := newPricing(catalog, 0, 0, map[string]float64{"kvm.small": 0.05})
	if got := pr2.price("kvm.small", providerkit.CapacityOnDemand); got != 0.05 {
		t.Errorf("override price = %v, want 0.05", got)
	}
}

func TestInstanceCatalog_Allocatable(t *testing.T) {
	c := newInstanceCatalog(nil)
	got := c.allocatable("kvm.large")
	if got["cpu"] != "8" || got["memory"] != "16Gi" {
		t.Errorf("kvm.large allocatable = %v, want cpu=8 memory=16Gi", got)
	}
	if c.allocatable("nope") != nil {
		t.Error("unknown type should yield nil allocatable")
	}
}

func TestParseConnections(t *testing.T) {
	// Single bare URI -> default zone.
	conns, err := parseConnections("qemu:///system", "local")
	if err != nil || len(conns) != 1 || conns[0].Zone != "local" || conns[0].URI != "qemu:///system" {
		t.Fatalf("single uri: got %v, %v", conns, err)
	}
	// zone=uri list.
	conns, err = parseConnections("a=qemu+libssh://h1/system,b=qemu+libssh://h2/system", "local")
	if err != nil || len(conns) != 2 {
		t.Fatalf("list: got %v, %v", conns, err)
	}
	// Empty -> nil (fake backend).
	if c, err := parseConnections("", "local"); err != nil || c != nil {
		t.Fatalf("empty: got %v, %v", c, err)
	}
	// Duplicate zone rejected.
	if _, err := parseConnections("a=x,a=y", "local"); err == nil {
		t.Error("duplicate zone should be rejected")
	}
	// Single bare URI WITH query params (keyfile/known_hosts) -> default zone, URI
	// kept intact (must NOT be mis-split into zone=uri on the '=' inside a param).
	bare := "qemu+libssh://bigfleet@host-a/system?keyfile=/k&known_hosts=/kh"
	conns, err = parseConnections(bare, "local")
	if err != nil || len(conns) != 1 || conns[0].Zone != "local" || conns[0].URI != bare {
		t.Fatalf("bare uri with query params: got %v, %v", conns, err)
	}
	// Single zone=uri WITH query params -> zone + full URI (split on the FIRST '=').
	conns, err = parseConnections("rack1=qemu+libssh://host-a/system?keyfile=/k&known_hosts=/kh", "local")
	if err != nil || len(conns) != 1 || conns[0].Zone != "rack1" ||
		conns[0].URI != "qemu+libssh://host-a/system?keyfile=/k&known_hosts=/kh" {
		t.Fatalf("zone=uri with query params: got %v, %v", conns, err)
	}
	// Multi-host list, each with query params.
	conns, err = parseConnections("a=qemu+libssh://h1/system?keyfile=/k,b=qemu+libssh://h2/system?keyfile=/k", "local")
	if err != nil || len(conns) != 2 || conns[0].URI != "qemu+libssh://h1/system?keyfile=/k" {
		t.Fatalf("multi-host with query params: got %v, %v", conns, err)
	}
}

func TestDefaultOfferings_SmallSeedCountNoEmptyBuckets(t *testing.T) {
	// A seed-count below 4 must not emit count==0 offerings (which the backend
	// rejects), and the surviving offerings must total the seed count.
	for _, seed := range []int{1, 2, 3, 7} {
		offs := defaultOfferings(seed, "rack1", "rack2", "on_demand", []string{"kvm.small", "kvm.large"})
		total := 0
		for _, off := range offs {
			if off.Count <= 0 {
				t.Errorf("seed %d: offering %s/%s has non-positive count %d", seed, off.InstanceType, off.Zone, off.Count)
			}
			total += off.Count
		}
		if total != seed {
			t.Errorf("seed %d: offerings total %d slots, want %d", seed, total, seed)
		}
	}
}

func TestNewLibvirtBackend_RejectsNonPositiveCount(t *testing.T) {
	catalog := newInstanceCatalog(nil)
	pr := newPricing(catalog, 0, 0, nil)
	offs := []offering{{InstanceType: "kvm.small", Zone: "rack1", Capacity: "on_demand", Count: 0}}
	if _, err := newLibvirtBackend("libvirt-test", "img", newLibvirtFake(), offs, catalog, pr, nil, quietLogger()); err == nil {
		t.Error("expected a count==0 offering to be rejected at startup")
	}
}

func TestSplitHostRef(t *testing.T) {
	z, d, ok := splitHostRef("rack1/bigfleet-000001")
	if !ok || z != "rack1" || d != "bigfleet-000001" {
		t.Errorf("split = (%q,%q,%v)", z, d, ok)
	}
	if _, _, ok := splitHostRef("noslash"); ok {
		t.Error("ref with no slash should not split")
	}
}

// resolveBackendMode: auto picks fake with no connections (the certify path),
// libvirt with connections.
func TestResolveBackendMode(t *testing.T) {
	if got := resolveBackendMode("auto", nil); got != "fake" {
		t.Errorf("auto+no-conns = %q, want fake", got)
	}
	if got := resolveBackendMode("auto", []hostConn{{Zone: "z", URI: "qemu:///system"}}); got != "libvirt" {
		t.Errorf("auto+conns = %q, want libvirt", got)
	}
	if got := resolveBackendMode("fake", []hostConn{{Zone: "z"}}); got != "fake" {
		t.Errorf("explicit fake = %q, want fake", got)
	}
}
