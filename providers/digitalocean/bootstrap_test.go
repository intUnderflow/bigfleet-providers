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

// The bootstrap secret is required for the real backend: a flag or env value is
// accepted, but an unset secret hard-fails rather than falling back to a random
// per-process value (which would invalidate issued agent tokens on restart).
func TestResolveBootstrapSecret(t *testing.T) {
	t.Setenv("BIGFLEET_BOOTSTRAP_SECRET", "")
	if _, err := resolveBootstrapSecret(""); err == nil {
		t.Error("unset bootstrap secret returned nil error, want a hard failure")
	}
	if got, err := resolveBootstrapSecret("from-flag"); err != nil || string(got) != "from-flag" {
		t.Errorf("flag secret: got (%q, %v), want (from-flag, nil)", got, err)
	}
	t.Setenv("BIGFLEET_BOOTSTRAP_SECRET", "from-env")
	if got, err := resolveBootstrapSecret(""); err != nil || string(got) != "from-env" {
		t.Errorf("env secret: got (%q, %v), want (from-env, nil)", got, err)
	}
}

// Enqueue blocks until the agent fetches the command and posts a successful ack,
// and rejects an unauthenticated fetch.
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
	if cmd.ID == "" {
		t.Fatal("fetched command has no id to echo in the ack")
	}

	// A stale ack (wrong command id) must NOT release the waiter.
	staleBody, _ := json.Marshal(bootstrapAck{CommandID: "does-not-match", OK: true})
	sr, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/ack?machine_id="+machineID, bytes.NewReader(staleBody))
	sr.Header.Set("Authorization", "Bearer "+token)
	sresp, err := http.DefaultClient.Do(sr)
	if err != nil {
		t.Fatalf("stale ack: %v", err)
	}
	_ = sresp.Body.Close()
	select {
	case err := <-done:
		t.Fatalf("Enqueue returned on a stale ack (err=%v); it must wait for the matching command id", err)
	case <-time.After(50 * time.Millisecond):
	}

	ackBody, _ := json.Marshal(bootstrapAck{CommandID: cmd.ID, OK: true})
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

	// Give Enqueue a moment to register the pending command, then fetch it to
	// learn its id (as the agent would) and ack failure echoing that id.
	time.Sleep(20 * time.Millisecond)
	gr := httptest.NewRequest(http.MethodGet, "/v1/command?machine_id="+machineID, nil)
	gr.Header.Set("Authorization", "Bearer "+token)
	gw := httptest.NewRecorder()
	v.ServeHTTP(gw, gr)
	if gw.Code != http.StatusOK {
		t.Fatalf("fetch command status = %d, want 200", gw.Code)
	}
	var cmd bootstrapCommand
	if err := json.NewDecoder(gw.Body).Decode(&cmd); err != nil {
		t.Fatalf("decode command: %v", err)
	}

	ackBody, _ := json.Marshal(bootstrapAck{CommandID: cmd.ID, OK: false, Error: "join failed"})
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
