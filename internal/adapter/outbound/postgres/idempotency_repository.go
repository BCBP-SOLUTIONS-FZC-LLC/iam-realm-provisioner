package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// IdempotencyRepository implements port.IdempotencyStore against
// `provisioning_operations` (§4.4, §9.2).
type IdempotencyRepository struct {
	pool *pgcommon.Pool
}

// NewIdempotencyRepository constructs an IdempotencyRepository backed by
// pool.
func NewIdempotencyRepository(pool *pgcommon.Pool) *IdempotencyRepository {
	return &IdempotencyRepository{pool: pool}
}

// Begin inserts an in_progress row, or returns the existing row when the
// key was already seen (caller replays `succeeded`.Result, or returns 409
// operation_in_progress for `in_progress`). found is true iff a row for
// key already existed before this call (detected via the INSERT ... ON
// CONFLICT DO NOTHING RowsAffected count).
func (r *IdempotencyRepository) Begin(ctx context.Context, key string, tenantID uuid.UUID, operation string) (*domain.ProvisioningOperation, bool, error) {
	var out *domain.ProvisioningOperation
	var found bool
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO provisioning_operations (idempotency_key, tenant_id, operation, status)
			VALUES ($1, $2, $3, 'in_progress')
			ON CONFLICT (idempotency_key) DO NOTHING`, key, tenantID, operation)
		if err != nil {
			return err
		}
		found = tag.RowsAffected() == 0
		if !found {
			return nil // fresh insert — caller proceeds, nothing to replay
		}
		row := tx.QueryRow(ctx, `
			SELECT idempotency_key, tenant_id, operation, status, result, created_at, completed_at
			FROM provisioning_operations WHERE idempotency_key = $1`, key)
		var op domain.ProvisioningOperation
		var result []byte
		if scanErr := row.Scan(&op.IdempotencyKey, &op.TenantID, &op.Operation, &op.Status, &result, &op.CreatedAt, &op.CompletedAt); scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				found = false
				return nil
			}
			return scanErr
		}
		op.Result = json.RawMessage(result)
		out = &op
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, found, nil
}

// Complete marks key succeeded with result (the response body to replay on
// a duplicate call, §9.2).
func (r *IdempotencyRepository) Complete(ctx context.Context, key string, result json.RawMessage) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE provisioning_operations SET status = 'succeeded', result = $2, completed_at = now()
			WHERE idempotency_key = $1`, key, result)
		return err
	})
}

// Fail marks key failed (no result to replay — caller retries with the
// same key, which re-executes the operation since it never reached
// succeeded, §9.2).
func (r *IdempotencyRepository) Fail(ctx context.Context, key string) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE provisioning_operations SET status = 'failed', completed_at = now()
			WHERE idempotency_key = $1`, key)
		return err
	})
}
