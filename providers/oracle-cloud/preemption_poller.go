package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/oracle/oci-go-sdk/v65/streaming"
)

// streamAPI is the subset of the OCI Streaming data-plane client the preemption
// poller uses, so the poller is unit-testable with an injected fake.
// streaming.StreamClient satisfies it.
type streamAPI interface {
	CreateGroupCursor(context.Context, streaming.CreateGroupCursorRequest) (streaming.CreateGroupCursorResponse, error)
	GetMessages(context.Context, streaming.GetMessagesRequest) (streaming.GetMessagesResponse, error)
}

// preemptionEventType is the OCI Events service event a preemptible instance
// emits ~2 minutes before it is reclaimed.
const preemptionEventType = "com.oraclecloud.computeapi.instancepreemptionaction"

// preemptionProbability is the observed interruption probability published for a
// machine once its preemption-action event is seen — the reclaim is imminent
// (matches the AWS spot-interruption-warning treatment).
const preemptionProbability = 0.99

// cursorGroup names the consumer group. With CommitOnGet the committed offset is
// durable, so the poller resumes after its last-read message across restarts
// (the cursor Type is only honoured when the group is first created).
const cursorGroup = "bigfleet-preemption"

// ociEvent is the subset of the OCI CloudEvents envelope the poller reads off the
// stream. The preempted instance's freeform tags ride along in the event data, so
// the BigFleet machine id is usually recoverable with no extra API call.
type ociEvent struct {
	EventType string `json:"eventType"`
	Data      struct {
		ResourceID   string            `json:"resourceId"`   // preempted instance OCID
		FreeFormTags map[string]string `json:"freeFormTags"` // carries bigfleet-machine-id
	} `json:"data"`
}

// preemptionPoller consumes OCI preemption-action events from a Streaming stream
// (fed by an Events rule) and raises the OBSERVED interruption probability of the
// affected machine toward 1.0. The periodic Server.Reconcile then propagates the
// raised value into inventory, so the engine's victim scoring sees a real, rising
// probability for a machine about to be reclaimed. It is the OCI analogue of the
// AWS SQS interruption poller.
type preemptionPoller struct {
	stream   streamAPI
	streamID string
	backend  *ociBackend
	m        *metrics
	logger   *slog.Logger
}

// newPreemptionPoller builds the poller. It resolves the stream's per-stream
// messages endpoint via the Streaming admin (control-plane) API, then builds the
// data-plane client against that endpoint. It reuses the provider's OCI auth, so
// it needs no extra credentials.
func newPreemptionPoller(ctx context.Context, authMode, region, streamID string, backend *ociBackend, m *metrics, logger *slog.Logger) (*preemptionPoller, error) {
	provider, err := configurationProvider(authMode, logger)
	if err != nil {
		return nil, fmt.Errorf("preemption poller: %w", err)
	}
	admin, err := streaming.NewStreamAdminClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, fmt.Errorf("preemption poller: build stream admin client: %w", err)
	}
	admin.SetRegion(region)
	res, err := admin.GetStream(ctx, streaming.GetStreamRequest{StreamId: &streamID})
	if err != nil {
		return nil, fmt.Errorf("preemption poller: get stream %s: %w", streamID, err)
	}
	if res.MessagesEndpoint == nil {
		return nil, fmt.Errorf("preemption poller: stream %s has no messages endpoint", streamID)
	}
	sc, err := streaming.NewStreamClientWithConfigurationProvider(provider, *res.MessagesEndpoint)
	if err != nil {
		return nil, fmt.Errorf("preemption poller: build stream client: %w", err)
	}
	return &preemptionPoller{stream: sc, streamID: streamID, backend: backend, m: m, logger: logger}, nil
}

func (p *preemptionPoller) run(ctx context.Context) {
	cursor, err := p.groupCursor(ctx)
	if err != nil {
		p.logger.Warn("preemption poller: create cursor failed; not watching for preemptions", "err", err)
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		limit := 10
		res, err := p.stream.GetMessages(ctx, streaming.GetMessagesRequest{
			StreamId: &p.streamID, Cursor: cursor, Limit: &limit,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.logger.Warn("preemption poller: get messages failed", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			// A cursor can expire; rebuild it before the next read.
			if c, cerr := p.groupCursor(ctx); cerr == nil {
				cursor = c
			}
			continue
		}
		for _, msg := range res.Items {
			p.handle(ctx, msg.Value)
		}
		if res.OpcNextCursor != nil {
			cursor = res.OpcNextCursor
		}
		// OCI GetMessages is a short poll (unlike SQS's 20s long-poll), so pause
		// briefly when the stream is idle rather than hot-looping.
		if len(res.Items) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// groupCursor creates a LATEST consumer-group cursor with commit-on-get, so the
// poller sees only new events and durably advances its committed offset.
func (p *preemptionPoller) groupCursor(ctx context.Context) (*string, error) {
	commit := true
	group := cursorGroup
	res, err := p.stream.CreateGroupCursor(ctx, streaming.CreateGroupCursorRequest{
		StreamId: &p.streamID,
		CreateGroupCursorDetails: streaming.CreateGroupCursorDetails{
			Type:        streaming.CreateGroupCursorDetailsTypeLatest,
			GroupName:   &group,
			CommitOnGet: &commit,
		},
	})
	if err != nil {
		return nil, err
	}
	return res.Value, nil
}

// handle decodes one stream message and, if it is a preemption-action event for a
// managed instance, raises that machine's observed interruption probability.
func (p *preemptionPoller) handle(ctx context.Context, value []byte) {
	var ev ociEvent
	if err := json.Unmarshal(value, &ev); err != nil {
		return
	}
	if ev.EventType != preemptionEventType {
		return
	}
	// The event payload usually carries the instance's freeform tags, so the
	// machine id needs no extra API call; fall back to an OCID lookup if absent.
	mid := ev.Data.FreeFormTags[tagMachineID]
	if mid == "" && ev.Data.ResourceID != "" {
		mid = p.backend.machineIDFor(ctx, ev.Data.ResourceID)
	}
	if mid == "" {
		return // not one of ours (or already gone)
	}
	p.backend.interruption.markPreemption(mid, preemptionProbability)
	if p.m != nil {
		p.m.interrupts.Inc()
	}
	p.logger.Info("observed preemption", "instance", ev.Data.ResourceID, "machine", mid, "probability", preemptionProbability)
}
