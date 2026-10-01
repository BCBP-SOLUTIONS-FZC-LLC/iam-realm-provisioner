package postgres

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
)

// ProcessedEventsRepository implements the consumer dedup ledger (§4.5,
// §7) against `processed_events`. RLS-exempt — pool must be constructed
// with no GUCProvider requirement for this table (no tenant_id column).
type ProcessedEventsRepository struct {
	pool *pgcommon.Pool
}

// NewProcessedEventsRepository constructs a ProcessedEventsRepository
// backed by pool.
func NewProcessedEventsRepository(pool *pgcommon.Pool) *ProcessedEventsRepository {
	return &ProcessedEventsRepository{pool: pool}
}

var _ port.ProcessedEventsStore = (*ProcessedEventsRepository)(nil)

// IsProcessed reports whether eventID has already been recorded as
// processed for consumer. A standalone read outside any caller transaction
// — matching iam-org-membership's IdempotencyRepository.IsProcessed.
func (r *ProcessedEventsRepository) IsProcessed(ctx context.Context, consumer domain.ProcessedEventsConsumer, eventID string) (bool, error) {
	var exists bool
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM processed_events WHERE event_id = $1 AND consumer = $2)`,
			eventID, string(consumer),
		).Scan(&exists)
	})
	if err != nil {
		return false, err
	}
	return exists, nil
}

// MarkProcessed records eventID as processed for consumer, idempotently.
//
// It goes through withPool, so when the caller is already inside a
// TxRunner.RunInTx block the dedup insert **joins that transaction** and
// commits or rolls back atomically with the projection write beside it
// (§7.1, IDEMP-2) — the same seam every other repository in this package
// uses, and the atomicity the sibling iam-org-membership consumer relies on.
// Called outside a transaction it opens its own, which is what the
// Keycloak-effect-only consumers need (see MembershipConsumer).
func (r *ProcessedEventsRepository) MarkProcessed(ctx context.Context, consumer domain.ProcessedEventsConsumer, eventID string) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return r.MarkProcessedInTx(ctx, tx, consumer, eventID)
	})
}

// MarkProcessedInTx records eventID as processed for consumer inside tx.
// It holds the single INSERT both entry points share; prefer MarkProcessed,
// which reaches it through withPool and therefore works both inside and
// outside an ambient transaction. This explicit form exists for callers that
// already hold a pgx.Tx and are not going through the port.
func (r *ProcessedEventsRepository) MarkProcessedInTx(ctx context.Context, tx pgx.Tx, consumer domain.ProcessedEventsConsumer, eventID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO processed_events (event_id, consumer)
		VALUES ($1, $2)
		ON CONFLICT (event_id, consumer) DO NOTHING`, eventID, string(consumer))
	return err
}

// CleanupExpired deletes rows older than olderThan (PROCESSED_EVENTS_TTL_DAYS,
// strictly greater than the 7-day SQS message lifetime).
func (r *ProcessedEventsRepository) CleanupExpired(ctx context.Context, olderThan time.Duration) (int64, error) {
	var n int64
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM processed_events WHERE processed_at < now() - ($1::float8 * interval '1 second')`,
			olderThan.Seconds())
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		return nil
	})
	return n, err
}

// Prune deletes up to limit processed_events rows older than ttlDays,
// matching iam-org-membership's ReconcilerStore.PruneProcessedEvents
// (subquery + LIMIT, then DELETE … WHERE (event_id, consumer) IN …).
func (r *ProcessedEventsRepository) Prune(ctx context.Context, ttlDays, limit int) (int, error) {
	var n int
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM processed_events
			WHERE (event_id, consumer) IN (
				SELECT event_id, consumer FROM processed_events
				WHERE processed_at < now() - make_interval(days => $1)
				LIMIT $2
			)`, ttlDays, limit)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		return nil
	})
	return n, err
}
