package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

// evictionReporter ingests Spot eviction notices and raises the affected
// machine's observed interruption probability toward 1.0, so the engine's victim
// scoring sees a real, rising probability for a machine about to be reclaimed.
//
// Azure surfaces an impending Spot eviction via Scheduled Events on the *per-VM*
// IMDS endpoint (http://169.254.169.254/metadata/scheduledevents, event type
// Preempt) — there is no central queue the provider control plane can read
// (unlike AWS's EventBridge→SQS). So a small node-side agent (installed via
// --base-user-data; see deploy/agent/scheduled-events-agent.sh) observes the
// Preempt event and POSTs it here. This endpoint is the provider's analogue of
// the AWS interruption poller: it marks the machine and lets the background
// reconcile loop propagate the raised value into kit inventory.
type evictionReporter struct {
	backend *azureBackend
	srv     *providerkit.Server // to propagate the raised value promptly
	m       *metrics
	token   string // shared bearer secret; empty = unauthenticated (in-cluster only)
	logger  *slog.Logger
}

// evictionReport is the JSON body the node-side agent POSTs. It identifies the
// machine either directly (machine_id, read from the bigfleet-machine-id IMDS
// tag) or by its Azure resource id (resolved via the bigfleet-machine-id tag).
type evictionReport struct {
	MachineID  string `json:"machine_id"`
	ResourceID string `json:"resource_id"`
	EventType  string `json:"event_type"` // Preempt (Spot eviction); others ack-and-ignore
}

// preemptProbability is the observed interruption probability published once a
// Preempt notice is seen — high, but < 1.0, matching the AWS 2-minute-notice
// convention (the eviction is imminent but not yet certain to this hour).
const preemptProbability = 0.99

func newEvictionReporter(backend *azureBackend, srv *providerkit.Server, m *metrics, token string, logger *slog.Logger) *evictionReporter {
	return &evictionReporter{backend: backend, srv: srv, m: m, token: token, logger: logger}
}

// handle is the POST /internal/eviction handler. It authenticates (if a token is
// configured), resolves the report to a machine id, and on a Preempt event
// raises that machine's observed interruption probability and counts the notice.
func (e *evictionReporter) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if e.token != "" {
		// Constant-time compare so the endpoint doesn't leak the token byte-by-byte
		// via response timing.
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+e.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	var rep evictionReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&rep); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	machineID := rep.MachineID
	if machineID == "" && rep.ResourceID != "" {
		machineID = e.backend.machineIDFor(r.Context(), rep.ResourceID)
	}
	if machineID == "" {
		// Unknown / unmanaged VM — ack so the agent stops retrying.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Only Preempt (Spot eviction) raises the probability; other Scheduled Events
	// types (Reboot/Redeploy/Freeze/Terminate) are acknowledged and ignored.
	if strings.EqualFold(rep.EventType, "Preempt") {
		e.backend.interruption.markWarning(machineID, preemptProbability)
		if e.m != nil {
			e.m.interrupts.Inc()
		}
		e.logger.Info("observed spot eviction notice", "machine", machineID, "event", rep.EventType, "probability", preemptProbability)
		e.propagate()
	}
	w.WriteHeader(http.StatusNoContent)
}

// propagate kicks a bounded background reconcile so the raised probability lands
// in kit inventory promptly, rather than waiting for the next --reconcile-interval
// tick. Best-effort and non-blocking.
func (e *evictionReporter) propagate() {
	if e.srv == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := e.srv.Reconcile(ctx); err != nil {
			e.logger.Warn("eviction propagate reconcile failed", "err", err)
		}
	}()
}
