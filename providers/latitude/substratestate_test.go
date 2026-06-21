package main

import (
	"path/filepath"
	"testing"
)

// The index is the authoritative machine_id<->server map and must survive a
// restart (a fresh process re-reading the same file), so adoption, host-key
// verification, and UserData teardown all keep working.
func TestSubstrateIndex_PersistReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "substrate.json")

	ix, err := newSubstrateIndex(path)
	if err != nil {
		t.Fatalf("newSubstrateIndex: %v", err)
	}
	if err := ix.upsert(machineState{MachineID: "m/000", ServerID: "sv_1", HostKeyFP: "fp1", UserDataID: "ud_1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := ix.setCluster("sv_1", "cluster-a"); err != nil {
		t.Fatalf("setCluster: %v", err)
	}

	// Reload from disk (simulating a restart).
	re, err := newSubstrateIndex(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	st, ok := re.machineByID("m/000")
	if !ok {
		t.Fatal("machine not recovered after reload")
	}
	if st.ServerID != "sv_1" || st.HostKeyFP != "fp1" || st.UserDataID != "ud_1" || st.ClusterID != "cluster-a" {
		t.Errorf("recovered state mismatch: %+v", st)
	}
	// Reverse lookup must survive too.
	if byServer, ok := re.machineByServer("sv_1"); !ok || byServer.MachineID != "m/000" {
		t.Errorf("reverse index not recovered: %+v ok=%v", byServer, ok)
	}

	// Remove persists; a second reload no longer sees it.
	if err := re.removeByServer("sv_1"); err != nil {
		t.Fatalf("removeByServer: %v", err)
	}
	re2, _ := newSubstrateIndex(path)
	if _, ok := re2.machineByID("m/000"); ok {
		t.Error("machine still present after removeByServer + reload")
	}
}

// Re-pointing a machine at a new server must drop the stale reverse-index entry,
// so a serverID is never mapped to a machine it no longer backs.
func TestSubstrateIndex_ReverseIndexConsistency(t *testing.T) {
	ix, _ := newSubstrateIndex("")
	_ = ix.upsert(machineState{MachineID: "m1", ServerID: "sv_a"})
	_ = ix.upsert(machineState{MachineID: "m1", ServerID: "sv_b"})

	if _, ok := ix.machineByServer("sv_a"); ok {
		t.Error("stale reverse-index entry sv_a still present after re-point")
	}
	st, ok := ix.machineByServer("sv_b")
	if !ok || st.MachineID != "m1" {
		t.Errorf("new reverse-index entry sv_b missing: %+v ok=%v", st, ok)
	}
}

// An in-memory index (empty path) is a no-op on persistence but fully functional.
func TestSubstrateIndex_InMemory(t *testing.T) {
	ix, err := newSubstrateIndex("")
	if err != nil {
		t.Fatalf("newSubstrateIndex(\"\"): %v", err)
	}
	if err := ix.upsert(machineState{MachineID: "m1", ServerID: "sv_1", UserDataID: "ud_1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	st, ok := ix.machineByServer("sv_1")
	if !ok || st.UserDataID != "ud_1" {
		t.Errorf("in-memory lookup failed: %+v ok=%v", st, ok)
	}
}
