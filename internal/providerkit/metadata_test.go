package providerkit

import (
	"context"
	"maps"
	"testing"
	"time"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

// shard_metadata is STORE AND ECHO, NEVER INTERPRET: kept verbatim from
// Configure, echoed byte-for-byte on every Get/List while the binding
// exists, and cleared together with the cluster when a Drain completes.

func TestShardMetadata_EchoedVerbatimOnGetAndList(t *testing.T) {
	s, _ := newTestServer(t, 4)
	id := firstSpeculative(t, s)
	create(t, s, id)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)

	md := map[string]string{
		"bigfleet.lucy.sh/assigned-priority": "900000",
		"bigfleet.lucy.sh/assigned-group":    "topology.bigfleet/rack\x00gang-7",
		"x-unknown/opaque":                   "echo-me",
		"x-unknown/empty":                    "",
	}
	configure(t, s, id, "cluster-md", md)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED, 2*time.Second)

	got := getMachine(t, s, id)
	if got.GetCluster() != "cluster-md" {
		t.Errorf("Get cluster = %q, want cluster-md", got.GetCluster())
	}
	if !maps.Equal(got.GetShardMetadata(), md) {
		t.Errorf("Get shard_metadata = %v, want verbatim %v", got.GetShardMetadata(), md)
	}

	resp, _ := s.List(bg(), &pb.ListFilter{States: []pb.MachineState{pb.MachineState_MACHINE_STATE_CONFIGURED}})
	found := false
	for _, m := range resp.GetMachines() {
		if m.GetId() != id {
			continue
		}
		found = true
		if !maps.Equal(m.GetShardMetadata(), md) {
			t.Errorf("List shard_metadata = %v, want verbatim %v", m.GetShardMetadata(), md)
		}
	}
	if !found {
		t.Errorf("List(CONFIGURED) did not return %s", id)
	}
}

// The binding must be visible for the WHOLE Configuring span, not just once
// the backend completes — a CONFIGURING record with an empty cluster trips
// the shard's structural-rejection tripwire. This pins the fix for a slow
// (non-instant) backend.
func TestShardMetadata_VisibleDuringConfiguring(t *testing.T) {
	s, b := newTestServer(t, 4)
	id := firstSpeculative(t, s)
	create(t, s, id)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)

	release := make(chan struct{})
	b.setConfigure(func(ctx context.Context, _ ConfigureInstanceRequest) error {
		<-release // hold the machine in Configuring until the test releases it
		return nil
	})
	md := map[string]string{"bigfleet.lucy.sh/assigned-priority": "7", "x/op": "keep"}
	ack := configure(t, s, id, "cluster-slow", md)

	// The ack itself, returned while the machine is still Configuring, must
	// already carry the binding.
	if ack.GetMachine().GetState() != pb.MachineState_MACHINE_STATE_CONFIGURING {
		t.Fatalf("ack state = %s, want CONFIGURING", ack.GetMachine().GetState())
	}
	if ack.GetMachine().GetCluster() != "cluster-slow" {
		t.Errorf("ack cluster empty during CONFIGURING: %q", ack.GetMachine().GetCluster())
	}
	if !maps.Equal(ack.GetMachine().GetShardMetadata(), md) {
		t.Errorf("ack shard_metadata empty during CONFIGURING: %v", ack.GetMachine().GetShardMetadata())
	}

	// And a Get mid-Configuring shows it too.
	g := getMachine(t, s, id)
	if g.GetState() != pb.MachineState_MACHINE_STATE_CONFIGURING {
		t.Fatalf("Get state = %s, want CONFIGURING", g.GetState())
	}
	if g.GetCluster() != "cluster-slow" || !maps.Equal(g.GetShardMetadata(), md) {
		t.Errorf("binding not visible during CONFIGURING: cluster=%q md=%v", g.GetCluster(), g.GetShardMetadata())
	}

	close(release)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED, 2*time.Second)
}

func TestShardMetadata_NotAliasedToCallerMap(t *testing.T) {
	s, _ := newTestServer(t, 4)
	id := firstSpeculative(t, s)
	create(t, s, id)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)

	md := map[string]string{"k": "v"}
	configure(t, s, id, "c", md)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED, 2*time.Second)

	// Mutating the caller's map after Configure must not reach the stored
	// record — the kit copies it verbatim.
	md["k"] = "TAMPERED"
	md["new"] = "x"
	got := getMachine(t, s, id)
	if got.GetShardMetadata()["k"] != "v" || len(got.GetShardMetadata()) != 1 {
		t.Errorf("stored metadata aliased the caller's map: %v", got.GetShardMetadata())
	}
}

func TestShardMetadata_ClearedWithBindingOnDrain(t *testing.T) {
	s, _ := newTestServer(t, 4)
	id := firstSpeculative(t, s)
	create(t, s, id)
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)
	configure(t, s, id, "c", map[string]string{"bigfleet.lucy.sh/assigned-priority": "1000000"})
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_CONFIGURED, 2*time.Second)

	if _, err := s.Drain(bg(), &pb.DrainRequest{MachineId: id, GracePeriodSeconds: 5}); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	waitState(t, s, id, pb.MachineState_MACHINE_STATE_IDLE, 2*time.Second)

	got := getMachine(t, s, id)
	if got.GetCluster() != "" {
		t.Errorf("cluster survived Drain: %q", got.GetCluster())
	}
	if len(got.GetShardMetadata()) != 0 {
		t.Errorf("shard_metadata survived Drain: %v", got.GetShardMetadata())
	}
}
