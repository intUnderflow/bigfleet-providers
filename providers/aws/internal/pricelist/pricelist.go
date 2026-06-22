// Package pricelist parses the public AWS Price List Bulk API EC2 offer files
// (the per-region offer JSON, no credentials required) and extracts plain
// on-demand hourly prices. It is shared by two callers in the AWS provider:
//
//   - cmd/genpricing, the offline tool that regenerates the pinned fallback
//     table, and
//   - the runtime on-demand price refresher (ec2Real), which fetches the same
//     offer file on a timer to keep live on-demand prices off the List hot path.
//
// The offer files are large (tens of MB), so the decoder keeps only the few
// fields we need and ignores the rest.
package pricelist

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
)

// DefaultBaseURL is the public AWS Price List Bulk API host. It serves the EC2
// region offer files with no credentials. Tests override the base URL to point
// at a local httptest server.
const DefaultBaseURL = "https://pricing.us-east-1.amazonaws.com"

// OfferURL builds the URL of a region's EC2 offer file under baseURL.
func OfferURL(baseURL, region string) string {
	return fmt.Sprintf("%s/offers/v1.0/aws/AmazonEC2/current/%s/index.json", baseURL, region)
}

// offerFile is the subset of an AWS EC2 offer file we parse. Unknown fields are
// ignored, so the decoder keeps only what we need regardless of the file size.
type offerFile struct {
	Products map[string]offerProduct `json:"products"`
	Terms    struct {
		OnDemand map[string]map[string]offerTerm `json:"OnDemand"`
	} `json:"terms"`
}

type offerProduct struct {
	Attributes struct {
		InstanceType    string `json:"instanceType"`
		OperatingSystem string `json:"operatingSystem"`
		Tenancy         string `json:"tenancy"`
		PreInstalledSw  string `json:"preInstalledSw"`
		CapacityStatus  string `json:"capacitystatus"`
		Operation       string `json:"operation"`
	} `json:"attributes"`
}

type offerTerm struct {
	PriceDimensions map[string]struct {
		PricePerUnit struct {
			USD string `json:"USD"`
		} `json:"pricePerUnit"`
	} `json:"priceDimensions"`
}

// ExtractOnDemandPrices parses an EC2 offer file and returns USD/hr on-demand
// prices for the wanted instance types. It selects the plain on-demand SKU —
// Linux, Shared tenancy, no pre-installed software, used capacity, RunInstances
// — so reserved/Windows/dedicated/host SKUs for the same type are ignored.
func ExtractOnDemandPrices(r io.Reader, want map[string]bool) (map[string]float64, error) {
	var offer offerFile
	if err := json.NewDecoder(r).Decode(&offer); err != nil {
		return nil, err
	}
	skuByType := make(map[string]string, len(want))
	for sku, p := range offer.Products {
		a := p.Attributes
		if !want[a.InstanceType] {
			continue
		}
		if a.OperatingSystem == "Linux" && a.Tenancy == "Shared" &&
			a.PreInstalledSw == "NA" && a.CapacityStatus == "Used" &&
			a.Operation == "RunInstances" {
			skuByType[a.InstanceType] = sku
		}
	}
	out := make(map[string]float64, len(skuByType))
	for typ, sku := range skuByType {
		terms, ok := offer.Terms.OnDemand[sku]
		if !ok {
			continue
		}
		if price, ok := onDemandHourlyPrice(terms); ok {
			out[typ] = price
		}
	}
	return out, nil
}

// onDemandHourlyPrice returns the USD price from an on-demand term set. An
// on-demand SKU has a single term with a single price dimension; we pick the
// lexicographically-smallest key for determinism if there is ever more than one.
func onDemandHourlyPrice(terms map[string]offerTerm) (float64, bool) {
	type candidate struct {
		key   string
		price float64
	}
	var cands []candidate
	for termKey, term := range terms {
		for dimKey, dim := range term.PriceDimensions {
			usd := dim.PricePerUnit.USD
			if usd == "" {
				continue
			}
			v, err := strconv.ParseFloat(usd, 64)
			if err != nil {
				continue
			}
			cands = append(cands, candidate{termKey + "/" + dimKey, v})
		}
	}
	if len(cands) == 0 {
		return 0, false
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].key < cands[j].key })
	return cands[0].price, true
}
