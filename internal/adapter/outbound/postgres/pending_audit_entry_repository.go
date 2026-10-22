package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
)

// maxAuditErrorLen caps last_error so an arbitrarily long upstream error
// body never bloats the row.
const maxAuditErrorLen = 1024

// PendingAuditEntryRepository implements port.PendingAuditEntryRepository
// against `pending_audit_entries` (§4.4). Enqueue runs on the app pool
// inside the caller's RunInTx (RLS-bound to the tenant); ClaimDue and the
// Mark* methods are the cross-tenant drainer's and MUST be called on a
// repository constructed over the BYPASSRLS sysPool.
type PendingAuditEntryRepository struct {
	pool *pgcommon.Pool
}

var _ port.PendingAuditEntryRepository = (*PendingAuditEntryRepository)(nil)

// NewPendingAuditEntryRepository constructs a PendingAuditEntryRepository
// backed by pool.
func NewPendingAuditEntryRepository(pool *pgcommon.Pool) *PendingAuditEntryRepository {
	return &PendingAuditEntryRepository{pool: pool}
}

// Enqueue records entry as pending, due immediately.
func (r *PendingAuditEntryRepository) Enqueue(ctx context.Context, entry domain.AuditEntry) error {
	body, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("pending_audit_entries: encode entry: %w", err)
	}
	return withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO pending_audit_entries (tenant_id, entry) VALUES ($1, $2)`,
			entry.TenantID, body)
		return err
	})
}

// EnqueueOnce records entry as pending under idempotencyKey, due
// immediately. ON CONFLICT (idempotency_key) DO NOTHING makes a second
// enqueue of the same key a no-op while the first row is still queued;
// once delivered (row deleted), Audit Log dedupes the same key itself.
func (r *PendingAuditEntryRepository) EnqueueOnce(ctx context.Context, idempotencyKey string, entry domain.AuditEntry) error {
	if idempotencyKey == "" {
		return fmt.Errorf("pending_audit_entries: empty idempotency key")
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("pending_audit_entries: encode entry: %w", err)
	}
	return withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO pending_audit_entries (tenant_id, idempotency_key, entry) VALUES ($1, $2, $3)
			ON CONFLICT (idempotency_key) DO NOTHING`,
			entry.TenantID, idempotencyKey, body)
		return err
	})
}

// ClaimDue claims up to limit due pending rows, oldest-due first. The
// single UPDATE ... FOR UPDATE SKIP LOCKED statement increments attempts
// and pushes next_attempt_at out by min(2^attempts s, 5 min) atomically,
// so a concurrent replica's claim skips rows already in flight and a row
// whose delivery crashes mid-flight is simply retried once its backoff
// elapses.
func (r *PendingAuditEntryRepository) ClaimDue(ctx context.Context, limit int) ([]*domain.PendingAuditEntry, error) {
	var out []*domain.PendingAuditEntry
	err := withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE pending_audit_entries
			SET attempts = attempts + 1,
			    next_attempt_at = now() + LEAST(power(2, attempts + 1) * interval '1 second', interval '5 minutes')
			WHERE id IN (
				SELECT id FROM pending_audit_entries
				WHERE status = 'pending' AND next_attempt_at <= now()
				ORDER BY next_attempt_at
				LIMIT $1
				FOR UPDATE SKIP LOCKED)
			RETURNING id, COALESCE(idempotency_key, ''), tenant_id, entry, attempts`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p domain.PendingAuditEntry
			var body []byte
			if err := rows.Scan(&p.ID, &p.IdempotencyKey, &p.TenantID, &body, &p.Attempts); err != nil {
				return err
			}
			if err := json.Unmarshal(body, &p.Entry); err != nil {
				return fmt.Errorf("pending_audit_entries: decode entry %s: %w", p.ID, err)
			}
			out = append(out, &p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MarkDelivered deletes delivered row id.
func (r *PendingAuditEntryRepository) MarkDelivered(ctx context.Context, id uuid.UUID) error {
	return withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM pending_audit_entries WHERE id = $1`, id)
		return err
	})
}

// MarkRetry records lastErr on row id, leaving it pending.
func (r *PendingAuditEntryRepository) MarkRetry(ctx context.Context, id uuid.UUID, lastErr string) error {
	return withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE pending_audit_entries SET last_error = $2 WHERE id = $1`, id, truncate(lastErr, maxAuditErrorLen))
		return err
	})
}

// MarkRetryAfter records lastErr and overrides next_attempt_at with
// now()+delay, honoring an AL-5 Retry-After hint over the row's default
// exponential backoff. A non-positive delay means "no specific hint" and
// falls back to MarkRetry, which leaves next_attempt_at unchanged so the
// row's own exponential-backoff schedule is preserved.
func (r *PendingAuditEntryRepository) MarkRetryAfter(ctx context.Context, id uuid.UUID, lastErr string, delay time.Duration) error {
	if delay <= 0 {
		return r.MarkRetry(ctx, id, lastErr)
	}
	return withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE pending_audit_entries
			SET last_error = $2,
			    next_attempt_at = now() + make_interval(secs => $3)
			WHERE id = $1`, id, truncate(lastErr, maxAuditErrorLen), delay.Seconds())
		return err
	})
}

// MarkFailed parks row id as permanently 'failed'.
func (r *PendingAuditEntryRepository) MarkFailed(ctx context.Context, id uuid.UUID, lastErr string) error {
	return withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE pending_audit_entries SET status = 'failed', last_error = $2 WHERE id = $1`, id, truncate(lastErr, maxAuditErrorLen))
		return err
	})
}

// truncate caps s at n bytes as valid UTF-8: an upstream error body may
// carry invalid bytes, and a cut mid-rune would create some — either makes
// Postgres reject the whole write ("invalid byte sequence for encoding
// UTF8"), losing the failed status with it.
func truncate(s string, n int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
