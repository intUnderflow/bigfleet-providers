package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// priceSource fetches live OCI Compute prices for the price refresher. It is the
// only place a live price lookup happens; the result is folded into the
// mutex-guarded price table that List/Describe read, so the hot path never calls
// a source.
//
// Two implementations ship:
//   - ociPriceList queries the public OCI cost-estimator / price-list API
//     (apexapps.oracle.com/.../cetools, no credentials) — the production source.
//   - fakePriceSource returns deterministic prices with no network call, backing
//     unit tests and the credential-free conformance/certification run.
type priceSource interface {
	// Fetch returns the current live OCI Compute prices: per-family flexible
	// rates (per-OCPU-hour + per-GB-hour) and whole-instance fixed-shape rates.
	// Shapes/families it cannot price are simply omitted, so the caller keeps the
	// seed/fallback for those rather than emitting a zero price.
	Fetch(ctx context.Context) (livePrices, error)
}

// livePrices is one snapshot of OCI Compute prices, shaped exactly like the
// seed table's tunable fields so the refresher can merge it in place.
type livePrices struct {
	flexRates   map[string]rate    // flexible-shape family -> per-OCPU/per-GB USD rate
	fixedHourly map[string]float64 // fixed shape -> whole-instance USD/hour
}

// defaultOCIPriceListURL is the public OCI cost-estimator products endpoint. It
// returns the OCI product catalogue (part number -> hourly USD) with no
// credentials. PAY_AS_YOU_GO is the on-demand list price.
const defaultOCIPriceListURL = "https://apexapps.oracle.com/pls/apex/cetools/api/v1/products/?currencyCode=USD"

// OCI prices Compute as metered SKUs ("part numbers"), not per shape: a flexible
// shape bills a per-OCPU SKU plus a per-GB-memory SKU; a GPU shape bills a
// per-GPU SKU; a bare-metal Standard shape bills the same per-OCPU/per-GB SKUs as
// its VM family across all of its (fixed) cores. These tables map the shapes this
// provider offers onto those SKUs. Refresh the part numbers from the live
// catalogue (the displayName makes each obvious) when OCI revises a family.

// flexSKU maps a flexible-shape family to its per-OCPU and per-GB OCI part
// numbers. (A1's SKUs price at 0 on the public list — the always-free Ampere
// tier — so the refresher keeps the seed rate for it rather than emitting $0.)
var flexSKU = map[string]struct{ ocpu, mem string }{
	"VM.Standard.E5.Flex": {"B97384", "B97385"},   // Compute - Standard - E5
	"VM.Standard.E4.Flex": {"B93113", "B93114"},   // Compute - Standard - E4
	"VM.Standard3.Flex":   {"B94176", "B94177"},   // Compute - Standard - X9 (Intel)
	"VM.Optimized3.Flex":  {"B93311", "B93312"},   // Compute - Optimized - X9 (Intel)
	"VM.Standard.A1.Flex": {"B93297", "B93298"},   // Compute - Standard - A1 (Ampere)
	"VM.Standard.A2.Flex": {"B109529", "B109530"}, // Compute - Standard - A2 (Ampere)
}

// gpuSKU maps a GPU shape to its per-GPU-hour OCI part number; the whole-instance
// price is that rate times the shape's GPU count (from shapeTable).
var gpuSKU = map[string]string{
	"VM.GPU.A10.1":     "B95909", // Compute - GPU - A10  ($/GPU/hr)
	"VM.GPU.A10.2":     "B95909",
	"BM.GPU.A100-v2.8": "B95907", // Compute - GPU - A100 - v2 ($/GPU/hr)
}

// bmStandardSKU maps a bare-metal Standard shape to the per-OCPU/per-GB OCI part
// numbers of its family; the whole-instance price is rate × the shape's pinned
// OCPU and memory (from shapeTable). These matter only when a BM.* shape is
// offered as capacity_type=on_demand (hourly-billed); a bare_metal lane reports 0.
var bmStandardSKU = map[string]struct{ ocpu, mem string }{
	"BM.Standard.E5.192": {"B97384", "B97385"}, // E5 OCPU/Memory
	"BM.Standard3.64":    {"B94176", "B94177"}, // X9 (Intel) OCPU/Memory
	"BM.Standard.A1.160": {"B93297", "B93298"}, // A1 (Ampere) OCPU/Memory (free-tier SKU = 0)
}

// ociPriceList is the production priceSource: it reads the public OCI price list
// over HTTP (no credentials) and maps the offered shapes onto their SKUs.
type ociPriceList struct {
	url    string
	hc     *http.Client
	logger *slog.Logger
}

func newOCIPriceList(url string, logger *slog.Logger) *ociPriceList {
	if url == "" {
		url = defaultOCIPriceListURL
	}
	return &ociPriceList{
		url:    url,
		hc:     &http.Client{Timeout: 30 * time.Second},
		logger: logger,
	}
}

// priceProduct is one OCI price-list catalogue entry (only the fields we read).
type priceProduct struct {
	PartNumber                string `json:"partNumber"`
	CurrencyCodeLocalizations []struct {
		CurrencyCode string `json:"currencyCode"`
		Prices       []struct {
			Model string  `json:"model"`
			Value float64 `json:"value"`
		} `json:"prices"`
	} `json:"currencyCodeLocalizations"`
}

// payAsYouGoUSD returns the product's USD PAY_AS_YOU_GO (on-demand) hourly rate.
func (p priceProduct) payAsYouGoUSD() (float64, bool) {
	for _, c := range p.CurrencyCodeLocalizations {
		if c.CurrencyCode != "USD" {
			continue
		}
		for _, pr := range c.Prices {
			if pr.Model == "PAY_AS_YOU_GO" {
				return pr.Value, true
			}
		}
	}
	return 0, false
}

func (s *ociPriceList) Fetch(ctx context.Context) (livePrices, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return livePrices{}, fmt.Errorf("oci price list: build request: %w", err)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return livePrices{}, fmt.Errorf("oci price list: fetch %s: %w", s.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Drain a little of the body for context, then bail.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return livePrices{}, fmt.Errorf("oci price list: %s returned %d: %s", s.url, resp.StatusCode, body)
	}
	var doc struct {
		Items []priceProduct `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return livePrices{}, fmt.Errorf("oci price list: decode: %w", err)
	}
	byPart := make(map[string]float64, len(doc.Items))
	for _, it := range doc.Items {
		if v, ok := it.payAsYouGoUSD(); ok {
			byPart[it.PartNumber] = v
		}
	}
	return buildLivePrices(byPart), nil
}

// buildLivePrices maps a part-number->USD price index onto the offered shapes'
// flexible rates and whole-instance fixed rates. A shape whose SKU is absent from
// the index is omitted (the refresher then keeps its seed value).
func buildLivePrices(byPart map[string]float64) livePrices {
	out := livePrices{
		flexRates:   make(map[string]rate),
		fixedHourly: make(map[string]float64),
	}
	for family, sku := range flexSKU {
		oc, okO := byPart[sku.ocpu]
		mem, okM := byPart[sku.mem]
		if !okO && !okM {
			continue
		}
		out.flexRates[family] = rate{OCPU: oc, MemoryGB: mem}
	}
	for shape, sku := range gpuSKU {
		per, ok := byPart[sku]
		if !ok {
			continue
		}
		spec, known := shapeTable[shape]
		if !known || spec.GPUCount <= 0 {
			continue
		}
		out.fixedHourly[shape] = per * float64(spec.GPUCount)
	}
	for shape, sku := range bmStandardSKU {
		oc, okO := byPart[sku.ocpu]
		mem, okM := byPart[sku.mem]
		if !okO && !okM {
			continue
		}
		spec, known := shapeTable[shape]
		if !known {
			continue
		}
		out.fixedHourly[shape] = oc*spec.OCPU + mem*spec.MemGiB
	}
	return out
}

// fakePriceSource is a deterministic, network-free priceSource for unit tests and
// the credential-free conformance/certification run. It reports plausible OCI
// on-demand rates so the refresher path is exercised offline.
type fakePriceSource struct {
	prices livePrices
	err    error
}

func newFakePriceSource() *fakePriceSource {
	return &fakePriceSource{prices: livePrices{
		flexRates: map[string]rate{
			"VM.Standard.E5.Flex": {OCPU: 0.030, MemoryGB: 0.002},
			"VM.Standard.E4.Flex": {OCPU: 0.025, MemoryGB: 0.0015},
			"VM.Standard3.Flex":   {OCPU: 0.040, MemoryGB: 0.0015},
			"VM.Optimized3.Flex":  {OCPU: 0.054, MemoryGB: 0.0015},
			"VM.Standard.A2.Flex": {OCPU: 0.014, MemoryGB: 0.002},
			// A1 omitted on purpose: its live SKU prices at 0, so the seed stands.
		},
		fixedHourly: map[string]float64{
			"VM.GPU.A10.1":       2.0,
			"VM.GPU.A10.2":       4.0,
			"BM.Standard.E5.192": 0.030*192 + 0.002*2304,
			"BM.Standard3.64":    0.040*64 + 0.0015*1024,
			"BM.GPU.A100-v2.8":   32.0,
		},
	}}
}

func (f *fakePriceSource) Fetch(_ context.Context) (livePrices, error) {
	if f.err != nil {
		return livePrices{}, f.err
	}
	return f.prices, nil
}

var (
	_ priceSource = (*ociPriceList)(nil)
	_ priceSource = (*fakePriceSource)(nil)
)
