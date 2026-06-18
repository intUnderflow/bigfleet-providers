package providerkit

import (
	"errors"
	"math"
	"testing"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

func validInstance() Instance {
	return Instance{
		ID:                      "m-1",
		InstanceType:            "x.large",
		Zone:                    "z-a",
		CapacityType:            CapacityOnDemand,
		PricePerHour:            0.5,
		InterruptionProbability: 0,
	}
}

func TestValidate_FieldShape(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(*Instance)
		requireZone bool
		wantErr     bool
	}{
		{"valid on-demand", func(*Instance) {}, false, false},
		{"missing instance_type", func(in *Instance) { in.InstanceType = "" }, false, true},
		{"missing capacity_type", func(in *Instance) { in.CapacityType = CapacityUnspecified }, false, true},
		{"empty id", func(in *Instance) { in.ID = "" }, false, true},
		{"negative price", func(in *Instance) { in.PricePerHour = -1 }, false, true},
		{"NaN price", func(in *Instance) { in.PricePerHour = math.NaN() }, false, true},
		{"prob above 1", func(in *Instance) { in.InterruptionProbability = 1.5 }, false, true},
		{"prob below 0", func(in *Instance) { in.InterruptionProbability = -0.1 }, false, true},
		{"NaN prob", func(in *Instance) { in.InterruptionProbability = math.NaN() }, false, true},
		{"spot with zero prob", func(in *Instance) {
			in.CapacityType = CapacitySpot
			in.InterruptionProbability = 0
		}, false, true},
		{"spot with real prob", func(in *Instance) {
			in.CapacityType = CapacitySpot
			in.InterruptionProbability = 0.05
		}, false, false},
		{"zone required and missing", func(in *Instance) { in.Zone = "" }, true, true},
		{"zone not required and missing", func(in *Instance) { in.Zone = "" }, false, false},
		// Host-vs-state invariant.
		{"idle without host", func(in *Instance) { in.State = StateIdle }, false, true},
		{"idle with host", func(in *Instance) {
			in.State = StateIdle
			in.Host = HostRef{Provider: "p", Ref: "r"}
		}, false, false},
		{"speculative with host", func(in *Instance) {
			in.State = StateSpeculative
			in.Host = HostRef{Provider: "p", Ref: "r"}
		}, false, true},
		{"configured hint rejected", func(in *Instance) {
			in.State = StateConfigured
			in.Host = HostRef{Provider: "p", Ref: "r"}
		}, false, true},
		{"creating hint rejected", func(in *Instance) { in.State = StateCreating }, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInstance()
			tc.mutate(&in)
			err := in.validate(tc.requireZone)
			if tc.wantErr && err == nil {
				t.Errorf("validate(%+v) = nil, want error", in)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validate(%+v) = %v, want nil", in, err)
			}
			if tc.wantErr && err != nil && !errors.Is(err, ErrInvalidMachine) {
				t.Errorf("validate error not wrapping ErrInvalidMachine: %v", err)
			}
		})
	}
}

// A backend whose Describe returns an out-of-shape record must fail New
// loudly rather than serve a record the autoscaler would reject.
func TestNew_RejectsInvalidSeed(t *testing.T) {
	b := &fakeBackend{seed: []Instance{{
		ID:           "bad",
		State:        StateSpeculative,
		InstanceType: "", // missing
		CapacityType: CapacityOnDemand,
	}}}
	_, err := New(b, NewMemStore(), quietOptions())
	if err == nil {
		t.Fatal("New must fail when Describe returns an invalid record")
	}
	if !errors.Is(err, ErrInvalidMachine) {
		t.Errorf("New error = %v, want wrapping ErrInvalidMachine", err)
	}
}

func TestNew_SpotSeedMustDeclareProbability(t *testing.T) {
	b := &fakeBackend{seed: speculativeSeed(2, CapacitySpot, 0)} // prob 0 invalid for SPOT
	_, err := New(b, NewMemStore(), quietOptions())
	if err == nil {
		t.Fatal("New must reject a SPOT seed with interruption_probability 0")
	}
}

// CostFieldBounds mirror: every emitted machine has price ≥ 0 and prob in
// [0,1].
func TestEmittedMachines_CostFieldsInBounds(t *testing.T) {
	b := &fakeBackend{seed: speculativeSeed(3, CapacitySpot, 0.07)}
	s, err := New(b, NewMemStore(), quietOptions())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, _ := s.List(bg(), &pb.ListFilter{})
	for _, m := range resp.GetMachines() {
		if p := m.GetPricePerHour(); math.IsNaN(p) || p < 0 {
			t.Errorf("machine %s: price_per_hour %v out of bounds", m.GetId(), p)
		}
		if ip := m.GetInterruptionProbability(); math.IsNaN(ip) || ip < 0 || ip > 1 {
			t.Errorf("machine %s: interruption_probability %v out of bounds", m.GetId(), ip)
		}
	}
}
