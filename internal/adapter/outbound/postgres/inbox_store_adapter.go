package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/inbox"
)

// InboxStoreAdapter implements port.ProcessedEventsStore via three
// platform-events inbox.Store instances — one per inbound consumer
// (membership, tenant, billing). All processed_events SQL is owned by
// inbox.Store; this adapter routes by consumer name, parses string event
// IDs to uuid.UUID, and preserves IDEMP-2 atomicity for callers already
// inside TxRunner.RunInTx by executing the INSERT on their ambient pgx.Tx
// (detected via TxFromContext) rather than opening a second transaction.
type InboxStoreAdapter struct {
	stores map[string]*inbox.Store
}

var _ port.ProcessedEventsStore = (*InboxStoreAdapter)(nil)

// NewInboxStoreAdapter returns a ProcessedEventsStore backed by one
// inbox.Store per consumer. Use the app pool (GUCProvider set) for consumer
// write paths; use the sysPool (BYPASSRLS) for the reconciler prune job.
func NewInboxStoreAdapter(membership, tenant, billing *inbox.Store) *InboxStoreAdapter {
	return &InboxStoreAdapter{stores: map[string]*inbox.Store{
		string(domain.ConsumerMembership): membership,
		string(domain.ConsumerTenant):     tenant,
		string(domain.ConsumerBilling):    billing,
	}}
}

func (a *InboxStoreAdapter) storeFor(consumer domain.ProcessedEventsConsumer) (*inbox.Store, error) {
	s, ok := a.stores[string(consumer)]
	if !ok {
		return nil, fmt.Errorf("inbox: no store registered for consumer %q", consumer)
	}
	return s, nil
}

// IsProcessed reports whether eventID has already been recorded for consumer.
// Delegates to the per-consumer inbox.Store, which opens its own transaction.
// Always called outside any write transaction so no ambient-tx check is needed.
func (a *InboxStoreAdapter) IsProcessed(ctx context.Context, consumer domain.ProcessedEventsConsumer, eventID string) (bool, error) {
	s, err := a.storeFor(consumer)
	if err != nil {
		return false, err
	}
	id, err := uuid.Parse(eventID)
	if err != nil {
		return false, fmt.Errorf("inbox: parse event ID %q: %w", eventID, err)
	}
	return s.IsProcessed(ctx, id)
}

// MarkProcessed records eventID as processed for consumer, idempotently.
//
// When ctx holds an ambient transaction from TxRunner.RunInTx (TxFromContext),
// the INSERT runs on that transaction so the dedup mark commits atomically
// with the caller's projection write (IDEMP-2 — same guarantee the old
// ProcessedEventsRepository provided). Outside a transaction, control passes
// to inbox.Store.MarkProcessed which opens its own transaction.
func (a *InboxStoreAdapter) MarkProcessed(ctx context.Context, consumer domain.ProcessedEventsConsumer, eventID string) error {
	s, err := a.storeFor(consumer)
	if err != nil {
		return err
	}
	id, err := uuid.Parse(eventID)
	if err != nil {
		return fmt.Errorf("inbox: parse event ID %q: %w", eventID, err)
	}
	if tx, ok := TxFromContext(ctx); ok {
		// Execute on the ambient transaction so the dedup INSERT commits
		// atomically with the caller's projection write (IDEMP-2). This SQL
		// is intentionally identical to inbox.Store.MarkProcessed's internal
		// INSERT — inbox.Store.MarkProcessed always opens its own transaction
		// and cannot join an existing one. If the platform-events inbox schema
		// ever adds a NOT NULL column to processed_events, this copy MUST be
		// updated in tandem; a test in test/postgres/processed_events_store_test.go
		// verifies both paths write equivalent rows.
		_, execErr := tx.Exec(ctx,
			`INSERT INTO processed_events (event_id, consumer) VALUES ($1, $2) ON CONFLICT (event_id, consumer) DO NOTHING`,
			id.String(), string(consumer))
		return execErr
	}
	return s.MarkProcessed(ctx, id)
}

// Prune deletes rows older than ttlDays across all three consumers, in
// batches of at most limit rows per consumer per call. Returns the total
// rows deleted. Callers are the reconciler's processed-events-prune job.
func (a *InboxStoreAdapter) Prune(ctx context.Context, ttlDays, limit int) (int, error) {
	retention := time.Duration(ttlDays) * 24 * time.Hour
	var total int
	for _, s := range a.stores {
		n, err := s.Prune(ctx, retention, limit)
		if err != nil {
			return total, err
		}
		total += int(n)
	}
	return total, nil
}
