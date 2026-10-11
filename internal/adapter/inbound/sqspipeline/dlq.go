package sqspipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// schemaViolationReason is the DLQ reason attribute for a consumed payload
// that fails its consumed-message schema.
const schemaViolationReason = "schema_violation"

// dlqStartupTimeout bounds RequireDLQ's RedrivePolicy lookup.
const dlqStartupTimeout = 5 * time.Second

// dlqReason reports whether err is a permanent reject that goes straight to
// the DLQ (the same bytes fail on every redelivery), and with which reason.
func dlqReason(err error) (string, bool) {
	if errors.Is(err, errSchemaViolation) {
		return schemaViolationReason, true
	}
	return "", false
}

// RequireDLQ is the startup preflight for one inbound queue: it fails when
// the queue has no usable RedrivePolicy (dead-lettered messages would be
// lost), and only warns when the lookup itself fails transiently — the
// forwarder resolves the DLQ again on first use.
func RequireDLQ(ctx context.Context, dlq events.DLQPublisher, queueURL string, log port.Logger) error {
	rctx, cancel := context.WithTimeout(ctx, dlqStartupTimeout)
	defer cancel()
	dlqURL, err := dlq.ResolveDLQ(rctx, queueURL)
	switch {
	case err == nil:
		if log != nil {
			log.Info("DLQ forwarding wired", map[string]any{"queue_url": queueURL, "dlq_url": dlqURL})
		}
		return nil
	case errors.Is(err, events.ErrDLQNotConfigured), errors.Is(err, events.ErrDLQInvalidRedrivePolicy):
		return fmt.Errorf("queue %s has no usable RedrivePolicy, so dead-lettered messages would be lost: %w", queueURL, err)
	default:
		if log != nil {
			log.Warn("could not resolve DLQ at startup; forwards will resolve it again",
				map[string]any{"queue_url": queueURL, "error": err.Error()})
		}
		return nil
	}
}

// routeRejectsToDLQ sends a permanent reject (dlqReason) straight to the
// queue's DLQ through platform-events' DLQ publisher instead of letting SQS
// redeliver it until redrive. If the send fails, the original error is
// returned so the message stays on the queue for retry and redrive.
func routeRejectsToDLQ(h events.Handler, dlq events.DLQPublisher, log port.Logger) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		err := h(ctx, env)
		reason, ok := dlqReason(err)
		if !ok || dlq == nil {
			return err
		}
		src, ok := events.SourceMessageFromContext(ctx)
		if !ok {
			return err
		}
		if sErr := dlq.SendToDLQ(ctx, src.QueueURL, src.Body, maps.Clone(src.Attributes), reason); sErr != nil {
			if log != nil {
				log.Warn("DLQ send failed — message stays on the queue for retry and redrive",
					map[string]any{"event_id": env.ID, "event_type": env.Type, "reason": reason, "queue_url": src.QueueURL, "error": sErr.Error()})
			}
			return err
		}
		return nil
	}
}
