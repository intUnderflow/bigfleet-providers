package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	latitudeshgosdk "github.com/latitudesh/latitudesh-go-sdk"
	"github.com/latitudesh/latitudesh-go-sdk/models/operations"
)

// substrateIndex is the small piece of substrate state the provider legitimately
// owns and must persist (the kit owns lifecycle/binding/metadata in its own
// FileStore): the authoritative machine_id -> {serverID, host-key fingerprint,
// per-server UserData id, cluster binding} mapping, so a retried Create can heal
// onto the existing server, Configure/Drain can verify the pinned host key, and
// Delete can tear the per-server UserData down without leaking it.
//
// machine_id is the identity key — NOT the hostname. The hostname is a
// collision-free hash of machine_id used only as a human-readable, deterministic
// deploy name (see deployHostname); it is never decoded back. This is what makes
// the scheme injective: two distinct machine ids never share an identity, even
// when their ids are long.
type substrateIndex struct {
	mu        sync.Mutex
	path      string                   // persisted JSON; "" = in-memory only
	byMachine map[string]*machineState // machine_id -> state (authoritative)
	byServer  map[string]string        // serverID -> machine_id (derived)
}

// machineState is the per-machine substrate state persisted in the index.
type machineState struct {
	MachineID  string `json:"machine_id"`
	ServerID   string `json:"server_id"`
	HostKeyFP  string `json:"host_key_fp,omitempty"`
	UserDataID string `json:"user_data_id,omitempty"`
	ClusterID  string `json:"cluster_id,omitempty"`
}

// newSubstrateIndex loads the index from path (if set and present). An empty path
// keeps it in-memory only (dev / the credential-free fake path never uses it).
func newSubstrateIndex(path string) (*substrateIndex, error) {
	ix := &substrateIndex{
		path:      path,
		byMachine: map[string]*machineState{},
		byServer:  map[string]string{},
	}
	if path == "" {
		return ix, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ix, nil
		}
		return nil, fmt.Errorf("read substrate state %s: %w", path, err)
	}
	var states []*machineState
	if err := json.Unmarshal(data, &states); err != nil {
		return nil, fmt.Errorf("parse substrate state %s: %w", path, err)
	}
	for _, st := range states {
		if st == nil || st.MachineID == "" {
			continue
		}
		ix.byMachine[st.MachineID] = st
		if st.ServerID != "" {
			ix.byServer[st.ServerID] = st.MachineID
		}
	}
	return ix, nil
}

// machineByID returns a copy of the state for a machine id.
func (ix *substrateIndex) machineByID(machineID string) (machineState, bool) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	st, ok := ix.byMachine[machineID]
	if !ok {
		return machineState{}, false
	}
	return *st, true
}

// machineByServer returns a copy of the state for a server id.
func (ix *substrateIndex) machineByServer(serverID string) (machineState, bool) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	id, ok := ix.byServer[serverID]
	if !ok {
		return machineState{}, false
	}
	return *ix.byMachine[id], true
}

// upsert stores/updates a machine's state and persists the index.
func (ix *substrateIndex) upsert(st machineState) error {
	if st.MachineID == "" {
		return fmt.Errorf("substrate index: empty machine id")
	}
	ix.mu.Lock()
	prev := ix.byMachine[st.MachineID]
	if prev != nil && prev.ServerID != "" && prev.ServerID != st.ServerID {
		delete(ix.byServer, prev.ServerID)
	}
	cp := st
	ix.byMachine[st.MachineID] = &cp
	if st.ServerID != "" {
		ix.byServer[st.ServerID] = st.MachineID
	}
	ix.mu.Unlock()
	return ix.save()
}

// setCluster records/clears the cluster binding for a server, persisting it.
func (ix *substrateIndex) setCluster(serverID, clusterID string) error {
	ix.mu.Lock()
	id, ok := ix.byServer[serverID]
	if ok {
		ix.byMachine[id].ClusterID = clusterID
	}
	ix.mu.Unlock()
	if !ok {
		return nil
	}
	return ix.save()
}

// removeByServer drops a server's entry and persists the index.
func (ix *substrateIndex) removeByServer(serverID string) error {
	ix.mu.Lock()
	id, ok := ix.byServer[serverID]
	if ok {
		delete(ix.byServer, serverID)
		delete(ix.byMachine, id)
	}
	ix.mu.Unlock()
	if !ok {
		return nil
	}
	return ix.save()
}

// save atomically writes the index to disk (no-op when in-memory only).
func (ix *substrateIndex) save() error {
	if ix.path == "" {
		return nil
	}
	ix.mu.Lock()
	states := make([]*machineState, 0, len(ix.byMachine))
	for _, st := range ix.byMachine {
		states = append(states, st)
	}
	ix.mu.Unlock()
	data, err := json.MarshalIndent(states, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal substrate state: %w", err)
	}
	dir := filepath.Dir(ix.path)
	tmp, err := os.CreateTemp(dir, ".substrate-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp substrate state: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp substrate state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp substrate state: %w", err)
	}
	if err := os.Rename(tmpName, ix.path); err != nil {
		return fmt.Errorf("rename substrate state: %w", err)
	}
	return nil
}

// userDataAPI is the slice of the Latitude UserData API the real client needs,
// abstracted so the create/recover/delete lifecycle can be unit-tested against
// an in-memory fake (the latitudeClient fake is a pure lifecycle simulator and
// does not model UserData).
type userDataAPI interface {
	create(ctx context.Context, project, description, contentB64 string) (string, error)
	list(ctx context.Context, project string) ([]userDataItem, error)
	delete(ctx context.Context, id string) error
}

// userDataItem is a substrate-neutral view of one UserData resource.
type userDataItem struct {
	ID          string
	Description string
}

// sdkUserData is the production userDataAPI backed by latitudesh-go-sdk.
type sdkUserData struct {
	sdk *latitudeshgosdk.Latitudesh
}

func (s *sdkUserData) create(ctx context.Context, project, description, contentB64 string) (string, error) {
	resp, err := s.sdk.UserData.CreateNew(ctx, operations.PostUserDataUserDataRequestBody{
		Data: operations.PostUserDataUserDataData{
			Type: operations.PostUserDataUserDataTypeUserData,
			Attributes: &operations.PostUserDataUserDataAttributes{
				Description: description,
				Project:     latitudeshgosdk.String(project),
				Content:     contentB64,
			},
		},
	})
	if err != nil {
		return "", err
	}
	if resp.UserDataObject == nil || resp.UserDataObject.Data == nil || resp.UserDataObject.Data.ID == nil {
		return "", fmt.Errorf("user-data create returned no id")
	}
	return *resp.UserDataObject.Data.ID, nil
}

// list returns the project's UserData resources. The SDK's List exposes no
// pagination, so this returns the API's default page — adequate because it is
// only a best-effort fallback for recovering a UserData id when the persisted
// index is unavailable (the index is the primary path).
func (s *sdkUserData) list(ctx context.Context, project string) ([]userDataItem, error) {
	resp, err := s.sdk.UserData.List(ctx, latitudeshgosdk.String(project), nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.UserData == nil {
		return nil, nil
	}
	out := make([]userDataItem, 0, len(resp.UserData.Data))
	for i := range resp.UserData.Data {
		ud := resp.UserData.Data[i].Data
		if ud == nil || ud.ID == nil {
			continue
		}
		item := userDataItem{ID: *ud.ID}
		if ud.Attributes != nil && ud.Attributes.Description != nil {
			item.Description = *ud.Attributes.Description
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *sdkUserData) delete(ctx context.Context, id string) error {
	_, err := s.sdk.UserData.Delete(ctx, id)
	return err
}
