package postgres

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

// IsProcessed reports whether eventID has already been recorded as
// processed for consumer.
func (r *ProcessedEventsRepository) IsProcessed(ctx context.Context, consumer domain.ProcessedEventsConsumer, eventID string) (bool, error) {
	var exists bool
	err := r.pool.WithConn(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		return conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM processed_events WHERE event_id = $1 AND consumer = $2)`,
			eventID, string(consumer),
		).Scan(&exists)
	})
	return exists, err
}

// MarkProcessed records eventID as processed for consumer, idempotently.
func (r *ProcessedEventsRepository) MarkProcessed(ctx context.Context, consumer domain.ProcessedEventsConsumer, eventID string) error {
	return r.pool.WithConn(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `
			INSERT INTO processed_events (event_id, consumer, processed_at)
			VALUES ($1, $2, now())
			ON CONFLICT (event_id, consumer) DO NOTHING`,
			eventID, string(consumer))
		return err
	})
}

// MarkProcessedInTx joins the caller's active transaction so the dedup
// write commits atomically with a projection update, when the consumer's
// business logic opens exactly one tx.
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
	err := r.pool.WithConn(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx,
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
