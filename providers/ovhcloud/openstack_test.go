package main

import (
	"strings"
	"testing"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

func TestFirstIPv4_PrefersFloating(t *testing.T) {
	addresses := map[string]any{
		"Ext-Net": []any{
			map[string]any{"addr": "10.0.0.5", "version": float64(4), "OS-EXT-IPS:type": "fixed"},
			map[string]any{"addr": "203.0.113.7", "version": float64(4), "OS-EXT-IPS:type": "floating"},
			map[string]any{"addr": "2001:db8::1", "version": float64(6), "OS-EXT-IPS:type": "fixed"},
		},
	}
	if got := firstIPv4(addresses); got != "203.0.113.7" {
		t.Errorf("firstIPv4 = %q, want the floating 203.0.113.7", got)
	}
}

func TestFirstIPv4_FallsBackToFixed(t *testing.T) {
	addresses := map[string]any{
		"private": []any{
			map[string]any{"addr": "10.0.0.9", "version": float64(4), "OS-EXT-IPS:type": "fixed"},
		},
	}
	if got := firstIPv4(addresses); got != "10.0.0.9" {
		t.Errorf("firstIPv4 = %q, want the fixed 10.0.0.9", got)
	}
	if got := firstIPv4(nil); got != "" {
		t.Errorf("firstIPv4(nil) = %q, want empty", got)
	}
}

func TestFlavorName_PrefersOriginalName(t *testing.T) {
	if got := flavorName(map[string]any{"original_name": "b2-7", "id": "abc"}); got != "b2-7" {
		t.Errorf("flavorName = %q, want b2-7", got)
	}
	if got := flavorName(nil); got != "" {
		t.Errorf("flavorName(nil) = %q, want empty", got)
	}
}

func TestServerName_StableOnToken(t *testing.T) {
	a := serverName(serverSpec{MachineID: "m1", IdempotencyToken: "op-7"})
	b := serverName(serverSpec{MachineID: "m1", IdempotencyToken: "op-7"})
	if a != b {
		t.Errorf("serverName not stable for same token: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "bigfleet-") {
		t.Errorf("serverName %q missing bigfleet- prefix", a)
	}
	if len(a) > 63 {
		t.Errorf("serverName %q exceeds 63 chars", a)
	}
}

func TestShellQuote_EscapesSingleQuote(t *testing.T) {
	got := shellQuote("a'b")
	if got != `'a'\''b'` {
		t.Errorf("shellQuote = %q, want %q", got, `'a'\''b'`)
	}
}

// Bare metal prices at 0 (already paid for); on-demand uses the pinned table.
func TestPricing_BareMetalZero(t *testing.T) {
	p := newPricing(1.10)
	if v := p.price("b2-7", providerkit.CapacityBareMetal); v != 0 {
		t.Errorf("bare-metal price = %v, want 0", v)
	}
	if v := p.price("b2-7", providerkit.CapacityOnDemand); v <= 0 {
		t.Errorf("on-demand b2-7 price = %v, want > 0", v)
	}
	p.setOverride("zz-custom", 9.99)
	if v := p.price("zz-custom", providerkit.CapacityOnDemand); v != 9.99 {
		t.Errorf("override price = %v, want 9.99", v)
	}
}
