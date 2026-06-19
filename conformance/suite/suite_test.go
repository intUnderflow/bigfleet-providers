//go:build certify

// Package suite is the BigFleet provider conformance EXTENSION suite. It runs
// against a live provider's gRPC endpoint (--target host:port) and certifies
// the contract far beyond the upstream baseline. Build-tagged `certify` so it
// only builds/runs when explicitly requested.
//
//	go test -tags=certify -target=localhost:9000 ./suite/...
//
// Every test maps to a behavior in ../docs/conformance.md. The upstream
// authoritative suite (bigfleet test/conformance) is the immovable baseline and
// is run separately by the certify runner; this suite EXTENDS it, it never
// forks or re-implements it.
package suite

import (
	"flag"
	"os"
	"testing"

	"github.com/intUnderflow/bigfleet-providers/conformance/internal/harness"
)

var targetFlag = flag.String("target", "", "host:port of the provider under certification (or BIGFLEET_PROVIDER_TARGET)")

func target(t *testing.T) string {
	t.Helper()
	if *targetFlag != "" {
		return *targetFlag
	}
	if v := os.Getenv("BIGFLEET_PROVIDER_TARGET"); v != "" {
		return v
	}
	t.Skip("certify: set -target or BIGFLEET_PROVIDER_TARGET to run the conformance extension suite")
	return ""
}

func dial(t *testing.T) *harness.H { return harness.Dial(t, target(t)) }
