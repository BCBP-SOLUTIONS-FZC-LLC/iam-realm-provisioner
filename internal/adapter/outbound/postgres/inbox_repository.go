package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/inbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// InboxRepository implements port.Inbox and port.InboxPruner with
// platform-events' inbox.Store, one Store per consumer name over the shared
// processed_events table (§4.5) — matching iam-org-membership. The table is
// RLS-exempt, so any pool works; the claim transaction still binds the
// caller's tenant GUC (pgcommon's GUCProvider) so fn's tenant-scoped writes
// pass RLS.
type InboxRepository struct {
	pool   *pgcommon.Pool
	events port.EventPublisher
	mu     sync.Mutex
	stores map[domain.ProcessedEventsConsumer]*inbox.Store
}

var (
	_ port.Inbox       = (*InboxRepository)(nil)
	_ port.InboxPruner = (*InboxRepository)(nil)
)

// NewInboxRepository builds an InboxRepository on pool. publisher is bound
// into fn's ctx (port.WithEventPublisher) exactly as TxRunner does, so a
// consumer can enqueue events atomically with its projection; nil disables
// it.
func NewInboxRepository(pool *pgcommon.Pool, publisher port.EventPublisher) *InboxRepository {
	return &InboxRepository{pool: pool, events: publisher, stores: map[domain.ProcessedEventsConsumer]*inbox.Store{}}
}

// IsProcessed implements port.Inbox.
func (r *InboxRepository) IsProcessed(ctx context.Context, consumer domain.ProcessedEventsConsumer, eventID string) (bool, error) {
	store, err := r.store(consumer)
	if err != nil {
		return false, err
	}
	id, err := uuid.Parse(eventID)
	if err != nil {
		return false, fmt.Errorf("inbox: event id %q is not a UUID: %w", eventID, err)
	}
	ok, err := store.IsProcessed(ctx, id)
	return ok, wrapConnErr(err)
}

// ProcessOnce implements port.Inbox.
func (r *InboxRepository) ProcessOnce(ctx context.Context, consumer domain.ProcessedEventsConsumer, eventID, eventType string, fn func(txCtx context.Context) error) (bool, error) {
	store, err := r.store(consumer)
	if err != nil {
		return false, err
	}
	ran := false
	env := events.Envelope[json.RawMessage]{ID: eventID, Type: eventType}
	err = store.Process(ctx, env, func(ctx context.Context, tx pgcommon.Tx) error {
		ran = true
		txCtx := WithTx(ctx, tx)
		if r.events != nil {
			txCtx = port.WithEventPublisher(txCtx, r.events)
		}
		return fn(txCtx)
	})
	if err != nil {
		return false, wrapConnErr(err)
	}
	return !ran, nil
}

// Prune implements port.InboxPruner with inbox.Store.Prune, which loops
// batch-sized deletes until no expired row is left.
func (r *InboxRepository) Prune(ctx context.Context, consumer domain.ProcessedEventsConsumer, retention time.Duration, batch int) (int64, error) {
	store, err := r.store(consumer)
	if err != nil {
		return 0, err
	}
	n, err := store.Prune(ctx, retention, batch)
	return n, wrapConnErr(err)
}

func (r *InboxRepository) store(consumer domain.ProcessedEventsConsumer) (*inbox.Store, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.stores[consumer]; ok {
		return s, nil
	}
	s, err := inbox.NewStore(r.pool, string(consumer))
	if err != nil {
		return nil, err
	}
	r.stores[consumer] = s
	return s, nil
}
