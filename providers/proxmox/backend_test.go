package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestBackend(t *testing.T, seedCount int) (*proxmoxBackend, *proxmoxFake) {
	t.Helper()
	fake := newProxmoxFake()
	catalog := newInstanceCatalog(nil, defaultTemplateVMID)
	offs := defaultOfferings(seedCount, "pve-1", "pve-2", catalog.names())
	pr := newPricing(catalog, 0, 0, nil)
	b, err := newProxmoxBackend("proxmox-test", fake, offs, catalog, pr, quietLogger())
	if err != nil {
		t.Fatalf("newProxmoxBackend: %v", err)
	}
	return b, fake
}

func newTestServer(t *testing.T, b *proxmoxBackend) *providerkit.Server {
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

// The default offerings must seed valid field shape. Proxmox VMs are on-demand
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

// A full lifecycle through providerkit drives the Proxmox fake: Create clones a
// VM (host appears), Configure binds it, Drain unbinds, Delete destroys it (slot
// returns to Speculative, no disks left behind).
func TestFullLifecycle_DrivesProxmox(t *testing.T) {
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
		t.Fatal("Idle machine has no host (CloneVM result not attached)")
	}
	if got := len(fake.vms); got != 1 {
		t.Fatalf("Proxmox fake has %d VMs after Create, want 1", got)
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
	// Teardown removes the VM (and, in the real client, its disks via purge).
	if got := len(fake.vms); got != 0 {
		t.Errorf("Proxmox fake has %d VMs after Delete, want 0 (VM + disks must be purged)", got)
	}
}

// Configure must power on a VM that went Idle then was stopped out of band (an
// operator power-cycle, an HA stop, a maintenance reboot) before driving the
// guest-agent bootstrap — otherwise it would run against a stopped VM and time
// out FAILED. The fake's ApplyBootstrap rejects a stopped VM, so a green
// Configure proves EnsureRunning healed it first. This is the §4.4 regression.
func TestConfigure_PowersOnStoppedVM(t *testing.T) {
	b, fake := newTestBackend(t, 8)
	s := newTestServer(t, b)
	ctx := context.Background()

	resp, _ := s.List(ctx, &pb.ListFilter{States: []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE}, MaxResults: 1})
	id := resp.GetMachines()[0].GetId()
	if _, err := s.Create(ctx, &pb.CreateRequest{MachineId: id}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m := waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)

	// Simulate an out-of-band stop of the now-Idle VM.
	node, vmid, ok := splitHostRef(m.GetHost().GetRef())
	if !ok {
		t.Fatalf("Idle machine has no usable host ref %q", m.GetHost().GetRef())
	}
	if !fake.setRunning(node, vmid, false) {
		t.Fatalf("setRunning: VM %s/%d not found", node, vmid)
	}

	// Configure must heal it (EnsureRunning) before the guest-agent bootstrap.
	if _, err := s.Configure(ctx, &pb.ConfigureRequest{MachineId: id, ClusterId: "c1", BootstrapBlob: []byte("join")}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED)

	v := fake.vms[fakeKey(node, vmid)]
	if v == nil {
		t.Fatalf("VM %s/%d gone after Configure", node, vmid)
	}
	if !v.Running {
		t.Error("Configure did not power the stopped VM back on")
	}
	if v.ClusterID != "c1" {
		t.Errorf("Configure did not bind the healed VM: cluster=%q", v.ClusterID)
	}
}

// Drain must also power on a stopped-but-Idle VM before the guest-agent drain.
func TestDrain_PowersOnStoppedVM(t *testing.T) {
	b, fake := newTestBackend(t, 8)
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

	node, vmid, _ := splitHostRef(m.GetHost().GetRef())
	if !fake.setRunning(node, vmid, false) {
		t.Fatalf("setRunning: VM %s/%d not found", node, vmid)
	}
	if _, err := s.Drain(ctx, &pb.DrainRequest{MachineId: id, GracePeriodSeconds: 5}); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)
	if v := fake.vms[fakeKey(node, vmid)]; v == nil || !v.Running {
		t.Error("Drain did not power the stopped VM back on before draining")
	}
}

// Describe must reconcile a running managed VM back to its offering slot as Idle
// (recovery from the VM tag/description when there is no persisted store).
func TestDescribe_ReconcilesRunningVM(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slots := b.speculativeSlots()
	slot := slots[0]
	if _, err := fake.CloneVM(ctx, vmSpec{MachineID: slot.ID, InstanceType: slot.InstanceType, Zone: slot.Zone}); err != nil {
		t.Fatalf("seed VM: %v", err)
	}

	got := findInstance(t, b, ctx, slot.ID)
	if got.State != providerkit.StateIdle {
		t.Errorf("backed slot state = %v, want Idle", got.State)
	}
	if got.Host.Ref == "" {
		t.Error("backed slot has no host")
	}
}

// Describe must NOT advertise a tagged-but-stopped managed VM as a schedulable
// Idle node: the VM owns its slot (so Create adopts it rather than cloning a
// duplicate), but until it is powered back on the slot stays Speculative.
func TestDescribe_StoppedVMNotIdle(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	slots := b.speculativeSlots()
	slot := slots[0]
	vm, err := fake.CloneVM(ctx, vmSpec{MachineID: slot.ID, InstanceType: slot.InstanceType, Zone: slot.Zone})
	if err != nil {
		t.Fatalf("seed VM: %v", err)
	}
	if !fake.setRunning(vm.Node, vm.VMID, false) {
		t.Fatalf("setRunning: VM %s/%d not found", vm.Node, vm.VMID)
	}

	got := findInstance(t, b, ctx, slot.ID)
	if got.State != providerkit.StateSpeculative {
		t.Errorf("stopped tagged VM slot state = %v, want Speculative", got.State)
	}
	if got.Host.Ref != "" {
		t.Errorf("stopped slot advertised a host %q, want none", got.Host.Ref)
	}
}

// A tagged managed VM that matches no current offering slot (offering removed)
// must still be surfaced by Describe even when stopped, so it stays managed and
// reapable rather than leaking its disks invisibly. Its instance_type comes from
// the slot id, not an arbitrary same-node offering.
func TestDescribe_StoppedVMNoOfferingNotDropped(t *testing.T) {
	b, fake := newTestBackend(t, 4)
	ctx := context.Background()

	const orphanID = "proxmox-test/OnDemand/pve.large/pve-1/999"
	vm, err := fake.CloneVM(ctx, vmSpec{MachineID: orphanID, InstanceType: "pve.large", Zone: "pve-1"})
	if err != nil {
		t.Fatalf("seed VM: %v", err)
	}
	if !fake.setRunning(vm.Node, vm.VMID, false) {
		t.Fatalf("setRunning: VM %s/%d not found", vm.Node, vm.VMID)
	}

	got := findInstance(t, b, ctx, orphanID)
	if got.State != providerkit.StateIdle {
		t.Errorf("no-offering VM state = %v, want Idle (reapable)", got.State)
	}
	if got.Host.Ref == "" {
		t.Error("no-offering VM surfaced without a host (not reapable)")
	}
	if got.InstanceType != "pve.large" {
		t.Errorf("no-offering VM instance_type = %q, want pve.large (parsed from the slot id)", got.InstanceType)
	}
	if got.CapacityType != providerkit.CapacityOnDemand {
		t.Errorf("no-offering VM capacity = %v, want OnDemand", got.CapacityType)
	}
}

// Create must be idempotent at the substrate level: a retried clone for the same
// machine id adopts the existing VM, never a duplicate (the no-double-clone
// invariant).
func TestCloneVM_IdempotentOnMachineID(t *testing.T) {
	fake := newProxmoxFake()
	ctx := context.Background()
	spec := vmSpec{MachineID: "m1", InstanceType: "pve.small", Zone: "pve-1", IdempotencyToken: "op-1"}
	a, err := fake.CloneVM(ctx, spec)
	if err != nil {
		t.Fatalf("clone a: %v", err)
	}
	bb, err := fake.CloneVM(ctx, spec)
	if err != nil {
		t.Fatalf("clone b: %v", err)
	}
	if a.VMID != bb.VMID {
		t.Errorf("idempotent clone returned distinct VMs %d vs %d", a.VMID, bb.VMID)
	}
	if len(fake.vms) != 1 {
		t.Errorf("idempotent clone created %d VMs, want 1", len(fake.vms))
	}
}

// A retried Create whose VM has since been stopped out of band must adopt it
// powered on, not return a stopped host.
func TestCloneVM_AdoptsStoppedVM(t *testing.T) {
	fake := newProxmoxFake()
	ctx := context.Background()
	spec := vmSpec{MachineID: "m1", InstanceType: "pve.small", Zone: "pve-1", IdempotencyToken: "op-1"}
	first, err := fake.CloneVM(ctx, spec)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if !fake.setRunning(first.Node, first.VMID, false) {
		t.Fatalf("setRunning: VM %s/%d not found", first.Node, first.VMID)
	}
	again, err := fake.CloneVM(ctx, spec)
	if err != nil {
		t.Fatalf("adopt clone: %v", err)
	}
	if !again.Running {
		t.Error("retried Create returned a stopped VM (adopt did not power it on)")
	}
}

// Delete is idempotent: deleting an already-gone VM succeeds.
func TestDeleteVM_Idempotent(t *testing.T) {
	fake := newProxmoxFake()
	ctx := context.Background()
	if err := fake.DeleteVM(ctx, "pve-1", 12345); err != nil {
		t.Errorf("delete of absent VM should succeed, got %v", err)
	}
}

func TestOffering_CapacityType(t *testing.T) {
	for _, ok := range []string{"on_demand", "on-demand", "ondemand", ""} {
		if ct, err := (offering{Capacity: ok}).capacityType(); err != nil || ct != providerkit.CapacityOnDemand {
			t.Errorf("capacity_type %q: got (%v, %v), want (OnDemand, nil)", ok, ct, err)
		}
	}
	// spot, reserved, and bare_metal have no meaning for Proxmox and must be
	// rejected.
	for _, bad := range []string{"spot", "reserved", "bare_metal", "nonsense"} {
		if _, err := (offering{Capacity: bad}).capacityType(); err == nil {
			t.Errorf("expected capacity_type %q to be rejected", bad)
		}
	}
}

// The on-demand backend always implements Delete (the cloud profile).
func TestBackend_ImplementsDeleter(t *testing.T) {
	b, _ := newTestBackend(t, 4)
	if _, ok := providerkit.Backend(b).(providerkit.Deleter); !ok {
		t.Error("proxmox backend must advertise Deleter (cloud profile)")
	}
	s := newTestServer(t, b)
	resp, _ := s.List(context.Background(), &pb.ListFilter{States: []pb.MachineState{pb.MachineState_MACHINE_STATE_CONFIGURED}})
	_ = resp
}

// Delete on a CONFIGURED machine must be rejected with a non-FAILED_PRECONDITION
// code (FAILED_PRECONDITION is reserved for fencing). The kit enforces the
// transition matrix; this asserts the code is not a false fencing page.
func TestDelete_OnConfiguredRejected(t *testing.T) {
	b, _ := newTestBackend(t, 8)
	s := newTestServer(t, b)
	ctx := context.Background()
	resp, _ := s.List(ctx, &pb.ListFilter{States: []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE}, MaxResults: 1})
	id := resp.GetMachines()[0].GetId()
	if _, err := s.Create(ctx, &pb.CreateRequest{MachineId: id}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)
	if _, err := s.Configure(ctx, &pb.ConfigureRequest{MachineId: id, ClusterId: "c1", BootstrapBlob: []byte("j")}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED)
	_, err := s.Delete(ctx, &pb.DeleteRequest{MachineId: id})
	if err == nil {
		t.Fatal("Delete on CONFIGURED should be rejected")
	}
	if status.Code(err) == codes.FailedPrecondition {
		t.Errorf("Delete on CONFIGURED returned FAILED_PRECONDITION (reserved for fencing); want a different code, got %v", status.Code(err))
	}
}

func TestInstanceCatalog_Allocatable(t *testing.T) {
	c := newInstanceCatalog(nil, defaultTemplateVMID)
	got := c.allocatable("pve.large")
	if got["cpu"] != "8" || got["memory"] != "16Gi" {
		t.Errorf("pve.large allocatable = %v, want cpu=8 memory=16Gi", got)
	}
	if c.allocatable("nope") != nil {
		t.Error("unknown type should yield nil allocatable")
	}
}

func TestPricing_SyntheticAndOverride(t *testing.T) {
	catalog := newInstanceCatalog(nil, defaultTemplateVMID)
	pr := newPricing(catalog, 0, 0, nil)
	if got := pr.price("pve.large"); got <= 0 {
		t.Errorf("synthetic price = %v, want > 0", got)
	}
	pr2 := newPricing(catalog, 0, 0, map[string]float64{"pve.small": 0.05})
	if got := pr2.price("pve.small"); got != 0.05 {
		t.Errorf("override price = %v, want 0.05", got)
	}
}

func TestSplitHostRef(t *testing.T) {
	n, v, ok := splitHostRef("pve-1/12345")
	if !ok || n != "pve-1" || v != 12345 {
		t.Errorf("split = (%q,%d,%v)", n, v, ok)
	}
	if _, _, ok := splitHostRef("noslash"); ok {
		t.Error("ref with no slash should not split")
	}
	if _, _, ok := splitHostRef("pve-1/notanumber"); ok {
		t.Error("ref with non-numeric vmid should not split")
	}
}

func TestParseNodes(t *testing.T) {
	got, err := parseNodes("pve-1, pve-2 ,pve-3")
	if err != nil || len(got) != 3 || got[0] != "pve-1" || got[2] != "pve-3" {
		t.Fatalf("parseNodes: got %v, %v", got, err)
	}
	if c, err := parseNodes(""); err != nil || c != nil {
		t.Fatalf("empty: got %v, %v", c, err)
	}
	if _, err := parseNodes("a,a"); err == nil {
		t.Error("duplicate node should be rejected")
	}
	if _, err := parseNodes("a/b"); err == nil {
		t.Error("node with '/' should be rejected")
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	// 64 hex chars (32 bytes), colon-separated and uppercase both accepted.
	bare := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	colon := "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89"
	got, err := normalizeFingerprint(colon)
	if err != nil {
		t.Fatalf("normalizeFingerprint: %v", err)
	}
	if len(got) != 64 {
		t.Errorf("normalized length = %d, want 64", len(got))
	}
	if g2, err := normalizeFingerprint(bare); err != nil || g2 != bare {
		t.Errorf("bare fingerprint: got (%q, %v)", g2, err)
	}
	if _, err := normalizeFingerprint("tooshort"); err == nil {
		t.Error("short fingerprint should be rejected")
	}
	if _, err := normalizeFingerprint("zz" + bare[2:]); err == nil {
		t.Error("non-hex fingerprint should be rejected")
	}
}

func TestResolveBackendMode(t *testing.T) {
	if got := resolveBackendMode("auto", ""); got != "fake" {
		t.Errorf("auto+no-url = %q, want fake", got)
	}
	if got := resolveBackendMode("auto", "https://h:8006/api2/json"); got != "proxmox" {
		t.Errorf("auto+url = %q, want proxmox", got)
	}
	if got := resolveBackendMode("fake", "https://h:8006/api2/json"); got != "fake" {
		t.Errorf("explicit fake = %q, want fake", got)
	}
}

func TestValidateOfferingNodes(t *testing.T) {
	nodes := []string{"pve-1", "pve-2"}
	ok := []offering{{InstanceType: "pve.small", Zone: "pve-1", Count: 1}, {InstanceType: "pve.large", Zone: "pve-2", Count: 1}}
	if err := validateOfferingNodes(ok, nodes); err != nil {
		t.Errorf("all-configured offerings rejected: %v", err)
	}
	bad := []offering{{InstanceType: "pve.small", Zone: "pve-9", Count: 1}}
	if err := validateOfferingNodes(bad, nodes); err == nil {
		t.Error("offering on unconfigured node pve-9 should be rejected")
	}
	if err := validateOfferingNodes(ok, nil); err == nil {
		t.Error("real backend with no --nodes should be rejected")
	}
}

func TestDefaultOfferings_SmallSeedCountNoEmptyBuckets(t *testing.T) {
	for _, seed := range []int{1, 2, 3, 7} {
		offs := defaultOfferings(seed, "pve-1", "pve-2", []string{"pve.small", "pve.large"})
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

func TestNewProxmoxBackend_RejectsNonPositiveCount(t *testing.T) {
	catalog := newInstanceCatalog(nil, defaultTemplateVMID)
	pr := newPricing(catalog, 0, 0, nil)
	offs := []offering{{InstanceType: "pve.small", Zone: "pve-1", Capacity: "on_demand", Count: 0}}
	if _, err := newProxmoxBackend("proxmox-test", newProxmoxFake(), offs, catalog, pr, quietLogger()); err == nil {
		t.Error("expected a count==0 offering to be rejected at startup")
	}
}

func TestSanitizeTagAndName(t *testing.T) {
	if got := sanitizeTag("proxmox-test/OnDemand/pve.large/pve-1/007"); got != "proxmox-test-ondemand-pve-large-pve-1-007" {
		t.Errorf("sanitizeTag = %q", got)
	}
	if got := machineIDTag("a/b"); got != "bigfleet-a-b" {
		t.Errorf("machineIDTag = %q", got)
	}
	if got := parseMachineIDDescription(machineIDDescription("a/b/c", "op-1")); got != "a/b/c" {
		t.Errorf("parseMachineIDDescription round-trip = %q, want a/b/c", got)
	}
	if got := parseMachineIDDescription("no marker here"); got != "" {
		t.Errorf("parseMachineIDDescription(no marker) = %q, want empty", got)
	}
	name := cloneName("proxmox-test/OnDemand/pve.large/pve-1/007", "pve.large")
	if len(name) > 63 {
		t.Errorf("clone name %q exceeds 63 chars", name)
	}
}

// findInstance runs Describe and returns the instance with the given id.
func findInstance(t *testing.T, b *proxmoxBackend, ctx context.Context, id string) providerkit.Instance {
	t.Helper()
	got, err := b.Describe(ctx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for i := range got {
		if got[i].ID == id {
			return got[i]
		}
	}
	t.Fatalf("Describe did not return %s", id)
	return providerkit.Instance{}
}
