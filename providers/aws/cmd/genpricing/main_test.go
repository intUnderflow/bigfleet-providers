package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestPrintGoTable(t *testing.T) {
	var b strings.Builder
	if err := printGoTable(&b, "us-east-1", map[string]float64{"m6i.xlarge": 0.192, "m6i.large": 0.096}); err != nil {
		t.Fatalf("printGoTable: %v", err)
	}
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
