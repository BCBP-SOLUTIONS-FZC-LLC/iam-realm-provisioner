package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/eventschema"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// errSchemaViolation marks a consumed payload that fails its
// consumed-message schema. It is permanent — the same bytes fail on every
// redelivery — but, per arch-lint's events invariant (no hand-rolled
// SQS/DLQ logic outside platform-events), it is returned as an ordinary
// handler error: the handler sees it on 4 deliveries, dlqHandler observes
// the 5th, then the queue's own RedrivePolicy (maxReceiveCount=5) moves it
// to -dlq. Validation still
// guarantees it never reaches the handler or processed_events.
var errSchemaViolation = errors.New("consumed event payload violates its consumed-message schema")

// payloadValidator is satisfied by *eventbus.ValidatingCodec (built with
// eventbus.NewConsumedValidator).
type payloadValidator interface {
	Validate(eventType string, payload []byte) error
}

// consumedValidator compiles consumer's consumed-message schemas
// (eventschema.ConsumedByConsumer). Per consumer, never the produced
// ByEventType set: billing-realm-q's TenantReactivated is Billing's, not
// this service's own source=operator TenantReactivated.
func consumedValidator(consumer string) (*eventbusadapter.ValidatingCodec, error) {
	return eventbusadapter.NewConsumedValidator(eventschema.ConsumedByConsumer[consumer])
}

// validateConsumed checks each inbound envelope's (already Glue-decoded)
// payload against this service's own consumed-message schema for env.Type
// before h runs — what api/asyncapi.yaml says this service relies on, not
// the producer's Glue schema, so it needs no Glue call and keeps working
// through a Glue outage.
//
// An event type with no consumed schema passes through untouched, so the
// consumer's ackUnknown forward-compat path still sees it. A violation is
// logged, counted (once per delivery) as
// iam_realm_provisioner_consumed_schema_violations_total, and returned as
// errSchemaViolation so SQS redelivers it until redrive.
func validateConsumed(consumer string, h events.Handler, v payloadValidator, log port.Logger) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		err := v.Validate(env.Type, env.Payload)
		if err == nil || errors.Is(err, eventbusadapter.ErrNoSchema) {
			return h(ctx, env)
		}
		metrics.ConsumedSchemaViolationsTotal.WithLabelValues(consumer, env.Type).Inc()
		if log != nil {
			log.Warn("consumed event violates its consumed-message schema — rejected before the handler, left for SQS redrive to -dlq",
				map[string]any{"consumer": consumer, "event_id": env.ID, "event_type": env.Type, "source": env.Source, "error": err.Error()})
		}
		return fmt.Errorf("%w: %w", errSchemaViolation, err)
	}
}

// inboundHandler composes the full inbound pipeline for one queue:
// consumed-schema validation → the consumer's own Handle. The Tier 1
// message-lifecycle counters (platform_messages_received/processed/
// failed_total, by queue and event_type) are recorded by platform-events'
// SQS consumer around this handler, so a schema reject counts as
// platform_messages_failed_total{reason="handler_error"}.
func inboundHandler(consumer string, h events.Handler, v payloadValidator, log port.Logger) events.Handler {
	return validateConsumed(consumer, h, v, log)
}
