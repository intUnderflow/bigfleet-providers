package main

import (
	"context"
	"errors"
	"testing"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// stubPriceClient is an ec2Client whose only meaningful method is
// OnDemandPricesUSD, for exercising the on-demand refresh path in isolation.
type stubPriceClient struct {
	*ec2Fake
	prices map[string]float64
	err    error
}

func (s *stubPriceClient) OnDemandPricesUSD(_ context.Context, instanceTypes []string) (map[string]float64, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]float64)
	for _, t := range instanceTypes {
		if v, ok := s.prices[t]; ok {
			out[t] = v
		}
	}
	return out, nil
}

// A successful on-demand refresh becomes the source of truth, overriding the
// pinned seed value that the cold cache served.
func TestPricing_RefreshOnDemandUpdatesPrice(t *testing.T) {
	stub := &stubPriceClient{ec2Fake: newEC2Fake(), prices: map[string]float64{"m6i.large": 0.111}}
	p := newPricing("us-east-1", stub, quietLogger())

	// Cold: the pinned seed.
	if got := p.price("m6i.large", "us-east-1a", providerkit.CapacityOnDemand); got != onDemandUSEast1["m6i.large"] {
		t.Fatalf("cold on-demand = %v, want seed %v", got, onDemandUSEast1["m6i.large"])
	}
	if failed := p.refreshOnDemand(context.Background(), []string{"m6i.large"}); failed != 0 {
		t.Fatalf("refreshOnDemand reported %d failures", failed)
	}
	if got := p.price("m6i.large", "us-east-1a", providerkit.CapacityOnDemand); got != 0.111 {
		t.Fatalf("refreshed on-demand = %v, want 0.111 (live wins)", got)
	}
}

// A refresh that fails, or that omits / zeroes a type, must keep the seed
// fallback — a priced offering must never drop to 0 (0 wins the cost signal).
func TestPricing_RefreshOnDemandFailClosed(t *testing.T) {
	// Fetch error: seed retained, one failure reported.
	errStub := &stubPriceClient{ec2Fake: newEC2Fake(), err: errors.New("offer fetch failed")}
	pe := newPricing("us-east-1", errStub, quietLogger())
	if failed := pe.refreshOnDemand(context.Background(), []string{"m6i.large"}); failed != 1 {
		t.Fatalf("failed refresh reported %d, want 1", failed)
	}
	if got := pe.price("m6i.large", "us-east-1a", providerkit.CapacityOnDemand); got != onDemandUSEast1["m6i.large"] {
		t.Fatalf("after failed refresh = %v, want seed %v", got, onDemandUSEast1["m6i.large"])
	}

	// Zero / missing in the live result must not overwrite the seed.
	zeroStub := &stubPriceClient{ec2Fake: newEC2Fake(), prices: map[string]float64{"m6i.large": 0}}
	pz := newPricing("us-east-1", zeroStub, quietLogger())
	if failed := pz.refreshOnDemand(context.Background(), []string{"m6i.large"}); failed != 0 {
		t.Fatalf("refresh reported %d failures", failed)
	}
	if got := pz.price("m6i.large", "us-east-1a", providerkit.CapacityOnDemand); got != onDemandUSEast1["m6i.large"] {
		t.Fatalf("zero live price = %v, want seed kept %v (never 0)", got, onDemandUSEast1["m6i.large"])
	}
}

func TestNewPricing_RegionTableSelection(t *testing.T) {
	const onDemandM6iLarge = 0.096

	cases := []struct {
		name   string
		region string
	}{
		{"baseline region", "us-east-1"},
		{"tabulated peer region", "us-west-2"},
		{"untabulated region falls back to baseline", "eu-west-1"},
		{"empty region falls back quietly", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPricing(tc.region, newEC2Fake(), quietLogger())
			if got := p.price("m6i.large", "zone-a", providerkit.CapacityOnDemand); got != onDemandM6iLarge {
				t.Fatalf("on-demand m6i.large in %q = %v, want %v (baseline)", tc.region, got, onDemandM6iLarge)
			}
		})
	}
}

func TestNewPricing_SpotColdCacheFraction(t *testing.T) {
	p := newPricing("us-east-1", newEC2Fake(), quietLogger())
	// Cold spot cache returns a conservative fraction of on-demand so spot still
	// ranks below on-demand without ever reading 0.
	want := 0.3 * 0.096
	if got := p.price("m6i.large", "us-east-1a", providerkit.CapacitySpot); got != want {
		t.Fatalf("cold spot m6i.large = %v, want %v", got, want)
	}
}

func TestNewPricing_BareMetalIsFree(t *testing.T) {
	p := newPricing("us-east-1", newEC2Fake(), quietLogger())
	if got := p.price("m6i.large", "us-east-1a", providerkit.CapacityBareMetal); got != 0 {
		t.Fatalf("bare-metal price = %v, want 0 (already paid for)", got)
	}
}
