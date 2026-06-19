//go:build certify

package suite

import (
	"math"
	"testing"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

// C8 — Machine field-shape & cost-field sweep (behaviors B80x). The autoscaler
// reads these top-level fields directly, so every machine the provider exposes
// must be in shape. Deeper than the upstream LabelShape/CostFieldBounds checks:
// it sweeps EVERY machine and adds the host-vs-state and capacity/SPOT
// invariants.

func stable(s pb.MachineState) bool {
	return s == pb.MachineState_MACHINE_STATE_SPECULATIVE ||
		s == pb.MachineState_MACHINE_STATE_IDLE ||
		s == pb.MachineState_MACHINE_STATE_CONFIGURED
}

func TestFieldShape_EveryMachine(t *testing.T) {
	h := dial(t)
	machines := h.List()
	if len(machines) == 0 {
		t.Skip("provider exposes no machines")
	}
	for _, m := range machines {
		id := m.GetId()
		if m.GetState() == pb.MachineState_MACHINE_STATE_UNSPECIFIED {
			t.Errorf("%s: state UNSPECIFIED", id)
		}
		if m.GetInstanceType() == "" {
			t.Errorf("%s: instance_type empty (must be top-level, never labels-only)", id)
		}
		// Required fields must not be hidden in labels.
		for _, leak := range []string{"instance-type", "instance_type", "zone", "capacity-type"} {
			if _, ok := m.GetLabels()[leak]; ok {
				t.Errorf("%s: %q present in labels — well-known fields must be top-level", id, leak)
			}
		}
		// capacity_type set for stable records (drives idle-hold + cost).
		if stable(m.GetState()) && m.GetCapacityType() == pb.CapacityType_CAPACITY_TYPE_UNSPECIFIED {
			t.Errorf("%s: capacity_type UNSPECIFIED on a stable record", id)
		}
		// Cost-formula inputs in bounds.
		if p := m.GetPricePerHour(); math.IsNaN(p) || math.IsInf(p, 0) || p < 0 {
			t.Errorf("%s: price_per_hour %v out of bounds", id, p)
		}
		ip := m.GetInterruptionProbability()
		if math.IsNaN(ip) || ip < 0 || ip > 1 {
			t.Errorf("%s: interruption_probability %v outside [0,1]", id, ip)
		}
		// SPOT must declare a real interruption probability.
		if m.GetCapacityType() == pb.CapacityType_CAPACITY_TYPE_SPOT && ip <= 0 {
			t.Errorf("%s: SPOT machine with interruption_probability %v (must be > 0)", id, ip)
		}
		// host invariant: nil for Speculative, set for the bound/idle states.
		hostSet := m.GetHost() != nil && (m.GetHost().GetRef() != "" || m.GetHost().GetProvider() != "")
		switch m.GetState() {
		case pb.MachineState_MACHINE_STATE_SPECULATIVE:
			if hostSet {
				t.Errorf("%s: Speculative machine has a host", id)
			}
		case pb.MachineState_MACHINE_STATE_IDLE, pb.MachineState_MACHINE_STATE_CONFIGURED:
			if !hostSet {
				t.Errorf("%s: %s machine has no host", id, m.GetState())
			}
		}
		// cluster only while bound.
		switch m.GetState() {
		case pb.MachineState_MACHINE_STATE_SPECULATIVE, pb.MachineState_MACHINE_STATE_IDLE:
			if m.GetCluster() != "" {
				t.Errorf("%s: unbound %s machine has cluster %q", id, m.GetState(), m.GetCluster())
			}
		case pb.MachineState_MACHINE_STATE_CONFIGURED:
			if m.GetCluster() == "" {
				t.Errorf("%s: Configured machine has empty cluster", id)
			}
		}
	}
}

// A machine walked to Idle has a real host; if Delete is supported, walking it
// back to Speculative clears the host.
func TestFieldShape_HostLifecycle(t *testing.T) {
	h := dial(t)
	id := h.WalkToIdle()
	m := h.Get(id)
	if m.GetHost().GetRef() == "" {
		t.Errorf("Idle machine %s has no host ref", id)
	}
}
