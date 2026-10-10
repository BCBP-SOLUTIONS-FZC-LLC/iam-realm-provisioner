package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// skipDuplicate is the cheap processed_events probe every known-type
// handler runs before opening a write transaction. A hit increments
// processed_events_duplicates_total (IDEMP-4) and short-circuits.
func skipDuplicate(ctx context.Context, dedup port.ProcessedEventsStore, consumer domain.ProcessedEventsConsumer, eventID string) (bool, error) {
	processed, err := dedup.IsProcessed(ctx, consumer, eventID)
	if err != nil {
		return false, err
	}
	if processed {
		if metrics.ProcessedEventsDuplicates != nil {
			metrics.ProcessedEventsDuplicates.WithLabelValues(string(consumer)).Inc()
		}
		return true, nil
	}
	return false, nil
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

// ackUnknown is the O&M forward-compat path: an event type with no wired
// handler is logged, counted, and recorded in processed_events inside a
// RunInTx so redelivery does not storm the same unknown type. MarkProcessed
// goes through InboxStoreAdapter which delegates to platform-events
// inbox.Store for the actual SQL.
func ackUnknown(ctx context.Context, tx port.TxRunner, dedup port.ProcessedEventsStore, log port.Logger, consumer domain.ProcessedEventsConsumer, env events.Envelope[json.RawMessage]) error {
	topic := sourceTopic[consumer]
	if metrics.UnknownEventAcknowledged != nil {
		metrics.UnknownEventAcknowledged.WithLabelValues(topic, env.Type).Inc()
	}
	log.Info("unknown event type — silently acknowledging", map[string]any{
		"event_type": env.Type, "event_id": env.ID, "consumer": string(consumer), "topic": topic,
	})
	return markProcessedInTx(ctx, tx, dedup, consumer, env.ID)
}

// markProcessedInTx records eventID on the caller's TxRunner so the dedup
// insert joins withPool's ambient transaction (or opens its own when the
// handler has no local write to be atomic with).
func markProcessedInTx(ctx context.Context, tx port.TxRunner, dedup port.ProcessedEventsStore, consumer domain.ProcessedEventsConsumer, eventID string) error {
	return tx.RunInTx(ctx, func(txCtx context.Context) error {
		if err := dedup.MarkProcessed(txCtx, consumer, eventID); err != nil {
			return fmt.Errorf("mark processed: %w", err)
		}
		return nil
	})
}
