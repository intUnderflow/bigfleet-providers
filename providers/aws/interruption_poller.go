package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// sqsAPI is the subset of the SQS SDK the interruption poller uses, so the
// poller is unit-testable with an injected fake. *sqs.Client satisfies it.
type sqsAPI interface {
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

// ebEvent is the subset of an EventBridge event we read off the queue.
type ebEvent struct {
	DetailType string `json:"detail-type"`
	Detail     struct {
		InstanceID string `json:"instance-id"`
	} `json:"detail"`
}

// probabilityForEvent maps a spot EventBridge event to the observed
// interruption probability to publish for the affected machine.
func probabilityForEvent(detailType string) (float64, bool) {
	switch detailType {
	case "EC2 Spot Instance Interruption Warning":
		return 0.99, true // the 2-minute kill notice — interruption is imminent
	case "EC2 Instance Rebalance Recommendation":
		return 0.5, true // elevated risk; AWS suggests proactively moving off
	default:
		return 0, false
	}
}

// interruptionPoller consumes EC2 spot interruption / rebalance events from an
// SQS queue (fed by an EventBridge rule) and raises the OBSERVED interruption
// probability of the affected machine. The periodic Server.Reconcile then
// propagates the raised value into inventory, so the engine's victim scoring
// sees a real, rising probability for a machine about to be reclaimed.
type interruptionPoller struct {
	sqs      sqsAPI
	queueURL string
	backend  *awsBackend
	m        *metrics
	logger   *slog.Logger
}

func newInterruptionPoller(ctx context.Context, region, queueURL string, backend *awsBackend, m *metrics, logger *slog.Logger) (*interruptionPoller, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("interruption poller: load AWS config: %w", err)
	}
	return &interruptionPoller{
		sqs: sqs.NewFromConfig(cfg), queueURL: queueURL, backend: backend, m: m, logger: logger,
	}, nil
}

func (p *interruptionPoller) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		out, err := p.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(p.queueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     20, // long-poll
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.logger.Warn("interruption poller: receive failed", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, msg := range out.Messages {
			p.handle(ctx, aws.ToString(msg.Body))
			_, _ = p.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl: aws.String(p.queueURL), ReceiptHandle: msg.ReceiptHandle,
			})
		}
	}
}

// handle parses one event body and, if it is a spot interruption/rebalance for
// a managed instance, raises that machine's observed interruption probability.
// Exported-for-test via the unexported method on the poller.
func (p *interruptionPoller) handle(ctx context.Context, body string) {
	var ev ebEvent
	if err := json.Unmarshal([]byte(body), &ev); err != nil {
		return
	}
	prob, ok := probabilityForEvent(ev.DetailType)
	if !ok || ev.Detail.InstanceID == "" {
		return
	}
	mid := p.backend.machineIDFor(ctx, ev.Detail.InstanceID)
	if mid == "" {
		return // not one of ours (or already gone)
	}
	p.backend.interruption.markWarning(mid, prob)
	if p.m != nil {
		p.m.interrupts.Inc()
	}
	p.logger.Info("observed spot interruption", "instance", ev.Detail.InstanceID, "machine", mid, "probability", prob, "event", ev.DetailType)
}
