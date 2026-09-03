package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/trace"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
)

// Publisher implements port.EventPublisher. It JSON-marshals the event
// payload, validates it via the configured Codec (schema validation only —
// no wire encoding), wraps it as plain JSON in an events.Envelope, and
// inserts it into outbox_events on the transaction TxRunner attached to
// ctx — atomic with the business write (RP-INV-3).
//
// Glue/wire encoding is NOT performed here. It is deferred to the SNS
// publisher (outbox runner publish path) via events.WithCodec so the
// outbox stores human-readable plain JSON for observability and replay.
type Publisher struct {
	source string
	codec  Codec
	log    port.SlogStyleLogger
}

// New builds a Publisher that stamps source on every enqueued envelope and
// validates payloads via codec.
func New(source string, codec Codec) *Publisher {
	return &Publisher{source: source, codec: codec}
}

// WithLogger attaches a logger for enqueue diagnostics and returns the
// publisher for chaining.
func (p *Publisher) WithLogger(log port.Logger) *Publisher {
	p.log = port.NewSlogStyleLogger(log)
	return p
}

var _ port.EventPublisher = (*Publisher)(nil)

// Enqueue validates and writes one plain-JSON envelope into outbox_events
// on the transaction TxRunner stored in ctx. Glue encoding is deferred to
// the outbox runner's SNS publisher (WithCodec).
func (p *Publisher) Enqueue(ctx context.Context, event *domain.Event) error {
	raw, err := json.Marshal(event.Data)
	if err != nil {
		return fmt.Errorf("marshal event payload: %w", err)
	}
	if _, _, err := p.codec.Encode(ctx, event.Type, raw); err != nil {
		return fmt.Errorf("validate event %s: %w", event.Type, err)
	}
	opts := []events.EnvelopeOpt{
		events.WithTenantID(event.TenantID.String()),
		events.WithSubject(event.Subject),
		events.WithActor(event.Actor),
		events.WithSchemaVersion("1"),
	}
	if event.IPAddress != "" {
		opts = append(opts, events.WithIPAddress(event.IPAddress))
	}
	if event.UserAgent != "" {
		opts = append(opts, events.WithUserAgent(event.UserAgent))
	}
	if spanCtx := trace.SpanFromContext(ctx).SpanContext(); spanCtx.IsValid() {
		opts = append(opts,
			events.WithTraceID(spanCtx.TraceID().String()),
			events.WithCorrelationID(spanCtx.SpanID().String()),
		)
	}
	env := events.NewEnvelope(event.Type, p.source, json.RawMessage(raw), opts...)
	tx, ok := pgadapter.TxFromContext(ctx)
	if !ok {
		return errors.New("event enqueue requires an open RunInTx transaction")
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		p.log.ErrorContext(ctx, "outbox.Enqueue failed", "type", event.Type, "error", err.Error())
		return err
	}
	p.log.DebugContext(ctx, "outbox.Enqueue", "type", event.Type, "event_id", env.ID)
	return nil
}
