package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/oracle/oci-go-sdk/v65/streaming"

	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

func newTestPoller(in *interruption, stream streamAPI) *preemptionPoller {
	backend := &ociBackend{interruption: in, client: newOCIFake(), logger: slog.Default()}
	return &preemptionPoller{stream: stream, streamID: "s", backend: backend, logger: slog.Default()}
}

func TestPreemptionPoller_RaisesProbabilityFromTag(t *testing.T) {
	in := newInterruption()
	p := newTestPoller(in, nil)
	ev := `{"eventType":"com.oraclecloud.computeapi.instancepreemptionaction","data":{"resourceId":"ocid1.instance.oc1..a","freeFormTags":{"bigfleet-machine-id":"m-1"}}}`
	p.handle(context.Background(), []byte(ev))
	if got := in.probability("m-1", "VM.Standard.E5.Flex", providerkit.CapacitySpot); got != preemptionProbability {
		t.Fatalf("probability = %v, want %v", got, preemptionProbability)
	}
}

func TestPreemptionPoller_IgnoresNonPreemptionEvent(t *testing.T) {
	in := newInterruption()
	p := newTestPoller(in, nil)
	ev := `{"eventType":"com.oraclecloud.computeapi.terminateinstance.begin","data":{"freeFormTags":{"bigfleet-machine-id":"m-2"}}}`
	p.handle(context.Background(), []byte(ev))
	if got := in.probability("m-2", "VM.Standard.E5.Flex", providerkit.CapacitySpot); got == preemptionProbability {
		t.Fatalf("a non-preemption event must not raise probability (got %v)", got)
	}
}

func TestPreemptionPoller_IgnoresUnknownMachine(t *testing.T) {
	in := newInterruption()
	p := newTestPoller(in, nil)
	// No freeform tag and the fake has no such instance → machineIDFor returns "".
	ev := `{"eventType":"com.oraclecloud.computeapi.instancepreemptionaction","data":{"resourceId":"ocid1.instance.oc1..ghost"}}`
	p.handle(context.Background(), []byte(ev))
	if got := len(in.observed); got != 0 {
		t.Fatalf("observed should be empty for an unmanaged instance, got %d", got)
	}
}

// fakeStream returns one batch of messages, then cancels the run loop and returns
// empty, so run() processes exactly the supplied batch and exits.
type fakeStream struct {
	batches [][]streaming.Message
	idx     int
	cancel  context.CancelFunc
}

func (f *fakeStream) CreateGroupCursor(context.Context, streaming.CreateGroupCursorRequest) (streaming.CreateGroupCursorResponse, error) {
	v := "cursor"
	return streaming.CreateGroupCursorResponse{Cursor: streaming.Cursor{Value: &v}}, nil
}

func (f *fakeStream) GetMessages(context.Context, streaming.GetMessagesRequest) (streaming.GetMessagesResponse, error) {
	if f.idx < len(f.batches) {
		b := f.batches[f.idx]
		f.idx++
		next := "next"
		return streaming.GetMessagesResponse{Items: b, OpcNextCursor: &next}, nil
	}
	if f.cancel != nil {
		f.cancel()
	}
	return streaming.GetMessagesResponse{}, nil
}

func TestPreemptionPoller_RunProcessesBatch(t *testing.T) {
	in := newInterruption()
	ctx, cancel := context.WithCancel(context.Background())
	ev := []byte(`{"eventType":"com.oraclecloud.computeapi.instancepreemptionaction","data":{"freeFormTags":{"bigfleet-machine-id":"m-9"}}}`)
	fs := &fakeStream{batches: [][]streaming.Message{{{Value: ev}}}, cancel: cancel}
	p := newTestPoller(in, fs)
	p.run(ctx)
	if got := in.probability("m-9", "VM.Standard.E5.Flex", providerkit.CapacitySpot); got != preemptionProbability {
		t.Fatalf("run() did not raise probability via the stream: got %v", got)
	}
}
