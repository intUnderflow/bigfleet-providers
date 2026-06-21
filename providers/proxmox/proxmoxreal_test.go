package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luthermonson/go-proxmox"
)

// newFakeAPI stands up an httptest server speaking just enough of the Proxmox
// /api2/json surface for the real-client teardown tests, and returns a
// proxmoxReal wired to it.
func newFakeAPI(t *testing.T, handler http.HandlerFunc) *proxmoxReal {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := proxmox.NewClient(srv.URL)
	r, err := newProxmoxReal(proxmoxRealConfig{Client: client}, quietLogger())
	if err != nil {
		t.Fatalf("newProxmoxReal: %v", err)
	}
	return r
}

// DeleteVM must be idempotent against an already-gone VM. A gone VM makes
// Proxmox answer the VM lookup with HTTP 500 (its "...does not exist" detail is
// in a body the pinned SDK discards before reading), so error-string matching
// can't recognise it — DeleteVM decides idempotency from cluster presence. With
// the VM absent from /cluster/resources, DeleteVM must return nil. This
// exercises the REAL go-proxmox client path (the in-memory fake can't reproduce
// the SDK's 500-shaped error).
func TestProxmoxReal_DeleteVM_IdempotentWhenGone(t *testing.T) {
	r := newFakeAPI(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/cluster/status":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case "/cluster/resources":
			_, _ = w.Write([]byte(`{"data":[]}`)) // VM is gone: no resources
		default:
			// Mirror Proxmox: a gone VM's status/current lookup is HTTP 500.
			http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
		}
	})
	if err := r.DeleteVM(context.Background(), "pve-1", 12345); err != nil {
		t.Errorf("DeleteVM of an already-gone VM = %v, want nil (idempotent)", err)
	}
}

// Conversely, DeleteVM must NOT swallow a real error: a VM that is still present
// in the cluster but whose lookup fails surfaces the error (so the kit can FAIL
// it), rather than being mistaken for already-gone.
func TestProxmoxReal_DeleteVM_ErrorsWhenStillPresent(t *testing.T) {
	r := newFakeAPI(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/cluster/status":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case "/cluster/resources":
			// VM is present (e.g. transiently unreachable, not gone).
			_, _ = w.Write([]byte(`{"data":[{"type":"qemu","vmid":12345,"node":"pve-1","name":"bigfleet-x","status":"running"}]}`))
		default:
			http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
		}
	})
	if err := r.DeleteVM(context.Background(), "pve-1", 12345); err == nil {
		t.Error("DeleteVM of a present-but-unreachable VM = nil, want an error (must not be mistaken for already-gone)")
	}
}
