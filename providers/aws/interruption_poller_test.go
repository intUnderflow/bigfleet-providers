package main

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

func TestProbabilityForEvent(t *testing.T) {
	if p, ok := probabilityForEvent("EC2 Spot Instance Interruption Warning"); !ok || p < 0.99 {
		t.Errorf("interruption warning -> %v,%v; want >=0.99,true", p, ok)
	}
	if p, ok := probabilityForEvent("EC2 Instance Rebalance Recommendation"); !ok || p <= 0 || p >= 1 {
		t.Errorf("rebalance -> %v,%v; want (0,1),true", p, ok)
	}
	if _, ok := probabilityForEvent("EC2 Instance State-change Notification"); ok {
		t.Error("unrelated event should be ignored")
	}
}

// firstSpotSlot returns the id + instance type of a SPOT offering slot.
func firstSpotSlot(t *testing.T, b *awsBackend) (string, string) {
	t.Helper()
	for _, s := range b.speculativeSlots() {
		if s.CapacityType == providerkit.CapacitySpot {
			return s.ID, s.InstanceType
		}
	}
	t.Fatal("no spot slot in default offerings")
	return "", ""
}

func TestInterruptionPoller_RaisesObservedProbability(t *testing.T) {
	b, fake := newTestBackend(t, 8)
	slotID, instType := firstSpotSlot(t, b)
	// Launch an instance tagged with the slot's machine id (as CreateInstance would).
	inst, err := fake.RunInstance(context.Background(), runSpec{MachineID: slotID, InstanceType: instType, Zone: "us-east-1a", Capacity: "spot"})
	if err != nil {
		t.Fatalf("seed instance: %v", err)
	}

	m := newMetrics()
	p := &interruptionPoller{backend: b, m: m, logger: quietLogger()}

	before := b.interruption.probability(slotID, instType, providerkit.CapacitySpot)
	body := `{"detail-type":"EC2 Spot Instance Interruption Warning","detail":{"instance-id":"` + inst.InstanceID + `"}}`
	p.handle(context.Background(), body)

	after := b.interruption.probability(slotID, instType, providerkit.CapacitySpot)
	if after <= before || after < 0.99 {
		t.Errorf("observed interruption probability not raised: before=%v after=%v", before, after)
	}
	if testutil.ToFloat64(m.interrupts) != 1 {
		t.Error("spot_interruptions metric not incremented")
	}
}

func TestInterruptionPoller_IgnoresUnrelatedAndUnknown(t *testing.T) {
	b, _ := newTestBackend(t, 4)
	p := &interruptionPoller{backend: b, m: newMetrics(), logger: quietLogger()}

	// Unparseable body, unrelated event, and an unknown instance must all no-op.
	p.handle(context.Background(), "not json")
	p.handle(context.Background(), `{"detail-type":"EC2 Instance State-change Notification","detail":{"instance-id":"i-x"}}`)
	p.handle(context.Background(), `{"detail-type":"EC2 Spot Instance Interruption Warning","detail":{"instance-id":"i-not-managed"}}`)
	if testutil.ToFloat64(p.m.interrupts) != 0 {
		t.Error("no interruption should have been recorded")
	}
}
