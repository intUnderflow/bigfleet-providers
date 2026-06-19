package main

import (
	"reflect"
	"strings"
	"testing"
)

// A trimmed EC2 offer file: the on-demand m6i.large + c7g.large SKUs, a Windows
// m6i.large SKU (must be ignored), and an r6i.large product with no OnDemand
// term (must be omitted).
const offerFixture = `{
  "products": {
    "SKU_M6I_LINUX": {"attributes": {
      "instanceType": "m6i.large", "operatingSystem": "Linux", "tenancy": "Shared",
      "preInstalledSw": "NA", "capacitystatus": "Used", "operation": "RunInstances"}},
    "SKU_M6I_WIN": {"attributes": {
      "instanceType": "m6i.large", "operatingSystem": "Windows", "tenancy": "Shared",
      "preInstalledSw": "NA", "capacitystatus": "Used", "operation": "RunInstances:0002"}},
    "SKU_M6I_DEDICATED": {"attributes": {
      "instanceType": "m6i.large", "operatingSystem": "Linux", "tenancy": "Dedicated",
      "preInstalledSw": "NA", "capacitystatus": "Used", "operation": "RunInstances"}},
    "SKU_C7G_LINUX": {"attributes": {
      "instanceType": "c7g.large", "operatingSystem": "Linux", "tenancy": "Shared",
      "preInstalledSw": "NA", "capacitystatus": "Used", "operation": "RunInstances"}},
    "SKU_R6I_LINUX": {"attributes": {
      "instanceType": "r6i.large", "operatingSystem": "Linux", "tenancy": "Shared",
      "preInstalledSw": "NA", "capacitystatus": "Used", "operation": "RunInstances"}}
  },
  "terms": {
    "OnDemand": {
      "SKU_M6I_LINUX": {"SKU_M6I_LINUX.JRTC": {"priceDimensions": {
        "SKU_M6I_LINUX.JRTC.6YS6": {"pricePerUnit": {"USD": "0.0960000000"}}}}},
      "SKU_M6I_WIN": {"SKU_M6I_WIN.JRTC": {"priceDimensions": {
        "SKU_M6I_WIN.JRTC.6YS6": {"pricePerUnit": {"USD": "0.1880000000"}}}}},
      "SKU_C7G_LINUX": {"SKU_C7G_LINUX.JRTC": {"priceDimensions": {
        "SKU_C7G_LINUX.JRTC.6YS6": {"pricePerUnit": {"USD": "0.0725000000"}}}}}
    }
  }
}`

func TestExtractOnDemandPrices(t *testing.T) {
	want := map[string]bool{"m6i.large": true, "c7g.large": true, "r6i.large": true, "absent.type": true}
	got, err := extractOnDemandPrices(strings.NewReader(offerFixture), want)
	if err != nil {
		t.Fatalf("extractOnDemandPrices: %v", err)
	}
	// m6i.large -> the Linux/Shared SKU (not Windows, not Dedicated).
	// c7g.large -> its Linux SKU. r6i.large -> product exists but no OnDemand
	// term, so omitted. absent.type -> never in the file.
	expect := map[string]float64{"m6i.large": 0.096, "c7g.large": 0.0725}
	if !reflect.DeepEqual(got, expect) {
		t.Fatalf("prices = %v, want %v", got, expect)
	}
}

func TestExtractOnDemandPrices_BadJSON(t *testing.T) {
	if _, err := extractOnDemandPrices(strings.NewReader("{not json"), map[string]bool{"m6i.large": true}); err == nil {
		t.Fatal("expected a decode error for malformed JSON")
	}
}

func TestPrintGoTable(t *testing.T) {
	var b strings.Builder
	printGoTable(&b, "us-east-1", map[string]float64{"m6i.xlarge": 0.192, "m6i.large": 0.096})
	want := "\t\"us-east-1\": {\n\t\t\"m6i.large\": 0.096,\n\t\t\"m6i.xlarge\": 0.192,\n\t},\n"
	if b.String() != want {
		t.Fatalf("printGoTable =\n%q\nwant\n%q", b.String(), want)
	}
}

func TestParseTypes(t *testing.T) {
	got := parseTypes(" m6i.large , c7g.large ,, m6i.large ")
	want := map[string]bool{"m6i.large": true, "c7g.large": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseTypes = %v, want %v", got, want)
	}
}
