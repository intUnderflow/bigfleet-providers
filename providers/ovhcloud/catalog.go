package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// priceSource fetches current hourly on-demand prices for flavors from a live,
// credential-free OVH price source. It is consulted ONLY off the List/Describe
// hot path — by pricing.refresh, on a timer — never per request.
//
// The only implementation that ships is catalogPriceSource (the public order
// catalog). Unit tests substitute a deterministic fake (no network), and the
// fake backend / credential-free conformance run uses no source at all (prices
// come from the dated seed table), so neither path makes a live call.
type priceSource interface {
	// HourlyEUR returns hourly on-demand prices in EUR for the requested
	// flavors, keyed by flavor. Flavors absent from the catalog are simply
	// omitted (the caller keeps the seed/fallback price for those). It returns
	// an error if the catalog cannot be fetched/parsed or is not priced in EUR
	// (the whole refresh then fails closed onto the seed, rather than converting
	// a wrong-currency number with --eur-usd).
	HourlyEUR(ctx context.Context, flavors []string) (map[string]float64, error)
}

// catalogPriceSource reads hourly Public Cloud instance prices from OVHcloud's
// public order catalog:
//
//	GET https://api.ovh.com/1.0/order/catalog/public/cloud?ovhSubsidiary=<sub>
//
// The endpoint needs NO credentials. Each flavor's pay-as-you-go hourly rate is
// the addon whose planCode is "<flavor>.consumption", under the pricing entry
// with intervalUnit=="hour" and a "consumption" capacity. Prices are integers in
// catalog units (1 currency unit == 1e8), ex-VAT — matching the seed table's
// ex-VAT EUR basis.
//
// The catalog is region-agnostic for instance flavors (OVH prices a flavor
// identically across its regions in a given subsidiary), so one fetch per
// subsidiary covers every region this provider serves.
type catalogPriceSource struct {
	endpoint   string // catalog URL without query (default ovhCatalogEndpoint)
	subsidiary string // ovhSubsidiary query value, e.g. "FR" (must be a EUR subsidiary)
	httpClient *http.Client
	logger     *slog.Logger
	// observe records the fetch as an API call for /metrics (op="Catalog"); nil
	// in tests. Mirrors metrics.observeAPI's signature.
	observe func(op string, start time.Time, err error)
}

const (
	// ovhCatalogEndpoint is the public, unauthenticated order-catalog URL.
	ovhCatalogEndpoint = "https://api.ovh.com/1.0/order/catalog/public/cloud"
	// catalogPriceScale converts a catalog integer price to currency units: the
	// catalog reports 1 EUR as 1e8.
	catalogPriceScale = 100_000_000.0
)

func newCatalogPriceSource(endpoint, subsidiary string, logger *slog.Logger, observe func(string, time.Time, error)) *catalogPriceSource {
	if endpoint == "" {
		endpoint = ovhCatalogEndpoint
	}
	if subsidiary == "" {
		subsidiary = "FR"
	}
	return &catalogPriceSource{
		endpoint:   endpoint,
		subsidiary: subsidiary,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		logger:     logger,
		observe:    observe,
	}
}

// catalogResponse is the slice of the (large) catalog payload we parse. Only the
// addon plan codes, their hourly consumption pricing, and the locale currency are
// read; everything else is ignored.
type catalogResponse struct {
	Locale struct {
		CurrencyCode string `json:"currencyCode"`
	} `json:"locale"`
	Addons []struct {
		PlanCode string `json:"planCode"`
		Pricings []struct {
			IntervalUnit string   `json:"intervalUnit"`
			Capacities   []string `json:"capacities"`
			Price        int64    `json:"price"`
		} `json:"pricings"`
	} `json:"addons"`
}

func (c *catalogPriceSource) HourlyEUR(ctx context.Context, flavors []string) (map[string]float64, error) {
	start := time.Now()
	cat, err := c.fetch(ctx)
	if c.observe != nil {
		c.observe("Catalog", start, err)
	}
	if err != nil {
		return nil, err
	}
	if cat.Locale.CurrencyCode != "EUR" {
		// --eur-usd assumes EUR. Refuse to convert a GBP/PLN/USD number with it;
		// fail closed onto the seed table instead of publishing a wrong price.
		return nil, fmt.Errorf("ovh catalog subsidiary %q is priced in %q, not EUR; pricing assumes a EUR subsidiary (set --price-subsidiary to a EUR one, or price these flavors with --flavor-price)", c.subsidiary, cat.Locale.CurrencyCode)
	}

	// Index the hourly consumption price by flavor, for every addon priced that
	// way — then pick out the flavors we were asked about.
	byFlavor := make(map[string]float64, len(cat.Addons))
	for _, a := range cat.Addons {
		flavor, ok := consumptionFlavor(a.PlanCode)
		if !ok {
			continue
		}
		for _, p := range a.Pricings {
			if p.IntervalUnit != "hour" || !slices.Contains(p.Capacities, "consumption") {
				continue
			}
			// Skip a non-positive price: overlaying 0 would publish
			// price_per_hour=0 (always wins cost ranking). Leave the flavor absent
			// so the seed/override covers it (fail closed), and let the caller's
			// missing-flavor warning fire.
			if p.Price <= 0 {
				break
			}
			byFlavor[flavor] = float64(p.Price) / catalogPriceScale
			break
		}
	}

	out := make(map[string]float64, len(flavors))
	for _, f := range flavors {
		if v, ok := byFlavor[f]; ok {
			out[f] = v
		}
	}
	return out, nil
}

func (c *catalogPriceSource) fetch(ctx context.Context) (*catalogResponse, error) {
	u, err := url.Parse(c.endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse catalog endpoint %q: %w", c.endpoint, err)
	}
	q := u.Query()
	q.Set("ovhSubsidiary", c.subsidiary)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build catalog request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch catalog: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Bound the error body so a misconfigured endpoint can't blow up logs.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("catalog returned HTTP %d: %s", resp.StatusCode, string(body))
	}
	var cat catalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	return &cat, nil
}

// consumptionFlavor returns the flavor name for an addon plan code of the form
// "<flavor>.consumption" (the pay-as-you-go hourly product), and false for any
// other plan code (monthly commitments, OS-bundled variants like "win-<flavor>",
// localised "<flavor>.consumption.LZ.*" zone variants, managed-database addons,
// etc.) so only the base hourly instance rate is picked up.
func consumptionFlavor(planCode string) (string, bool) {
	flavor, ok := strings.CutSuffix(planCode, ".consumption")
	if !ok || flavor == "" {
		return "", false
	}
	// A bare instance flavor never contains a dot. This excludes localised/option
	// variants ("b2-7.option.dc-adp.consumption") and database/service addons that
	// embed a flavor token, keeping only the base hourly instance product.
	if strings.Contains(flavor, ".") {
		return "", false
	}
	return flavor, true
}

var _ priceSource = (*catalogPriceSource)(nil)
