package consumer

import (
	"context"
	"encoding/json"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// skipDuplicate is the cheap processed_events pre-check a handler runs
// before an external side effect (a Keycloak call) that must not repeat on
// a duplicate delivery. A hit increments processed_events_duplicates_total
// (IDEMP-4) and short-circuits.
func skipDuplicate(ctx context.Context, inbox port.Inbox, consumer domain.ProcessedEventsConsumer, eventID string) (bool, error) {
	processed, err := inbox.IsProcessed(ctx, consumer, eventID)
	if err != nil {
		return false, err
	}
	if processed {
		countDuplicate(consumer)
		return true, nil
	}
	return false, nil
}

// processOnce runs fn exactly once per (consumer, env.ID) through the
// platform-events inbox (port.Inbox): the processed_events claim and fn's
// writes commit in one transaction (IDEMP-1..4). A duplicate skips fn and
// increments iam_realm_provisioner_processed_events_duplicates_total; the
// inbox itself records the Tier 1 platform_duplicate_messages_total.
func processOnce(ctx context.Context, inbox port.Inbox, consumer domain.ProcessedEventsConsumer, env events.Envelope[json.RawMessage], fn func(txCtx context.Context) error) error {
	duplicate, err := inbox.ProcessOnce(ctx, consumer, env.ID, env.Type, fn)
	if err != nil {
		return err
	}
	if duplicate {
		countDuplicate(consumer)
	}
	return nil
}

// noLocalWrite is the processOnce fn for a handler whose effect is not a
// Postgres write — only the processed_events claim commits.
func noLocalWrite(context.Context) error { return nil }

func countDuplicate(consumer domain.ProcessedEventsConsumer) {
	if metrics.ProcessedEventsDuplicates != nil {
		metrics.ProcessedEventsDuplicates.WithLabelValues(string(consumer)).Inc()
	}
}

// sourceTopic maps each inbound consumer to the SNS topic its queue is
// subscribed to (§7.1) — the `topic` label value of
// unknown_event_acknowledged_total, so the alert names the topic whose
// producer added the type (a bounded 3-value set).
var sourceTopic = map[domain.ProcessedEventsConsumer]string{
	domain.ConsumerMembership: "iam.membership.events",
	domain.ConsumerTenant:     "iam.tenant.events",
	domain.ConsumerBilling:    "billing.events",
}

// ackUnknown is the forward-compat path (matching iam-org-membership): an
// event type with no wired handler is logged, counted, and recorded in
// processed_events so redelivery does not storm the same unknown type.
func ackUnknown(ctx context.Context, inbox port.Inbox, log port.Logger, consumer domain.ProcessedEventsConsumer, env events.Envelope[json.RawMessage]) error {
	topic := sourceTopic[consumer]
	if metrics.UnknownEventAcknowledged != nil {
		metrics.UnknownEventAcknowledged.WithLabelValues(topic, env.Type).Inc()
	}
	log.Info("unknown event type — silently acknowledging", map[string]any{
		"event_type": env.Type, "event_id": env.ID, "consumer": string(consumer), "topic": topic,
	})
	return processOnce(ctx, inbox, consumer, env, noLocalWrite)
}
