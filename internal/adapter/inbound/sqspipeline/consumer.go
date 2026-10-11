// Package sqspipeline builds this service's three inbound SQS consumers
// (§7.1: membership-realm-q, tenant-realm-q, billing-realm-q) entirely from
// platform-events: the library's env contract (eventcfg.LoadSQS /
// SQSConsumerOptions / SQSConfigFromEnv), its decode-only Glue codec, and
// its DLQ forwarder (events.WithDLQForwarding). Every handler is wrapped in
// consumed-schema validation, a consumer span, and the straight-to-DLQ
// route for permanent rejects — matching iam-org-membership's sqspipeline.
package sqspipeline

import (
	"context"
	"encoding/json"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// dlqForwardReceiveCount is the consumer-side threshold above which
// platform-events forwards a still-failing message to the queue's DLQ
// itself (events.WithDLQForwarding): a delivery whose ApproximateReceiveCount
// exceeds it — the 5th — is forwarded instead of handled. That is the last
// delivery before the inbound queues' own RedrivePolicy maxReceiveCount=5
// (§7.1, scripts/init-floci.sh) would move it, so the library's forward
// (with its reason attribution and platform_dlq_messages_total) always wins
// over SQS's silent redrive. SQS_MAX_RECEIVE_COUNT overrides it.
const dlqForwardReceiveCount = 4

// Library defaults this service sets when the env leaves them unset.
const (
	sqsDrainTimeout       = 15 * time.Second
	sqsHandlerTimeout     = 20 * time.Second
	sqsQueueDepthInterval = 60 * time.Second
)

// tracerScope names this service's consumer spans.
const tracerScope = "iam-realm-provisioner"

// EnvForQueue overlays one queue's URL and concurrency onto the shared
// eventcfg.LoadSQS() result (region, endpoint, long-poll, visibility,
// max-receive come from the library's SQS_* env contract; SQS_QUEUE_URL /
// SQS_CONCURRENCY cannot serve three queues) and fills this service's
// defaults for anything the env left unset.
func EnvForQueue(base eventcfg.SQSConfigEnv, queueURL string, concurrency int) eventcfg.SQSConfigEnv {
	env := base
	env.QueueURL = queueURL
	if concurrency > 0 {
		env.Concurrency = concurrency
	}
	if env.MaxReceiveCount <= 0 {
		env.MaxReceiveCount = dlqForwardReceiveCount
	}
	if env.DrainTimeout <= 0 {
		env.DrainTimeout = sqsDrainTimeout
	}
	if env.HandlerTimeout <= 0 {
		env.HandlerTimeout = sqsHandlerTimeout
	}
	if env.QueueDepthInterval <= 0 {
		env.QueueDepthInterval = sqsQueueDepthInterval
	}
	return env
}

// NewConsumer builds one inbound consumer: the library's env-derived
// options, the decode-only Glue codec (upstream producers' Glue-framed
// payloads, no registry needed to decode) and DLQ forwarding through dlq.
// A nil client lets platform-events build its own from env's Region /
// EndpointURL (tests against floci); cmd/server always shares one client.
func NewConsumer(env eventcfg.SQSConfigEnv, client events.SQSClientLike, handler events.Handler, dlq events.DLQPublisher, log port.Logger) (events.Consumer, error) {
	opts := append(eventcfg.SQSConsumerOptions(env), events.WithConsumerCodec(events.GlueDecodeCodec{}))
	if dlq != nil {
		opts = append(opts, events.WithDLQForwarding(dlq))
	}
	if client == nil {
		return events.NewSQSConsumer(eventcfg.SQSConfigFromEnv(env, log), handler, opts...)
	}
	return events.NewSQSConsumerWithClient(eventcfg.SQSConfigFromEnv(env, log), client, handler, opts...)
}

// instrumentedHandler wraps h in a consumer.<event_type> span. The span is
// linked (not parented) to the producer's span when the envelope carries
// one, so a provision → event → consume chain stays navigable without
// making every consume a child of a long-finished request.
func instrumentedHandler(queue string, h events.Handler) events.Handler {
	tracer := gincommon.NewTracer(tracerScope)
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		spanOpts := []trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attribute.String("messaging.destination.name", queue)),
		}
		if link, ok := originLink(env); ok {
			spanOpts = append(spanOpts, trace.WithLinks(link))
		}
		ctx, span := tracer.Start(ctx, "consumer."+env.Type, spanOpts...)
		defer span.End()
		if err := h(ctx, env); err != nil {
			span.RecordError(err)
			return err
		}
		return nil
	}
}

// originLink builds a span link to the producer's span from the envelope's
// trace_id / correlation_id, when both are valid.
func originLink(env events.Envelope[json.RawMessage]) (trace.Link, bool) {
	tid, err := trace.TraceIDFromHex(env.TraceID)
	if err != nil {
		return trace.Link{}, false
	}
	sid, err := trace.SpanIDFromHex(env.CorrelationID)
	if err != nil {
		return trace.Link{}, false
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, Remote: true})
	if !sc.IsValid() {
		return trace.Link{}, false
	}
	return trace.Link{SpanContext: sc}, true
}
