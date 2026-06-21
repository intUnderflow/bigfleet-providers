package main

import (
	"context"
	"testing"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// Bare-metal capacity is owned hardware: price is always 0, regardless of any
// cached or pinned value.
func TestPricing_BareMetalIsZero(t *testing.T) {
	p := newPricing(defaultEURtoUSD, newSCWFake(), quietLogger())
	if got := p.price("EM-A210R-HDD", "fr-par-1", providerkit.CapacityBareMetal); got != 0 {
		t.Errorf("bare-metal price = %v, want 0", got)
	}
}

// A cold cache falls back to the pinned EUR table converted to USD; a warm cache
// returns the refreshed value.
func TestPricing_ColdFallbackAndRefresh(t *testing.T) {
	fake := newSCWFake()
	p := newPricing(defaultEURtoUSD, fake, quietLogger())

	wantCold := onDemandEURHourly["DEV1-S"] * defaultEURtoUSD
	if got := p.price("DEV1-S", "fr-par-1", providerkit.CapacityOnDemand); got != wantCold {
		t.Errorf("cold price = %v, want pinned %v", got, wantCold)
	}

	if failed := p.refresh(context.Background(), []pricePair{{commercialType: "DEV1-S", zone: "fr-par-1"}}); failed != 0 {
		t.Fatalf("refresh reported %d failures", failed)
	}
	if got := p.price("DEV1-S", "fr-par-1", providerkit.CapacityOnDemand); got != fake.priceUSD {
		t.Errorf("warm price = %v, want refreshed %v", got, fake.priceUSD)
	}
}

// allocatable renders Kubernetes-style quantity strings and exposes GPUs for GPU
// commercial types.
func TestCommercialCapacity_Allocatable(t *testing.T) {
	got := commercialCapacity{VCPU: 8, MemMiB: gib(16), GPUs: 0}.allocatable()
	if got["cpu"] != "8" || got["memory"] != "16Gi" {
		t.Errorf("allocatable = %v, want cpu=8 memory=16Gi", got)
	}
	if _, ok := got["nvidia.com/gpu"]; ok {
		t.Errorf("non-GPU type must not declare nvidia.com/gpu: %v", got)
	}
	gpu := commercialCapacity{VCPU: 24, MemMiB: gib(240), GPUs: 1}.allocatable()
	if gpu["nvidia.com/gpu"] != "1" {
		t.Errorf("GPU type allocatable = %v, want nvidia.com/gpu=1", gpu)
	}
}

// GPU and Arm64 commercial types get the right extra labels (top-level fields
// stay top-level; labels carry only the extras).
func TestSlotLabels_GPUAndArch(t *testing.T) {
	gpu := slotLabels(offering{CommercialType: "RENDER-S"})
	if gpu["accelerator-type"] == "" {
		t.Errorf("GPU type missing accelerator-type label: %v", gpu)
	}
	arm := slotLabels(offering{CommercialType: "COPARM1-4C-16G"})
	if arm["kubernetes.io/arch"] != "arm64" {
		t.Errorf("Arm64 type arch label = %v, want arm64", arm)
	}
}
