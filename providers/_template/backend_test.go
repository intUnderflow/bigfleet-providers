package main

import (
	"context"
	"testing"
	"time"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"

	"github.com/intUnderflow/bigfleet-providers/internal/providerkit"
)

func newServer(t *testing.T) *providerkit.Server {
	t.Helper()
	b := &templateBackend{providerName: "example", seeds: seedInventory(8, "example")}
	s, err := providerkit.New(b, providerkit.NewMemStore(), providerkit.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func waitState(t *testing.T, s *providerkit.Server, id string, want pb.MachineState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m, err := s.Get(context.Background(), &pb.MachineRef{Id: id})
		if err == nil && m.GetState() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("machine %s did not reach %s in time", id, want)
}

// The seed the template ships must be in shape — otherwise providerkit.New
// would have rejected it (so reaching here already proves validity), but we
// also assert the SPOT slots declare a real interruption probability.
func TestSeedInventoryIsValid(t *testing.T) {
	s := newServer(t)
	resp, err := s.List(context.Background(), &pb.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.GetMachines()) != 8 {
		t.Fatalf("seeded %d machines, want 8", len(resp.GetMachines()))
	}
	for _, m := range resp.GetMachines() {
		if m.GetInstanceType() == "" {
			t.Errorf("%s: empty instance_type", m.GetId())
		}
		if m.GetCapacityType() == pb.CapacityType_CAPACITY_TYPE_SPOT && m.GetInterruptionProbability() <= 0 {
			t.Errorf("%s: SPOT slot with zero interruption_probability", m.GetId())
		}
	}
}

// The template, wired through providerkit, walks a machine through the full
// lifecycle — proving the kit + template are contract-correct end-to-end
// without needing a network.
func TestTemplateFullLifecycle(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	resp, _ := s.List(ctx, &pb.ListFilter{States: []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE}, MaxResults: 1})
	id := resp.GetMachines()[0].GetId()

	if _, err := s.Create(ctx, &pb.CreateRequest{MachineId: id}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE)

	if _, err := s.Configure(ctx, &pb.ConfigureRequest{MachineId: id, ClusterId: "c", BootstrapBlob: []byte("x")}); err != nil {
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
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_SPECULATIVE)
}
