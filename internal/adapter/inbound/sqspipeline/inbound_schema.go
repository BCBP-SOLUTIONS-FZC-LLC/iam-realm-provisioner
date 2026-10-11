package sqspipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// errSchemaViolation marks a consumed payload that fails its
// consumed-message schema. It is permanent — the same bytes fail on every
// redelivery — so routeRejectsToDLQ sends it straight to the DLQ.
var errSchemaViolation = errors.New("consumed event payload violates its consumed-message schema")

// Validator checks one decoded payload against this service's own
// consumed-message schema for eventType. It returns nil when eventType has
// no consumed schema, so the consumer's ackUnknown forward-compat path
// still sees it.
type Validator interface {
	Validate(eventType string, payload []byte) error
}

// ValidatorFunc adapts a function to Validator.
type ValidatorFunc func(eventType string, payload []byte) error

// Validate implements Validator.
func (f ValidatorFunc) Validate(eventType string, payload []byte) error { return f(eventType, payload) }

// validateConsumed checks each inbound envelope's (already Glue-decoded)
// payload before h runs. A violation is logged, counted as
// iam_realm_provisioner_consumed_schema_violations_total, and returned as
// errSchemaViolation — it never reaches the handler or processed_events.
func validateConsumed(consumer string, h events.Handler, v Validator, log port.Logger) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		err := v.Validate(env.Type, env.Payload)
		if err == nil {
			return h(ctx, env)
		}
		if metrics.ConsumedSchemaViolationsTotal != nil {
			metrics.ConsumedSchemaViolationsTotal.WithLabelValues(consumer, env.Type).Inc()
		}
		if log != nil {
			log.Warn("consumed event violates its consumed-message schema — rejecting to DLQ",
				map[string]any{"consumer": consumer, "event_id": env.ID, "event_type": env.Type, "source": env.Source, "error": err.Error()})
		}
		return fmt.Errorf("%w: %w", errSchemaViolation, err)
	}
}

// Handler composes the full inbound pipeline for one queue: consumed-schema
// validation → a consumer span → the consumer's own Handle, with permanent
// rejects routed straight to the DLQ. consumer is the frozen
// processed_events.consumer value (the schema-violation metric label);
// queue names the span attribute. The Tier 1 message-lifecycle counters
// (platform_messages_*) are recorded by platform-events around this.
func Handler(consumer, queue string, h events.Handler, v Validator, dlq events.DLQPublisher, log port.Logger) events.Handler {
	return routeRejectsToDLQ(instrumentedHandler(queue, validateConsumed(consumer, h, v, log)), dlq, log)
}
