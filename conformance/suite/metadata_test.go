//go:build certify

package suite

import (
	"fmt"
	"maps"
	"testing"
	"time"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

// C5 — shard_metadata store/echo/clear under stress (behaviors B50x). Deeper
// than the upstream echo/clear/unknown-key checks: large maps, unicode, binary,
// empty values, read stability across repeated Get/List, and clean replacement
// after a Drain.

func stressMetadata() map[string]string {
	// Values are proto3 strings, so they must be valid UTF-8 — but that still
	// includes embedded NUL and the full control range (the upstream suite
	// relies on NUL too). Invalid-UTF-8 bytes can't ride the wire at all, so
	// they aren't a conformance case.
	md := map[string]string{
		"bigfleet.lucy.sh/assigned-priority": "1000000",
		"bigfleet.lucy.sh/assigned-group":    "topology.bigfleet/rack\x00gang-7", // embedded NUL
		"x-unicode/value":                    "παντα ρει — 你好 — 🚀\t\n",
		"x-empty/value":                      "",
		"x-control/value":                    "\x00\x01\x02\x1f\x7f", // valid-UTF-8 control bytes
	}
	// A large map: providers must not cap or summarize.
	for i := 0; i < 50; i++ {
		md[fmt.Sprintf("x-bulk/key-%02d", i)] = fmt.Sprintf("value-%d-with-some-length-padding", i)
	}
	return md
}

func TestMetadata_StressEchoedVerbatim(t *testing.T) {
	h := dial(t)
	md := stressMetadata()
	id := h.WalkToConfigured("conf-md-stress", md)

	// Echoed verbatim on Get, and stable across repeated reads.
	for i := 0; i < 3; i++ {
		got := h.Get(id)
		if got.GetCluster() != "conf-md-stress" {
			t.Errorf("read %d: cluster %q, want conf-md-stress", i, got.GetCluster())
		}
		if !maps.Equal(got.GetShardMetadata(), md) {
			t.Errorf("read %d: shard_metadata not verbatim (got %d keys, want %d)", i, len(got.GetShardMetadata()), len(md))
		}
	}

	// And verbatim on List(CONFIGURED).
	found := false
	for _, m := range h.List(pb.MachineState_MACHINE_STATE_CONFIGURED) {
		if m.GetId() != id {
			continue
		}
		found = true
		if !maps.Equal(m.GetShardMetadata(), md) {
			t.Errorf("List shard_metadata not verbatim for %s", id)
		}
	}
	if !found {
		t.Errorf("List(CONFIGURED) did not return %s", id)
	}
}

// shard_metadata clears with the binding on Drain, and a subsequent Configure
// installs a fresh map with no residue from the previous binding.
func TestMetadata_ClearedAndReplacedCleanly(t *testing.T) {
	h := dial(t)
	id := h.WalkToConfigured("conf-md-1", map[string]string{"a": "1", "old": "x"})

	if _, err := h.Drain(id, 5); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	m := h.MustReach(id, pb.MachineState_MACHINE_STATE_IDLE, 15*time.Second)
	if len(m.GetShardMetadata()) != 0 || m.GetCluster() != "" {
		t.Fatalf("metadata/cluster survived Drain: md=%v cluster=%q", m.GetShardMetadata(), m.GetCluster())
	}

	// Re-Configure with a DIFFERENT map: no key from the old binding leaks.
	fresh := map[string]string{"b": "2"}
	if _, err := h.Configure(id, "conf-md-2", fresh); err != nil {
		t.Fatalf("re-Configure: %v", err)
	}
	h.MustReach(id, pb.MachineState_MACHINE_STATE_CONFIGURED, 15*time.Second)
	got := h.Get(id)
	if !maps.Equal(got.GetShardMetadata(), fresh) {
		t.Errorf("re-Configure metadata = %v, want exactly %v (no residue)", got.GetShardMetadata(), fresh)
	}
	if got.GetCluster() != "conf-md-2" {
		t.Errorf("re-Configure cluster = %q, want conf-md-2", got.GetCluster())
	}
}
