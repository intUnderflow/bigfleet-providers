package main

import (
	"math"
	"testing"

	cloudbilling "google.golang.org/api/cloudbilling/v1"
)

// sku is a tiny constructor for an on-demand Compute SKU at a per-unit hourly
// rate, for composeFamilyRates tests.
func sku(desc string, region string, units int64, nanos int64) *cloudbilling.Sku {
	return &cloudbilling.Sku{
		Description:    desc,
		ServiceRegions: []string{region},
		Category: &cloudbilling.Category{
			ResourceFamily: "Compute",
			UsageType:      "OnDemand",
		},
		PricingInfo: []*cloudbilling.PricingInfo{{
			PricingExpression: &cloudbilling.PricingExpression{
				TieredRates: []*cloudbilling.TierRate{{
					UnitPrice: &cloudbilling.Money{CurrencyCode: "USD", Units: units, Nanos: nanos},
				}},
			},
		}},
	}
}

func TestMachineFamily(t *testing.T) {
	cases := map[string]string{
		"n2-standard-4":  "N2",
		"n2d-standard-8": "N2D",
		"c3-highmem-22":  "C3",
		"e2-standard-2":  "E2",
	}
	for in, want := range cases {
		if got := machineFamily(in); got != want {
			t.Errorf("machineFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestComposeFamilyRates_OnDemandCoreAndRam(t *testing.T) {
	// Core $0.031611/vCPU-hr, Ram $0.004237/GiB-hr for N2 in us-central1.
	skus := []*cloudbilling.Sku{
		sku("N2 Instance Core running in Americas", "us-central1", 0, 31_611_000),
		sku("N2 Instance Ram running in Americas", "us-central1", 0, 4_237_000),
		// Decoys the matcher must skip: wrong family (prefix guard), wrong region.
		sku("N2D Instance Core running in Americas", "us-central1", 0, 99_000_000),
		sku("N2 Instance Core running in EMEA", "europe-west1", 0, 99_000_000),
	}
	core, ram, err := composeFamilyRates(skus, "N2", "us-central1")
	if err != nil {
		t.Fatalf("composeFamilyRates: %v", err)
	}
	if core != 0.031611 {
		t.Errorf("core rate = %v, want 0.031611", core)
	}
	if ram != 0.004237 {
		t.Errorf("ram rate = %v, want 0.004237", ram)
	}

	// vCPU×core + GiB×ram == on-demand hourly price (n2-standard-4 = 4 vCPU/16GiB).
	mc := machineTypeTable["n2-standard-4"]
	price := float64(mc.VCPU)*core + float64(mc.MemMiB)/1024*ram
	if want := 4*0.031611 + 16*0.004237; math.Abs(price-want) > 1e-9 {
		t.Errorf("composed n2-standard-4 price = %v, want %v", price, want)
	}
}

func TestComposeFamilyRates_MissingRamFails(t *testing.T) {
	// Only a core SKU: a family/region with no ram SKU must error, so the caller
	// keeps the pinned seed/fallback rather than under-pricing.
	skus := []*cloudbilling.Sku{
		sku("N2 Instance Core running in Americas", "us-central1", 0, 31_611_000),
	}
	if _, _, err := composeFamilyRates(skus, "N2", "us-central1"); err == nil {
		t.Fatal("expected an error when the ram SKU is missing")
	}
}
