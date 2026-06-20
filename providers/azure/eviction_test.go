package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// A Preempt notice for a known machine raises its observed interruption
// probability above the forecast and increments the eviction counter.
func TestEviction_PreemptRaisesProbability(t *testing.T) {
	b, _ := newTestBackend(t, 8)
	m := newMetrics()
	r := newEvictionReporter(b, nil, m, "", quietLogger())

	// Pick a SPOT slot so the forecast is a real, sub-1.0 value to exceed.
	var spotID, spotSize string
	for _, slot := range b.speculativeSlots() {
		if slot.CapacityType == providerkit.CapacitySpot {
			spotID, spotSize = slot.ID, slot.InstanceType
			break
		}
	}
	if spotID == "" {
		t.Fatal("no spot slot to test")
	}
	forecast := b.interruption.probability(spotID, spotSize, providerkit.CapacitySpot)

	req := httptest.NewRequest(http.MethodPost, "/internal/eviction",
		strings.NewReader(`{"machine_id":"`+spotID+`","event_type":"Preempt"}`))
	w := httptest.NewRecorder()
	r.handle(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := b.interruption.probability(spotID, spotSize, providerkit.CapacitySpot); got <= forecast {
		t.Errorf("observed probability %v did not exceed forecast %v after Preempt", got, forecast)
	}
	if got := testutil.ToFloat64(m.interrupts); got != 1 {
		t.Errorf("eviction counter = %v, want 1", got)
	}
}

// A non-Preempt event (e.g. Reboot) is acknowledged but does not raise the
// probability or count an eviction.
func TestEviction_NonPreemptIgnored(t *testing.T) {
	b, _ := newTestBackend(t, 8)
	m := newMetrics()
	r := newEvictionReporter(b, nil, m, "", quietLogger())

	req := httptest.NewRequest(http.MethodPost, "/internal/eviction",
		strings.NewReader(`{"machine_id":"whatever","event_type":"Reboot"}`))
	w := httptest.NewRecorder()
	r.handle(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := testutil.ToFloat64(m.interrupts); got != 0 {
		t.Errorf("eviction counter = %v, want 0 for a non-Preempt event", got)
	}
}

// With a token configured, a request without the matching bearer is rejected
// before any state change.
func TestEviction_TokenAuth(t *testing.T) {
	b, _ := newTestBackend(t, 8)
	m := newMetrics()
	r := newEvictionReporter(b, nil, m, "s3cret", quietLogger())

	body := `{"machine_id":"m","event_type":"Preempt"}`
	// No token -> 401.
	w := httptest.NewRecorder()
	r.handle(w, httptest.NewRequest(http.MethodPost, "/internal/eviction", strings.NewReader(body)))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d, want 401", w.Code)
	}
	// Correct token -> accepted (204).
	req := httptest.NewRequest(http.MethodPost, "/internal/eviction", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer s3cret")
	w = httptest.NewRecorder()
	r.handle(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("valid token: status = %d, want 204", w.Code)
	}
}

// Only POST is accepted.
func TestEviction_MethodNotAllowed(t *testing.T) {
	b, _ := newTestBackend(t, 4)
	r := newEvictionReporter(b, nil, newMetrics(), "", quietLogger())
	w := httptest.NewRecorder()
	r.handle(w, httptest.NewRequest(http.MethodGet, "/internal/eviction", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", w.Code)
	}
}
