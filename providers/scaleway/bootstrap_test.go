package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The per-machine token is deterministic (restart-safe) and distinct per machine.
func TestAgentToken_DeterministicAndPerMachine(t *testing.T) {
	secret := []byte("test-secret")
	a1 := agentToken(secret, "machine-a")
	a2 := agentToken(secret, "machine-a")
	bID := agentToken(secret, "machine-b")
	if a1 != a2 {
		t.Errorf("token not deterministic: %q vs %q", a1, a2)
	}
	if a1 == bID {
		t.Errorf("tokens for distinct machines collide: %q", a1)
	}
	if agentToken([]byte("other"), "machine-a") == a1 {
		t.Errorf("token did not change with the secret")
	}
}

// authenticate accepts only the correct per-machine bearer token.
func TestBootstrapVault_Authenticate(t *testing.T) {
	v := newBootstrapVault([]byte("secret"), quietLogger())
	tok := v.Token("m-a")
	if !v.authenticate("m-a", "Bearer "+tok) {
		t.Error("correct token must authenticate")
	}
	if v.authenticate("m-b", "Bearer "+tok) {
		t.Error("m-a's token must not authenticate m-b")
	}
	if v.authenticate("m-a", tok) {
		t.Error("a token without the Bearer prefix must not authenticate")
	}
	if v.authenticate("m-a", "Bearer garbage") {
		t.Error("garbage token must not authenticate")
	}
}

// Enqueue blocks until the agent fetches the command and posts a successful ack,
// and rejects an unauthenticated fetch. This is the real delivery path the fake
// backend (and so the conformance suite) cannot exercise.
func TestBootstrapVault_DeliverAndAck(t *testing.T) {
	v := newBootstrapVault([]byte("secret"), quietLogger())
	srv := httptest.NewServer(v)
	defer srv.Close()

	const machineID = "m-1"
	token := v.Token(machineID)

	// Unauthenticated fetch is rejected.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/command?machine_id="+machineID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unauth fetch: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth fetch status = %d, want 401", resp.StatusCode)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done <- v.Enqueue(ctx, machineID, bootstrapCommand{Type: "configure", ClusterID: "c1", Blob: "blob"})
	}()

	// Poll for the command (authenticated), then ack success.
	var cmd bootstrapCommand
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/command?machine_id="+machineID, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		cr, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatalf("auth fetch: %v", err)
		}
		if cr.StatusCode == http.StatusOK {
			_ = json.NewDecoder(cr.Body).Decode(&cmd)
			_ = cr.Body.Close()
			break
		}
		_ = cr.Body.Close()
		time.Sleep(10 * time.Millisecond)
	}
	if cmd.Type != "configure" || cmd.ClusterID != "c1" {
		t.Fatalf("fetched command = %+v, want configure/c1", cmd)
	}

	ackBody, _ := json.Marshal(bootstrapAck{CommandID: cmd.CommandID, OK: true})
	ar, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/ack?machine_id="+machineID, bytes.NewReader(ackBody))
	ar.Header.Set("Authorization", "Bearer "+token)
	aresp, err := http.DefaultClient.Do(ar)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	_ = aresp.Body.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Enqueue returned error after successful ack: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue did not return after ack")
	}
}

// A failure ack surfaces as an error from Enqueue (the kit then drives FAILED).
func TestBootstrapVault_FailureAck(t *testing.T) {
	v := newBootstrapVault([]byte("secret"), quietLogger())
	const machineID = "m-2"
	token := v.Token(machineID)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done <- v.Enqueue(ctx, machineID, bootstrapCommand{Type: "configure"})
	}()

	// Fetch the command to learn its command_id (the agent must echo it), then ack
	// failure with that id.
	var cmd bootstrapCommand
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gr := httptest.NewRequest(http.MethodGet, "/v1/command?machine_id="+machineID, nil)
		gr.Header.Set("Authorization", "Bearer "+token)
		gw := httptest.NewRecorder()
		v.ServeHTTP(gw, gr)
		if gw.Code == http.StatusOK {
			_ = json.NewDecoder(gw.Body).Decode(&cmd)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cmd.CommandID == "" {
		t.Fatal("did not fetch a command with a command_id")
	}
	ackBody, _ := json.Marshal(bootstrapAck{CommandID: cmd.CommandID, OK: false, Error: "join failed"})
	r := httptest.NewRequest(http.MethodPost, "/v1/ack?machine_id="+machineID, bytes.NewReader(ackBody))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	v.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("ack status = %d, want 200", w.Code)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Enqueue returned nil after a failure ack, want an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue did not return after failure ack")
	}
}

// An ack carrying a stale command_id (one that has since been superseded) must
// NOT complete the current waiter — otherwise a Configure/Drain could report
// success for a command the agent never executed.
func TestBootstrapVault_StaleAckIgnored(t *testing.T) {
	v := newBootstrapVault([]byte("secret"), quietLogger())
	const machineID = "m-stale"
	token := v.Token(machineID)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		done <- v.Enqueue(ctx, machineID, bootstrapCommand{Type: "configure"})
	}()
	time.Sleep(20 * time.Millisecond)

	// Ack a DIFFERENT (stale) command id.
	ackBody, _ := json.Marshal(bootstrapAck{CommandID: "stale-deadbeef", OK: true})
	r := httptest.NewRequest(http.MethodPost, "/v1/ack?machine_id="+machineID, bytes.NewReader(ackBody))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	v.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale ack status = %d, want 409", w.Code)
	}

	// The waiter must NOT have completed on the stale ack — it should still be
	// blocked, and only return (with an error) when ctx expires.
	select {
	case err := <-done:
		t.Fatalf("Enqueue completed on a stale ack (err=%v); it must ignore mismatched command_id", err)
	case <-time.After(100 * time.Millisecond):
		// still blocked — correct
	}
	if err := <-done; err == nil {
		t.Fatal("Enqueue should return an error once ctx expires with no matching ack")
	}
}

// When the agent never acks, Enqueue returns once ctx is cancelled (the kit's
// transition timeout) — so a stuck bootstrap drives the machine to FAILED rather
// than hanging or falsely reporting CONFIGURED.
func TestBootstrapVault_ContextCancel(t *testing.T) {
	v := newBootstrapVault([]byte("secret"), quietLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := v.Enqueue(ctx, "m-3", bootstrapCommand{Type: "configure"})
	if err == nil {
		t.Fatal("Enqueue returned nil when ctx expired before any ack, want an error")
	}
}
