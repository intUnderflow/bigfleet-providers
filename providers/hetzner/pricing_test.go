package main

import (
	"context"
	"testing"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

func TestPricing_EURtoUSDConversion(t *testing.T) {
	logger := quietLogger()
	p := newPricing(2.0, newHCloudFake(), logger) // exaggerated rate for an exact assertion
	// Cold cache falls back to the pinned EUR table * rate.
	want := onDemandEURHourly["cx22"] * 2.0
	if got := p.price("cx22", "nbg1", providerkit.CapacityOnDemand); got != want {
		t.Errorf("cold cache price = %v, want %v", got, want)
	}
}

func TestPricing_RefreshUsesClient(t *testing.T) {
	fake := newHCloudFake()
	fake.priceUSD = 0.123
	p := newPricing(1.0, fake, quietLogger())
	if failed := p.refresh(context.Background(), []pricePair{{serverType: "cx22", location: "nbg1"}}); failed != 0 {
		t.Fatalf("refresh reported %d failures", failed)
	}
	if got := p.price("cx22", "nbg1", providerkit.CapacityOnDemand); got != 0.123 {
		t.Errorf("refreshed price = %v, want 0.123", got)
	}
}

func TestServerTypeResolver_AllocatableDensity(t *testing.T) {
	r := newServerTypeResolver(newHCloudFake(), quietLogger())
	alloc := r.allocatable("cx22")
	if alloc["cpu"] != "2" || alloc["memory"] != "4Gi" {
		t.Errorf("cx22 allocatable = %v, want cpu=2 memory=4Gi", alloc)
	}
	// resources (per-replica) must be distinct from allocatable so density > 1.
	if alloc["cpu"] == "1" {
		t.Error("allocatable cpu must reflect hardware, not the per-replica request")
	}
	if r.allocatable("totally-unknown") != nil {
		t.Error("unknown server type should resolve to nil allocatable")
	}
}

func TestMachineIDLabelRoundTrip(t *testing.T) {
	for _, id := range []string{"hetzner-nbg1/OnDemand/cx22/nbg1/000", "simple", ""} {
		if got := decodeMachineID(encodeMachineID(id)); got != id {
			t.Errorf("round-trip %q -> %q", id, got)
		}
	}
}
