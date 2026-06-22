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
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/intUnderflow/bigfleet-providers/providers/aws/internal/pricelist"
)

func main() {
	region := flag.String("region", "us-east-1", "AWS region code, e.g. us-east-1")
	typesCSV := flag.String("types", "", "comma-separated instance types (required)")
	flag.Parse()

	want := parseTypes(*typesCSV)
	if len(want) == 0 {
		fmt.Fprintln(os.Stderr, "genpricing: -types is required (comma-separated instance types)")
		os.Exit(2)
	}

	url := pricelist.OfferURL(pricelist.DefaultBaseURL, *region)
	body, err := fetch(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "genpricing: fetch %s: %v\n", url, err)
		os.Exit(1)
	}
	defer func() { _ = body.Close() }()

	prices, err := pricelist.ExtractOnDemandPrices(body, want)
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
