package postgres

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
)

// ReconciliationRepository implements port.ReconciliationRepository against
// `realm_config_reconciliations` (§4.4, RP-INV-6).
type ReconciliationRepository struct {
	pool *pgcommon.Pool
}

// NewReconciliationRepository constructs a ReconciliationRepository
// backed by pool.
func NewReconciliationRepository(pool *pgcommon.Pool) *ReconciliationRepository {
	return &ReconciliationRepository{pool: pool}
}

// Insert records a pending realm-config reconciliation for rec.
func (r *ReconciliationRepository) Insert(ctx context.Context, rec *domain.RealmConfigReconciliation) error {
	return withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO realm_config_reconciliations (tenant_id, setting_key, desired_value, status)
			VALUES ($1, $2, $3, 'pending')`,
			rec.TenantID, rec.SettingKey, rec.DesiredValue)
		return err
	})
}

// ListPending returns up to limit pending reconciliations, oldest first.
func (r *ReconciliationRepository) ListPending(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error) {
	var out []*domain.RealmConfigReconciliation
	err := withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, setting_key, desired_value, status, attempts, last_error, created_at, applied_at
			FROM realm_config_reconciliations
			WHERE status = 'pending'
			ORDER BY created_at ASC
			LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var rec domain.RealmConfigReconciliation
			if err := rows.Scan(&rec.ID, &rec.TenantID, &rec.SettingKey, &rec.DesiredValue, &rec.Status, &rec.Attempts, &rec.LastError, &rec.CreatedAt, &rec.AppliedAt); err != nil {
				return err
			}
			out = append(out, &rec)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MarkApplied marks reconciliation id as applied.
func (r *ReconciliationRepository) MarkApplied(ctx context.Context, id uuid.UUID) error {
	return withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE realm_config_reconciliations SET status = 'applied', applied_at = now() WHERE id = $1`, id)
		return err
	})
}

// MarkFailed increments reconciliation id's attempt count and records
// lastErr, leaving it pending for retry unless the incremented attempt
// count has reached maxAttempts, in which case it moves to the terminal
// 'failed' status instead (RP-C4 poison-pill cap, §8.10). The CASE keeps
// the increment-and-maybe-terminate decision atomic in one statement,
// rather than a read-then-write that could race a concurrent tick.
func (r *ReconciliationRepository) MarkFailed(ctx context.Context, id uuid.UUID, lastErr string, maxAttempts int) (domain.ReconStatus, error) {
	var status domain.ReconStatus
	err := withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		return tx.QueryRow(ctx, `
			UPDATE realm_config_reconciliations
			SET attempts = attempts + 1,
			    last_error = $2,
			    status = (CASE WHEN attempts + 1 >= $3 THEN 'failed' ELSE 'pending' END)::recon_status
			WHERE id = $1
			RETURNING status`, id, lastErr, maxAttempts).Scan(&status)
	})
	if err != nil {
		return "", err
	}
	return status, nil
}
