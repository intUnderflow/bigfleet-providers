package main

import (
	"context"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
)

// mockService is a tiny in-memory upcloudService for unit-testing the real
// client's substrate logic (stop-then-delete, idempotency, EnsureRunning) without
// a live UpCloud account.
type mockService struct {
	details     map[string]*upcloud.ServerDetails
	stopped     []string
	deleted     []string
	startCalled []string
	createCalls int
}

func newMockService() *mockService {
	return &mockService{details: map[string]*upcloud.ServerDetails{}}
}

func (m *mockService) put(uuid, state string) {
	d := &upcloud.ServerDetails{}
	d.UUID = uuid
	d.State = state
	m.details[uuid] = d
}

// putManaged registers a server carrying the bigfleet-managed + machine-id
// labels, as DescribeManaged would see it.
func (m *mockService) putManaged(uuid, state, machineID string) {
	d := &upcloud.ServerDetails{}
	d.UUID = uuid
	d.State = state
	d.Labels = upcloud.LabelSlice{
		{Key: labelManaged, Value: "true"},
		{Key: labelMachineID, Value: encodeMachineID(machineID)},
	}
	m.details[uuid] = d
}

func (m *mockService) CreateServer(_ context.Context, _ *request.CreateServerRequest) (*upcloud.ServerDetails, error) {
	m.createCalls++
	return nil, nil
}
func (m *mockService) GetServerDetails(_ context.Context, r *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
	d, ok := m.details[r.UUID]
	if !ok {
		return nil, &upcloud.Problem{Status: 404, Title: "not found"}
	}
	return d, nil
}
func (m *mockService) GetServersWithFilters(_ context.Context, _ *request.GetServersWithFiltersRequest) (*upcloud.Servers, error) {
	out := &upcloud.Servers{}
	for uuid, d := range m.details {
		out.Servers = append(out.Servers, upcloud.Server{UUID: uuid, State: d.State})
	}
	return out, nil
}
func (m *mockService) StartServer(_ context.Context, r *request.StartServerRequest) (*upcloud.ServerDetails, error) {
	m.startCalled = append(m.startCalled, r.UUID)
	if d, ok := m.details[r.UUID]; ok {
		d.State = stateStarted
	}
	return m.details[r.UUID], nil
}
func (m *mockService) StopServer(_ context.Context, r *request.StopServerRequest) (*upcloud.ServerDetails, error) {
	m.stopped = append(m.stopped, r.UUID)
	if d, ok := m.details[r.UUID]; ok {
		d.State = stateStopped
	}
	return m.details[r.UUID], nil
}
func (m *mockService) WaitForServerState(_ context.Context, r *request.WaitForServerStateRequest) (*upcloud.ServerDetails, error) {
	if d, ok := m.details[r.UUID]; ok {
		d.State = r.DesiredState
		return d, nil
	}
	return nil, &upcloud.Problem{Status: 404}
}
func (m *mockService) DeleteServerAndStorages(_ context.Context, r *request.DeleteServerAndStoragesRequest) error {
	if _, ok := m.details[r.UUID]; !ok {
		return &upcloud.Problem{Status: 404}
	}
	m.deleted = append(m.deleted, r.UUID)
	delete(m.details, r.UUID)
	return nil
}
func (m *mockService) ModifyServer(_ context.Context, _ *request.ModifyServerRequest) (*upcloud.ServerDetails, error) {
	return nil, nil
}
func (m *mockService) GetPlans(_ context.Context) (*upcloud.Plans, error) {
	return &upcloud.Plans{Plans: []upcloud.Plan{{Name: "2xCPU-4GB", CoreNumber: 2, MemoryAmount: 4096}}}, nil
}

// GetPricesByZone returns a /price-shaped response: one priced plan in fi-hel1,
// quoted in credits/hour (1 credit = one cent), so the real client's credit→EUR
// conversion is exercised.
func (m *mockService) GetPricesByZone(_ context.Context) (*upcloud.PricesByZone, error) {
	return &upcloud.PricesByZone{
		"fi-hel1": {
			"server_plan_2xCPU-4GB": {Amount: 1, Price: 2.9761},
		},
	}, nil
}

func newRealWithMock(m *mockService) *upcloudReal {
	return &upcloudReal{cfg: upcloudRealConfig{Username: "u", Password: "p", Zone: "fi-hel1", Template: "tpl"}, svc: m, log: quietLogger()}
}

// Create must be idempotent at the substrate level even though UpCloud offers no
// client idempotency token and no title uniqueness: a Create for a machine id
// that already has a managed server ADOPTS it (pre-create lookup) instead of
// launching a second server — the double-provision the reference review flagged.
func TestReal_Create_AdoptsExistingByMachineID(t *testing.T) {
	m := newMockService()
	m.putManaged("srv-existing", stateStarted, "machine-1")
	r := newRealWithMock(m)

	got, err := r.CreateServer(context.Background(), serverSpec{MachineID: "machine-1", Plan: "2xCPU-4GB", Zone: "fi-hel1", Template: "tpl"})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if got.UUID != "srv-existing" {
		t.Errorf("adopted UUID = %q, want srv-existing", got.UUID)
	}
	if m.createCalls != 0 {
		t.Errorf("CreateServer hit the substrate %d times; want 0 (must adopt, not double-provision)", m.createCalls)
	}
	if len(m.details) != 1 {
		t.Errorf("ended with %d servers, want 1 (no duplicate)", len(m.details))
	}
}

// Delete on a running server must STOP it first (UpCloud refuses to delete a
// running server), then delete server AND storage in one shot.
func TestReal_Delete_StopsThenDeletesWithStorage(t *testing.T) {
	m := newMockService()
	m.put("srv-1", stateStarted)
	r := newRealWithMock(m)
	if err := r.DeleteServer(context.Background(), "srv-1"); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	if len(m.stopped) != 1 || m.stopped[0] != "srv-1" {
		t.Errorf("server not stopped before delete: %v", m.stopped)
	}
	if len(m.deleted) != 1 || m.deleted[0] != "srv-1" {
		t.Errorf("DeleteServerAndStorages not called: %v", m.deleted)
	}
}

// Delete is idempotent: an already-gone server is success (no error).
func TestReal_Delete_IdempotentWhenGone(t *testing.T) {
	m := newMockService()
	r := newRealWithMock(m)
	if err := r.DeleteServer(context.Background(), "ghost"); err != nil {
		t.Errorf("DeleteServer on missing server should succeed, got %v", err)
	}
}

// EnsureRunning powers on a stopped server; it is a no-op for a started one.
func TestReal_EnsureRunning(t *testing.T) {
	m := newMockService()
	m.put("srv-2", stateStopped)
	r := newRealWithMock(m)
	if _, err := r.EnsureRunning(context.Background(), serverInstance{UUID: "srv-2"}); err != nil {
		t.Fatalf("EnsureRunning(stopped): %v", err)
	}
	if len(m.startCalled) != 1 {
		t.Errorf("stopped server not started: %v", m.startCalled)
	}

	m.startCalled = nil
	m.put("srv-3", stateStarted)
	if _, err := r.EnsureRunning(context.Background(), serverInstance{UUID: "srv-3"}); err != nil {
		t.Fatalf("EnsureRunning(started): %v", err)
	}
	if len(m.startCalled) != 0 {
		t.Errorf("started server should not be re-started: %v", m.startCalled)
	}
}

func TestReal_DescribePlanCapacities(t *testing.T) {
	m := newMockService()
	r := newRealWithMock(m)
	caps, err := r.DescribePlanCapacities(context.Background(), []string{"2xCPU-4GB", "nope"})
	if err != nil {
		t.Fatalf("DescribePlanCapacities: %v", err)
	}
	c, ok := caps["2xCPU-4GB"]
	if !ok || c.Cores != 2 || c.MemMiB != 4096 {
		t.Errorf("plan capacity = %+v (ok=%v), want {2 4096}", c, ok)
	}
	if _, ok := caps["nope"]; ok {
		t.Error("unknown plan should be omitted")
	}
}

// DescribePlanPrices reads the /price endpoint, scopes to the provider's zone,
// and converts UpCloud credits/hour (1 credit = one cent) to EUR/hour. An unpriced
// plan is omitted so the caller keeps its pinned fallback.
func TestReal_DescribePlanPrices(t *testing.T) {
	m := newMockService()
	r := newRealWithMock(m)
	prices, err := r.DescribePlanPrices(context.Background(), []string{"2xCPU-4GB", "nope"})
	if err != nil {
		t.Fatalf("DescribePlanPrices: %v", err)
	}
	credits := 2.9761                      // as quoted by the /price endpoint
	want := credits / upcloudCreditsPerEUR // credits/hour -> EUR/hour (runtime division matches the client)
	if got, ok := prices["2xCPU-4GB"]; !ok || got != want {
		t.Errorf("2xCPU-4GB price = %v (ok=%v), want %v EUR/hour", got, ok, want)
	}
	if _, ok := prices["nope"]; ok {
		t.Error("unpriced plan should be omitted (caller keeps pinned fallback)")
	}
}
