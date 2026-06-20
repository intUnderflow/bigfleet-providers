package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitalocean/godo"
)

// fakeDOAPI is a minimal HTTP stand-in for the subset of the DigitalOcean REST
// API that doReal's create path touches: create a Droplet, get it by id, and
// list by name. It tracks how many create POSTs it received so a test can prove
// the pre-create idempotency lookup prevents a second, billed Droplet.
type fakeDOAPI struct {
	mu          sync.Mutex
	createCount int
	nextID      int
	byID        map[int]map[string]any
}

func newFakeDOAPI() *fakeDOAPI { return &fakeDOAPI{byID: map[int]map[string]any{}} }

func (a *fakeDOAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v2/droplets":
		var req struct {
			Name   string   `json:"name"`
			Region string   `json:"region"`
			Size   string   `json:"size"`
			Tags   []string `json:"tags"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		a.createCount++
		a.nextID++
		d := a.droplet(a.nextID, req.Name, req.Region, req.Size, req.Tags)
		a.byID[a.nextID] = d
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"droplet": d})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/droplets/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/v2/droplets/"))
		d, ok := a.byID[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "not_found"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"droplet": d})
	case r.Method == http.MethodGet && r.URL.Path == "/v2/droplets":
		name := r.URL.Query().Get("name")
		var match []map[string]any
		for _, d := range a.byID {
			if name == "" || d["name"] == name {
				match = append(match, d)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"droplets": match})
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "not_found"})
	}
}

func (a *fakeDOAPI) droplet(id int, name, region, size string, tags []string) map[string]any {
	return map[string]any{
		"id":        id,
		"name":      name,
		"status":    "active",
		"size_slug": size,
		"region":    map[string]any{"slug": region},
		"tags":      tags,
		"networks": map[string]any{
			"v4": []map[string]any{{"ip_address": "198.51.100.10", "type": "public"}},
		},
	}
}

func newTestDOReal(t *testing.T, baseURL string) *doReal {
	t.Helper()
	client, err := godo.New(http.DefaultClient, godo.SetBaseURL(baseURL))
	if err != nil {
		t.Fatalf("godo.New: %v", err)
	}
	return &doReal{
		cfg: doRealConfig{
			Region:            "nyc3",
			Image:             "ubuntu-24-04-x64",
			Vault:             newBootstrapVault([]byte("secret"), quietLogger()),
			BootstrapEndpoint: "https://do-provider.example:9443",
			CreateWaitTimeout: 5 * time.Second,
			PollInterval:      2 * time.Millisecond,
			SizesCacheTTL:     time.Minute,
		},
		client: client,
		logger: quietLogger(),
	}
}

// A re-dispatched Create with the same OperationID must NOT launch a second,
// billed Droplet: the pre-create lookup finds the one this operation already
// created and reuses it. This is the regression the fake alone cannot catch.
func TestDOReal_CreateDroplet_NoDoubleProvision(t *testing.T) {
	api := newFakeDOAPI()
	srv := httptest.NewServer(api)
	defer srv.Close()
	r := newTestDOReal(t, srv.URL+"/")

	spec := dropletSpec{MachineID: "m1", Size: "s-2vcpu-4gb", Region: "nyc3", IdempotencyToken: "op-1"}

	first, err := r.CreateDroplet(t.Context(), spec)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Re-dispatch the SAME operation (e.g. after a waitActive timeout / restart).
	second, err := r.CreateDroplet(t.Context(), spec)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}

	if api.createCount != 1 {
		t.Errorf("create POSTs = %d, want 1 (a second Droplet was provisioned)", api.createCount)
	}
	if first.DropletID != second.DropletID {
		t.Errorf("re-dispatched create returned a different droplet: %s vs %s", first.DropletID, second.DropletID)
	}
}

// A region-mismatched offering is refused so this single-region process never
// places a host outside the region it manages.
func TestDOReal_CreateDroplet_RejectsForeignRegion(t *testing.T) {
	api := newFakeDOAPI()
	srv := httptest.NewServer(api)
	defer srv.Close()
	r := newTestDOReal(t, srv.URL+"/")

	_, err := r.CreateDroplet(t.Context(), dropletSpec{MachineID: "m1", Size: "s-2vcpu-4gb", Region: "sfo3"})
	if err == nil {
		t.Fatal("expected a region-mismatch error, got nil")
	}
	if api.createCount != 0 {
		t.Errorf("create POSTs = %d, want 0 (foreign-region create must not reach the API)", api.createCount)
	}
}

// DescribeManaged must drop Droplets from other regions (DO tags are
// account-wide), so a single-region process never adopts a sibling's hosts.
func TestDOReal_DescribeManaged_FiltersRegion(t *testing.T) {
	api := newFakeDOAPI()
	srv := httptest.NewServer(api)
	defer srv.Close()
	r := newTestDOReal(t, srv.URL+"/")

	// One managed Droplet in-region, one in another region — both carry the
	// account-wide bigfleet-managed tag.
	api.mu.Lock()
	api.nextID++
	in := api.droplet(api.nextID, "in", "nyc3", "s-2vcpu-4gb", []string{tagManaged, tagMachinePrefix + encodeID("m-in")})
	api.byID[api.nextID] = in
	api.nextID++
	out := api.droplet(api.nextID, "out", "sfo3", "s-2vcpu-4gb", []string{tagManaged, tagMachinePrefix + encodeID("m-out")})
	api.byID[api.nextID] = out
	api.mu.Unlock()

	got, err := r.DescribeManaged(t.Context())
	if err != nil {
		t.Fatalf("DescribeManaged: %v", err)
	}
	if len(got) != 1 || got[0].Region != "nyc3" {
		t.Fatalf("DescribeManaged returned %d droplets %v, want exactly the nyc3 one", len(got), got)
	}
}
