package providerkit

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/intUnderflow/bigfleet/pkg/proto/bigfleet/v1alpha1"
)

// fakeBackend is a controllable test [Backend] that also implements
// [Deleter]. Each actuator defaults to instant success; a test can override
// any of them with a hook (e.g. to inject an error or a delay).
type fakeBackend struct {
	seed []Instance

	mu          sync.Mutex
	createFn    func(context.Context, CreateInstanceRequest) (CreateInstanceResult, error)
	configureFn func(context.Context, ConfigureInstanceRequest) error
	drainFn     func(context.Context, DrainInstanceRequest) error
	deleteFn    func(context.Context, DeleteInstanceRequest) error
}

func (b *fakeBackend) Describe(context.Context) ([]Instance, error) { return b.seed, nil }

func (b *fakeBackend) CreateInstance(ctx context.Context, req CreateInstanceRequest) (CreateInstanceResult, error) {
	b.mu.Lock()
	fn := b.createFn
	b.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return CreateInstanceResult{Host: HostRef{Provider: "test", Ref: req.Machine.ID}}, nil
}

func (b *fakeBackend) ConfigureInstance(ctx context.Context, req ConfigureInstanceRequest) error {
	b.mu.Lock()
	fn := b.configureFn
	b.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return nil
}

func (b *fakeBackend) DrainInstance(ctx context.Context, req DrainInstanceRequest) error {
	b.mu.Lock()
	fn := b.drainFn
	b.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return nil
}

func (b *fakeBackend) DeleteInstance(ctx context.Context, req DeleteInstanceRequest) error {
	b.mu.Lock()
	fn := b.deleteFn
	b.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return nil
}

func (b *fakeBackend) setCreate(fn func(context.Context, CreateInstanceRequest) (CreateInstanceResult, error)) {
	b.mu.Lock()
	b.createFn = fn
	b.mu.Unlock()
}

func (b *fakeBackend) setConfigure(fn func(context.Context, ConfigureInstanceRequest) error) {
	b.mu.Lock()
	b.configureFn = fn
	b.mu.Unlock()
}

// bareBackend implements only the four [Backend] methods (no [Deleter]),
// modelling a bare-metal free-pool provider whose Delete must surface as
// codes.Unimplemented.
type bareBackend struct{ seed []Instance }

func (b *bareBackend) Describe(context.Context) ([]Instance, error) { return b.seed, nil }
func (b *bareBackend) CreateInstance(_ context.Context, req CreateInstanceRequest) (CreateInstanceResult, error) {
	return CreateInstanceResult{Host: HostRef{Provider: "bare", Ref: req.Machine.ID}}, nil
}
func (b *bareBackend) ConfigureInstance(context.Context, ConfigureInstanceRequest) error { return nil }
func (b *bareBackend) DrainInstance(context.Context, DrainInstanceRequest) error         { return nil }

// speculativeSeed builds n Speculative quota slots with valid field shape.
func speculativeSeed(n int, capType CapacityType, prob float64) []Instance {
	out := make([]Instance, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Instance{
			ID:                      "m-" + itoa(i),
			State:                   StateSpeculative,
			InstanceType:            "test-standard-4",
			Zone:                    "test-zone-a",
			CapacityType:            capType,
			PricePerHour:            0.5,
			InterruptionProbability: prob,
			Resources:               map[string]string{"cpu": "4", "memory": "16Gi"},
		})
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// quietOptions returns Options with a discard logger so test output stays
// clean even when a transition deliberately fails.
func quietOptions() Options {
	return Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// newTestServer builds a Server over a fresh fakeBackend seeded with n
// Speculative on-demand slots, backed by an in-memory store.
func newTestServer(t *testing.T, n int) (*Server, *fakeBackend) {
	t.Helper()
	b := &fakeBackend{seed: speculativeSeed(n, CapacityOnDemand, 0)}
	s, err := New(b, NewMemStore(), quietOptions())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, b
}

func bg() context.Context { return context.Background() }

// create/configure/drain/delete call the RPCs with a zero (unfenced) token.
func create(t *testing.T, s *Server, id string) *pb.TransitionAck {
	t.Helper()
	ack, err := s.Create(bg(), &pb.CreateRequest{MachineId: id})
	if err != nil {
		t.Fatalf("Create(%s): %v", id, err)
	}
	return ack
}

func configure(t *testing.T, s *Server, id, cluster string, md map[string]string) *pb.TransitionAck {
	t.Helper()
	ack, err := s.Configure(bg(), &pb.ConfigureRequest{
		MachineId: id, ClusterId: cluster, BootstrapBlob: []byte("# bootstrap\n"), ShardMetadata: md,
	})
	if err != nil {
		t.Fatalf("Configure(%s): %v", id, err)
	}
	return ack
}

func getMachine(t *testing.T, s *Server, id string) *pb.Machine {
	t.Helper()
	m, err := s.Get(bg(), &pb.MachineRef{Id: id})
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return m
}

// waitState polls Get until the machine reaches want or the deadline passes.
func waitState(t *testing.T, s *Server, id string, want pb.MachineState, timeout time.Duration) *pb.Machine {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m, err := s.Get(bg(), &pb.MachineRef{Id: id})
		if err == nil && m.GetState() == want {
			return m
		}
		time.Sleep(2 * time.Millisecond)
	}
	m, _ := s.Get(bg(), &pb.MachineRef{Id: id})
	t.Fatalf("machine %s did not reach %s within %s (final state %s)", id, want, timeout, m.GetState())
	return nil
}

func codeOf(err error) codes.Code { return status.Code(err) }

// firstSpeculative returns the id of one Speculative slot from the server.
func firstSpeculative(t *testing.T, s *Server) string {
	t.Helper()
	resp, err := s.List(bg(), &pb.ListFilter{
		States:     []pb.MachineState{pb.MachineState_MACHINE_STATE_SPECULATIVE},
		MaxResults: 1,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.GetMachines()) == 0 {
		t.Fatal("no Speculative machines seeded")
	}
	return resp.GetMachines()[0].GetId()
}
