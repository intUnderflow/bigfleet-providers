package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

// The bootstrap control channel is how the provider delivers the
// cluster-specific, secret-bearing bootstrap blob to an ALREADY-RUNNING Scaleway
// server (§4.5: an on-host agent over a mutually-authenticated TLS channel). It
// is NOT user_data: a server's user_data is consumed by cloud-init only at first
// boot, so it cannot carry a post-create secret.
//
// Shape of the channel:
//   - Provider side: the bootstrapVault, an HTTPS endpoint the provider serves
//     (its own server certificate; the agent pins the CA). It holds, per
//     machine, a single pending command (configure-with-blob, or drain) and the
//     waiter blocked in Enqueue.
//   - Server side: a small agent, installed by the GENERIC pre-binding user_data
//     baked in at Create. It long-polls GET /v1/command with its per-machine
//     bearer token, applies the returned command, and POSTs the result to
//     /v1/ack.
//
// Authentication is mutual: the agent verifies it is talking to the real provider
// (pinned server cert / CA), and the provider authorises only that specific
// server to fetch its own blob via a per-machine bearer token. The token is
// HMAC(secret, machineID), so it is restart-safe (re-derivable, never stored) and
// per-machine (no cross-machine read). Never an unauthenticated or plaintext
// fetch. This is the HTTP/agent analogue of the hetzner provider's SSH
// host-key-pinned delivery.

// agentToken derives the per-machine bearer token from the vault secret and the
// machine id: base64(HMAC-SHA256(secret, machineID)). Deterministic and
// restart-safe — the provider re-derives it to authenticate the agent, and never
// has to persist it.
func agentToken(secret []byte, machineID string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(machineID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// bootstrapCommand is the JSON the agent fetches from GET /v1/command.
type bootstrapCommand struct {
	// CommandID is a per-enqueue nonce. The agent MUST echo it in its ack so a
	// stale ack (for a command that has since been superseded) cannot complete the
	// current waiter — see handleAck.
	CommandID    string `json:"command_id"`
	Type         string `json:"type"`                    // "configure" | "drain"
	ClusterID    string `json:"cluster_id,omitempty"`    // configure: the cluster to join
	Blob         string `json:"blob,omitempty"`          // configure: base64 of the opaque bootstrap blob
	GraceSeconds int64  `json:"grace_seconds,omitempty"` // drain: graceful-shutdown budget
}

// bootstrapAck is the JSON the agent posts to /v1/ack once it has applied a
// command. CommandID echoes the command the agent actually executed.
type bootstrapAck struct {
	CommandID string `json:"command_id"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

// newCommandID mints a random per-enqueue command nonce. It surfaces an entropy
// failure rather than returning an all-zero id, which would defeat the stale-ack
// protection (a colliding id could let an ack complete the wrong command).
func newCommandID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate command id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// pendingCommand couples a queued command with the channel its Enqueue caller is
// blocked on.
type pendingCommand struct {
	cmd  bootstrapCommand
	done chan error
}

// bootstrapVault is the provider-served control plane for one provider process.
// It is safe for concurrent use.
type bootstrapVault struct {
	secret []byte
	logger *slog.Logger

	mu      sync.Mutex
	pending map[string]*pendingCommand // machineID -> queued command + waiter
}

func newBootstrapVault(secret []byte, logger *slog.Logger) *bootstrapVault {
	return &bootstrapVault{
		secret:  secret,
		logger:  logger,
		pending: make(map[string]*pendingCommand),
	}
}

// Token returns the per-machine bearer token the agent must present. The real
// client injects it into the server's generic user_data at Create.
func (v *bootstrapVault) Token(machineID string) string {
	return agentToken(v.secret, machineID)
}

// Enqueue queues a command for the named machine's agent and blocks until the
// agent acknowledges it (or ctx is cancelled — e.g. the kit's Configure/Drain
// transition timeout). The kit drives Configure/Drain serially per machine, so a
// single pending slot per machine is sufficient; a fresh Enqueue supersedes any
// stale one.
func (v *bootstrapVault) Enqueue(ctx context.Context, machineID string, cmd bootstrapCommand) error {
	if machineID == "" {
		return fmt.Errorf("bootstrap: empty machine id")
	}
	id, err := newCommandID()
	if err != nil {
		return err
	}
	cmd.CommandID = id
	pc := &pendingCommand{cmd: cmd, done: make(chan error, 1)}
	v.mu.Lock()
	if old := v.pending[machineID]; old != nil {
		// Unblock a superseded waiter so it does not hang forever.
		select {
		case old.done <- fmt.Errorf("superseded by a newer command"):
		default:
		}
	}
	v.pending[machineID] = pc
	v.mu.Unlock()

	defer func() {
		v.mu.Lock()
		if v.pending[machineID] == pc {
			delete(v.pending, machineID)
		}
		v.mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return fmt.Errorf("agent did not apply %s for %s: %w", cmd.Type, machineID, ctx.Err())
	case err := <-pc.done:
		return err
	}
}

// authenticate checks the Authorization bearer token against the per-machine
// HMAC token. Constant-time, so it leaks no timing signal.
func (v *bootstrapVault) authenticate(machineID, authHeader string) bool {
	const prefix = "Bearer "
	if machineID == "" || !strings.HasPrefix(authHeader, prefix) {
		return false
	}
	got := strings.TrimPrefix(authHeader, prefix)
	want := agentToken(v.secret, machineID)
	return hmac.Equal([]byte(got), []byte(want))
}

// ServeHTTP routes the two agent endpoints. Mount it on the provider's HTTPS
// bootstrap listener.
func (v *bootstrapVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/command":
		v.handleCommand(w, r)
	case "/v1/ack":
		v.handleAck(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (v *bootstrapVault) handleCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	machineID := r.URL.Query().Get("machine_id")
	if !v.authenticate(machineID, r.Header.Get("Authorization")) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	v.mu.Lock()
	pc := v.pending[machineID]
	v.mu.Unlock()
	if pc == nil {
		w.WriteHeader(http.StatusNoContent) // nothing pending; agent polls again
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pc.cmd)
}

func (v *bootstrapVault) handleAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	machineID := r.URL.Query().Get("machine_id")
	if !v.authenticate(machineID, r.Header.Get("Authorization")) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var ack bootstrapAck
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&ack); err != nil {
		http.Error(w, "bad ack body", http.StatusBadRequest)
		return
	}
	v.mu.Lock()
	pc := v.pending[machineID]
	v.mu.Unlock()
	if pc == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Ignore an ack for a command that has since been superseded: the current
	// waiter is a DIFFERENT command, so completing it on this stale ack would
	// report success/failure for work the agent never did. The agent will
	// re-poll and pick up the current command.
	if ack.CommandID != pc.cmd.CommandID {
		w.WriteHeader(http.StatusConflict)
		return
	}
	var result error
	if !ack.OK {
		msg := ack.Error
		if msg == "" {
			msg = "agent reported failure"
		}
		result = fmt.Errorf("agent bootstrap failed for %s: %s", machineID, msg)
	}
	select {
	case pc.done <- result:
	default:
	}
	w.WriteHeader(http.StatusOK)
}

// agentCloudConfig renders the cloud-init #cloud-config that the GENERIC
// pre-binding user_data uses to self-configure the on-host agent at Create time:
// it writes the agent's config (the provider's bootstrap endpoint, the pinned CA,
// the per-machine token, and the machine id) so the agent can fetch its
// cluster-specific blob later over the verified TLS channel. The image must ship
// the agent itself; this only hands it the per-machine credentials.
func agentCloudConfig(endpoint, caPEM, machineID, token string) string {
	cfg := map[string]string{
		"endpoint":   endpoint,
		"ca_pem":     caPEM,
		"machine_id": machineID,
		"token":      token,
	}
	body, _ := json.MarshalIndent(cfg, "", "  ")
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("write_files:\n")
	b.WriteString("  - path: /etc/bigfleet-agent/config.json\n")
	b.WriteString("    permissions: '0600'\n")
	b.WriteString("    owner: root:root\n")
	b.WriteString("    content: |\n")
	for _, line := range strings.Split(string(body), "\n") {
		b.WriteString("      ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// drainGrace clamps a grace period to something sane for the agent command.
func drainGrace(seconds int64) int64 {
	if seconds <= 0 {
		return 1
	}
	return seconds
}
