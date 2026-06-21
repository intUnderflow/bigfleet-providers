package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// fakeUserData is an in-memory userDataAPI for exercising the real client's
// UserData create/recover/delete lifecycle without a live Latitude API (the
// latitudeClient fake is a pure lifecycle simulator and models no UserData).
type fakeUserData struct {
	mu        sync.Mutex
	seq       int
	items     map[string]userDataItem // id -> item
	createErr error
}

func newFakeUserData() *fakeUserData {
	return &fakeUserData{items: map[string]userDataItem{}}
}

func (f *fakeUserData) create(_ context.Context, _, description, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.seq++
	id := fmt.Sprintf("ud_%d", f.seq)
	f.items[id] = userDataItem{ID: id, Description: description}
	return id, nil
}

func (f *fakeUserData) list(_ context.Context, _ string) ([]userDataItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]userDataItem, 0, len(f.items))
	for _, it := range f.items {
		out = append(out, it)
	}
	return out, nil
}

func (f *fakeUserData) delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, id)
	return nil
}

func (f *fakeUserData) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items)
}

// realWithFakeUserData builds a latitudeReal wired to an in-memory userDataAPI
// and substrate index — enough to drive the UserData helpers (which never touch
// the SDK). The sdk field stays nil; tests must only call ud/index helpers.
func realWithFakeUserData(t *testing.T, statePath string) (*latitudeReal, *fakeUserData) {
	t.Helper()
	idx, err := newSubstrateIndex(statePath)
	if err != nil {
		t.Fatalf("newSubstrateIndex: %v", err)
	}
	ud := newFakeUserData()
	return &latitudeReal{ud: ud, project: "proj", index: idx, logger: quietLogger()}, ud
}

// createUserData then deleteUserData must leave no orphan resource.
func TestUserData_CreateDelete_NoLeak(t *testing.T) {
	r, ud := realWithFakeUserData(t, "")
	ctx := context.Background()

	id, err := r.createUserData(ctx, "latitude-ash/on_demand/c2-small-x86/ASH/000", "#cloud-config\n")
	if err != nil {
		t.Fatalf("createUserData: %v", err)
	}
	if ud.count() != 1 {
		t.Fatalf("want 1 UserData after create, got %d", ud.count())
	}
	r.deleteUserData(ctx, id)
	if ud.count() != 0 {
		t.Errorf("UserData leaked after delete: %d remain", ud.count())
	}
}

// The round-2 regression: after an index loss the UserData id must be recoverable
// from the REAL machine id (not a hostname-decoded partial), so teardown does not
// leak the orphan resource (which holds a cleartext host private key). A
// different machine id must NOT match.
func TestUserData_RecoverByMachineID(t *testing.T) {
	r, ud := realWithFakeUserData(t, "")
	ctx := context.Background()
	machineID := "latitude-ash/on_demand/c2-small-x86/ASH/017"

	id, err := r.createUserData(ctx, machineID, "#cloud-config\n")
	if err != nil {
		t.Fatalf("createUserData: %v", err)
	}

	// Simulate a restart with a lost index: recover purely by the machine id.
	got := r.findUserDataID(ctx, userDataDescription(machineID))
	if got != id {
		t.Fatalf("findUserDataID by machine id = %q, want %q", got, id)
	}
	// A different machine id must not match (no cross-machine teardown).
	if other := r.findUserDataID(ctx, userDataDescription("latitude-ash/on_demand/c2-small-x86/ASH/000")); other != "" {
		t.Errorf("findUserDataID matched a different machine id: %q", other)
	}

	r.deleteUserData(ctx, got)
	if ud.count() != 0 {
		t.Errorf("UserData leaked after recovery-delete: %d remain", ud.count())
	}
}

// On the create-error path the just-created UserData must be torn down, never
// leaked (it carries a cleartext host private key).
func TestUserData_CreateErrorCleanup(t *testing.T) {
	r, ud := realWithFakeUserData(t, "")
	ctx := context.Background()
	id, err := r.createUserData(ctx, "m1", "#cloud-config\n")
	if err != nil {
		t.Fatalf("createUserData: %v", err)
	}
	// Model the CreateServer error branch: the deploy failed, so clean up.
	r.deleteUserData(ctx, id)
	if ud.count() != 0 {
		t.Errorf("UserData leaked on create-error path: %d remain", ud.count())
	}
	// A create error surfaces, leaving nothing behind.
	ud.createErr = errors.New("boom")
	if _, err := r.createUserData(ctx, "m2", "x"); err == nil {
		t.Error("expected createUserData to surface the API error")
	}
	if ud.count() != 0 {
		t.Errorf("failed create left a resource: %d", ud.count())
	}
}
