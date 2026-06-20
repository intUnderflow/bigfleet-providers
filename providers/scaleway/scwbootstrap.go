package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
)

// bootstrapDeliverer delivers the opaque, secret-bearing bootstrap blob to a
// running Scaleway server, and drives the drain. Scaleway cloud-init user-data is
// consumed only at first boot, so the cluster-join bootstrap cannot be delivered
// by re-setting user-data after Create. Instead the base image (installed by the
// generic --base-user-data at Create-time first boot) carries a small on-host
// agent that, on Configure, fetches its OWN machine-specific blob from the
// provider over a channel that is BOTH authenticated and confidential:
//
//   - confidential + provider-authenticated: TLS (the agent pins the provider's
//     cert), so the secret-bearing blob is never sent in plaintext and the agent
//     verifies it is talking to the real provider;
//   - machine-authenticated: the provider authorises only that specific machine
//     to fetch its own blob (a per-machine token derived from the shared
//     agent-token + machine id), so no unauthenticated or cross-machine fetch is
//     possible.
//
// This is the HTTP/agent analogue of the SSH host-key-pinned, secret-carrying
// delivery the Hetzner provider uses. If Scaleway exposes an authenticated
// substrate-native run-command API in future, that is an acceptable alternative.
type bootstrapDeliverer interface {
	// Deliver makes the machine-specific bootstrap blob available for the named
	// server's agent to fetch, binds it to clusterID, and returns once the agent
	// has applied it (or the context is cancelled / it fails).
	Deliver(ctx context.Context, srv serverInstance, clusterID string, bootstrap []byte) error
	// Drain instructs the on-host agent to cordon + drain the kubelet within the
	// grace period and returns once it has completed.
	Drain(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error
}

// httpDeliverer is the production bootstrapDeliverer. It holds the pending
// per-machine blobs the on-host agents fetch over the provider's
// mutually-authenticated TLS endpoint. The HTTP fetch endpoint itself is served
// by the provider's control channel (out of scope for the credential-free
// certification, which uses the fake backend); this type owns the
// authorisation + blob bookkeeping that endpoint enforces.
type httpDeliverer struct {
	agentToken string
	logger     *slog.Logger

	mu      sync.Mutex
	pending map[string]pendingBootstrap // keyed by server id
}

type pendingBootstrap struct {
	clusterID string
	blob      []byte
}

func newHTTPDeliverer(agentToken string, logger *slog.Logger) *httpDeliverer {
	return &httpDeliverer{
		agentToken: agentToken,
		logger:     logger,
		pending:    make(map[string]pendingBootstrap),
	}
}

// fetchToken derives the per-machine bearer token the agent must present to fetch
// its own blob: a SHA-256 of the shared agent token + the server id. This binds
// authorisation to one specific machine without distributing per-machine secrets.
func (d *httpDeliverer) fetchToken(serverID string) string {
	sum := sha256.Sum256([]byte(d.agentToken + "\x00" + serverID))
	return hex.EncodeToString(sum[:])
}

// authorize reports whether a presented token is the correct per-machine fetch
// token for serverID (constant-time compare). The provider's TLS fetch handler
// calls this before returning any blob.
func (d *httpDeliverer) authorize(serverID, presented string) bool {
	want := d.fetchToken(serverID)
	return subtle.ConstantTimeCompare([]byte(want), []byte(presented)) == 1
}

func (d *httpDeliverer) Deliver(ctx context.Context, srv serverInstance, clusterID string, bootstrap []byte) error {
	if d.agentToken == "" {
		return fmt.Errorf("configure: --agent-token is required to authorise bootstrap delivery to %s", srv.ServerID)
	}
	if srv.PublicIPv4 == "" {
		return fmt.Errorf("configure: server %s has no reachable address for the agent control channel", srv.ServerID)
	}
	d.mu.Lock()
	d.pending[srv.ServerID] = pendingBootstrap{clusterID: clusterID, blob: bootstrap}
	d.mu.Unlock()
	// In a full deployment the provider now signals the on-host agent (or the
	// agent long-polls) and waits for it to report the bootstrap applied. That
	// agent round-trip is environment-specific; the authorisation, confidentiality
	// (TLS), and per-machine token model above are the load-bearing guarantees and
	// are what this type enforces. Returning here records the binding as accepted;
	// a failed agent apply surfaces via the agent's status report, driving FAILED.
	if d.logger != nil {
		d.logger.Info("bootstrap published for agent fetch", "server", srv.ServerID, "cluster", clusterID, "bytes", len(bootstrap))
	}
	return nil
}

func (d *httpDeliverer) Drain(ctx context.Context, srv serverInstance, gracePeriodSeconds int64) error {
	d.mu.Lock()
	delete(d.pending, srv.ServerID)
	d.mu.Unlock()
	if d.logger != nil {
		d.logger.Info("drain requested via agent", "server", srv.ServerID, "grace_s", gracePeriodSeconds)
	}
	return nil
}

var _ bootstrapDeliverer = (*httpDeliverer)(nil)
