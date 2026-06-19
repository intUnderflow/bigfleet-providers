// Command genpricing regenerates a region's pinned on-demand price table for the
// AWS provider from the public AWS Price List Bulk API — no AWS credentials
// required.
//
// Usage:
//
//	go run ./cmd/genpricing -region us-east-1 \
//	    -types m6i.large,m6i.xlarge,c7g.large,r6i.large
//
// It fetches the region's EC2 offer file, extracts the Linux / Shared-tenancy /
// no-pre-installed-software on-demand price for each requested instance type,
// and prints a Go map literal you paste into onDemandByRegion in pricing.go.
//
// The offer files are large (tens of MB), so this is an offline maintenance tool
// run occasionally — never a runtime dependency of the provider. It uses only
// the standard library, so it adds nothing to the module's dependencies.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const offerURLFmt = "https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEC2/current/%s/index.json"

func main() {
	region := flag.String("region", "us-east-1", "AWS region code, e.g. us-east-1")
	typesCSV := flag.String("types", "", "comma-separated instance types (required)")
	flag.Parse()

	want := parseTypes(*typesCSV)
	if len(want) == 0 {
		fmt.Fprintln(os.Stderr, "genpricing: -types is required (comma-separated instance types)")
		os.Exit(2)
	}

	url := fmt.Sprintf(offerURLFmt, *region)
	body, err := fetch(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "genpricing: fetch %s: %v\n", url, err)
		os.Exit(1)
	}
	defer func() { _ = body.Close() }()

	prices, err := extractOnDemandPrices(body, want)
	if err != nil {
		fmt.Fprintf(os.Stderr, "genpricing: parse offer file: %v\n", err)
		os.Exit(1)
	}
	if missing := missingTypes(want, prices); len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "genpricing: warning: no on-demand price found for: %s\n", strings.Join(missing, ", "))
	}
	if err := printGoTable(os.Stdout, *region, prices); err != nil {
		fmt.Fprintf(os.Stderr, "genpricing: write output: %v\n", err)
		os.Exit(1)
	}
}

func parseTypes(csv string) map[string]bool {
	want := map[string]bool{}
	for _, t := range strings.Split(csv, ",") {
		if t = strings.TrimSpace(t); t != "" {
			want[t] = true
		}
	}
	return want
}

func fetch(url string) (io.ReadCloser, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	return resp.Body, nil
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

// extractOnDemandPrices parses an EC2 offer file and returns USD/hr on-demand
// prices for the wanted instance types. It selects the plain on-demand SKU —
// Linux, Shared tenancy, no pre-installed software, used capacity, RunInstances
// — so reserved/Windows/dedicated/host SKUs for the same type are ignored.
func extractOnDemandPrices(r io.Reader, want map[string]bool) (map[string]float64, error) {
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

func missingTypes(want map[string]bool, got map[string]float64) []string {
	var missing []string
	for t := range want {
		if _, ok := got[t]; !ok {
			missing = append(missing, t)
		}
	}
	sort.Strings(missing)
	return missing
}

// printGoTable writes a Go map-literal entry ready to paste into
// onDemandByRegion in pricing.go.
func printGoTable(w io.Writer, region string, prices map[string]float64) error {
	types := make([]string, 0, len(prices))
	for t := range prices {
		types = append(types, t)
	}
	sort.Strings(types)
	out := fmt.Sprintf("\t%q: {\n", region)
	for _, t := range types {
		out += fmt.Sprintf("\t\t%q: %s,\n", t, strconv.FormatFloat(prices[t], 'g', -1, 64))
	}
	out += "\t},\n"
	_, err := io.WriteString(w, out)
	return err
}
