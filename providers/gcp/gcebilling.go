package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	cloudbilling "google.golang.org/api/cloudbilling/v1"
	"google.golang.org/api/option"
)

// computeEngineServiceID is the well-known Cloud Billing Catalog service id for
// Compute Engine. Its SKUs include the per-vCPU ("Instance Core") and per-GiB
// ("Instance Ram") on-demand rates this pricer composes machine-type prices from.
const computeEngineServiceID = "6F81-5844-456A"

// gceBillingPricer is the production pricingSource. It reads on-demand GCE core
// and memory SKUs from the Cloud Billing Catalog API
// (cloudbilling.googleapis.com/v1/services/{computeEngineServiceID}/skus) and
// composes a per-machine-type hourly USD price for a region: a predefined
// machine type is billed as (vCPU × core-rate) + (memory GiB × ram-rate) for its
// family in that region.
//
// It is only called by pricing.refresh (off the List hot path). The SKU
// catalogue is listed once per refresh and cached for skuTTL, so all offered
// types in one pass share a single List; a type whose family/region SKUs are not
// found returns an error and the caller keeps the pinned seed/fallback. Auth is
// ADC by default, or an API key.
type gceBillingPricer struct {
	svc    *cloudbilling.APIService
	caps   func(machineType string) (machineCapacity, bool)
	logger *slog.Logger
	skuTTL time.Duration

	mu     sync.Mutex
	skus   []*cloudbilling.Sku
	skusAt time.Time
}

// newGCEBillingPricer builds the live billing pricer. apiKey is optional: when
// empty, Application Default Credentials are used (the Cloud Billing Catalog is
// also reachable with an API key, which needs no service-account identity). caps
// resolves a machine type's vCPU/memory (the pinned machine-type table is a fine
// source — predefined types are billed off their nominal shape).
func newGCEBillingPricer(ctx context.Context, apiKey string, caps func(string) (machineCapacity, bool), logger *slog.Logger) (*gceBillingPricer, error) {
	var opts []option.ClientOption
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	svc, err := cloudbilling.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("cloud billing catalog service: %w", err)
	}
	return &gceBillingPricer{svc: svc, caps: caps, logger: logger, skuTTL: time.Minute}, nil
}

// OnDemandPriceUSD composes the on-demand USD/hour for machineType in region
// from its family's core + ram SKUs.
func (p *gceBillingPricer) OnDemandPriceUSD(ctx context.Context, machineType, region string) (float64, error) {
	mc, ok := p.caps(machineType)
	if !ok {
		return 0, fmt.Errorf("no known capacity (vCPU/memory) for machine type %q", machineType)
	}
	skus, err := p.listSKUs(ctx)
	if err != nil {
		return 0, err
	}
	core, ram, err := composeFamilyRates(skus, machineFamily(machineType), region)
	if err != nil {
		return 0, err
	}
	memGiB := float64(mc.MemMiB) / 1024
	price := float64(mc.VCPU)*core + memGiB*ram
	if price <= 0 {
		return 0, fmt.Errorf("composed non-positive price for %s in %s", machineType, region)
	}
	return price, nil
}

// listSKUs returns the Compute Engine SKU catalogue, cached for skuTTL so a
// refresh pass over many types lists it once.
func (p *gceBillingPricer) listSKUs(ctx context.Context) ([]*cloudbilling.Sku, error) {
	p.mu.Lock()
	if p.skus != nil && time.Since(p.skusAt) < p.skuTTL {
		cached := p.skus
		p.mu.Unlock()
		return cached, nil
	}
	p.mu.Unlock()

	var all []*cloudbilling.Sku
	parent := "services/" + computeEngineServiceID
	err := p.svc.Services.Skus.List(parent).CurrencyCode("USD").Pages(ctx, func(r *cloudbilling.ListSkusResponse) error {
		all = append(all, r.Skus...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list compute engine skus: %w", err)
	}
	p.mu.Lock()
	p.skus = all
	p.skusAt = time.Now()
	p.mu.Unlock()
	return all, nil
}

// composeFamilyRates finds the on-demand per-vCPU (core) and per-GiB (ram) hourly
// USD rates for a machine family in a region. Predefined GCE machine types are
// billed off these two SKUs ("<Family> Instance Core" / "<Family> Instance Ram",
// usageType OnDemand), so a type's price is vCPU×core + memGiB×ram. Pure (no
// I/O), so it is unit-testable against constructed SKUs. Returns an error when
// either SKU is missing for the family/region (the caller then keeps the seed).
func composeFamilyRates(skus []*cloudbilling.Sku, family, region string) (core, ram float64, err error) {
	corePrefix := family + " Instance Core"
	ramPrefix := family + " Instance Ram"
	var haveCore, haveRam bool
	for _, sku := range skus {
		if sku == nil || sku.Category == nil {
			continue
		}
		if sku.Category.ResourceFamily != "Compute" || sku.Category.UsageType != "OnDemand" {
			continue
		}
		if !containsString(sku.ServiceRegions, region) {
			continue
		}
		switch {
		case !haveCore && strings.HasPrefix(sku.Description, corePrefix):
			if r, ok := hourlyUSD(sku); ok {
				core, haveCore = r, true
			}
		case !haveRam && strings.HasPrefix(sku.Description, ramPrefix):
			if r, ok := hourlyUSD(sku); ok {
				ram, haveRam = r, true
			}
		}
		if haveCore && haveRam {
			break
		}
	}
	if !haveCore || !haveRam {
		return 0, 0, fmt.Errorf("no on-demand core/ram SKUs for family %q in %q", family, region)
	}
	return core, ram, nil
}

// hourlyUSD extracts the latest unit price (USD) from a SKU's pricing info. Core
// SKUs are priced per vCPU-hour and Ram per GiB-hour, so the unit price is the
// per-unit hourly rate the caller multiplies by vCPU / GiB.
func hourlyUSD(sku *cloudbilling.Sku) (float64, bool) {
	if len(sku.PricingInfo) == 0 {
		return 0, false
	}
	pe := sku.PricingInfo[len(sku.PricingInfo)-1].PricingExpression
	if pe == nil || len(pe.TieredRates) == 0 {
		return 0, false
	}
	tr := pe.TieredRates[len(pe.TieredRates)-1]
	if tr.UnitPrice == nil {
		return 0, false
	}
	return float64(tr.UnitPrice.Units) + float64(tr.UnitPrice.Nanos)/1e9, true
}

// machineFamily extracts the catalogue family token from a machine type, e.g.
// "n2-standard-4" -> "N2", "n2d-standard-8" -> "N2D", "c3-highmem-22" -> "C3".
// The trailing space in the SKU prefix ("N2 Instance Core") keeps "N2" from
// matching "N2D".
func machineFamily(machineType string) string {
	prefix, _, _ := strings.Cut(machineType, "-")
	return strings.ToUpper(prefix)
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

var _ pricingSource = (*gceBillingPricer)(nil)
