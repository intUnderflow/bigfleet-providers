//go:build certify

package suite

import (
	"testing"

	"github.com/intUnderflow/bigfleet-providers/conformance/internal/registry"
)

// behavior binds the current test to a frozen registry leaf-id, fails if the id
// is unknown (drift guard), and logs a marker the runner parses for coverage.
func behavior(t *testing.T, id string) registry.Behavior {
	t.Helper()
	b, ok := registry.ByID(id)
	if !ok {
		t.Fatalf("behavior %q is not in the conformance registry", id)
	}
	t.Logf("BEHAVIOR %s", id)
	return b
}
