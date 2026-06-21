package main

import (
	"testing"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

func TestPricing_PinnedTableConvertsToUSD(t *testing.T) {
	p := newPricing(1.10, quietLogger())
	off := offering{Plan: "2xCPU-4GB", Zone: "fi-hel1"}
	want := onDemandEURHourly["2xCPU-4GB"] * 1.10
	if got := p.price(off, providerkit.CapacityOnDemand); got != want {
		t.Errorf("price(2xCPU-4GB) = %v, want %v", got, want)
	}
}

func TestPricing_OperatorOverrideWins(t *testing.T) {
	p := newPricing(defaultEURtoUSD, quietLogger())
	off := offering{Plan: "2xCPU-4GB", Zone: "fi-hel1", PriceUSDPerHour: 0.123}
	if got := p.price(off, providerkit.CapacityOnDemand); got != 0.123 {
		t.Errorf("override price = %v, want 0.123", got)
	}
}

func TestPricing_UnknownPlanIsZero(t *testing.T) {
	p := newPricing(defaultEURtoUSD, quietLogger())
	off := offering{Plan: "no-such-plan", Zone: "fi-hel1"}
	if got := p.price(off, providerkit.CapacityOnDemand); got != 0 {
		t.Errorf("unknown plan price = %v, want 0", got)
	}
}

func TestPlanResolver_AllocatableDistinctFromMemoryUnits(t *testing.T) {
	r := newPlanResolver(newUpcloudFake(), quietLogger())
	alloc := r.allocatable("2xCPU-4GB")
	if alloc["cpu"] != "2" {
		t.Errorf("cpu = %q, want 2", alloc["cpu"])
	}
	if alloc["memory"] != "4Gi" {
		t.Errorf("memory = %q, want 4Gi", alloc["memory"])
	}
	if r.allocatable("totally-unknown-plan") != nil {
		t.Error("unknown plan should yield nil allocatable")
	}
}
