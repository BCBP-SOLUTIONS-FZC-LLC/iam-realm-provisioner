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

// Begin inserts an in_progress row, reclaims one left `failed` by a prior
// attempt, or returns the existing row when the key is `succeeded` or
// still genuinely `in_progress` (caller replays `succeeded`.Result, or
// returns 409 operation_in_progress for `in_progress`). found is true iff
// the caller must not proceed with the operation — a fresh insert and a
// reclaimed `failed` row both come back as found=false, since both mean
// "go ahead and do the work" (Fail's own doc comment promises exactly
// this: "caller retries with the same key, which re-executes the
// operation"). Without the reclaim, idempotency_key's PRIMARY KEY plus the
// absence of any provisioning_operations cleanup job would make a single
// transient failure permanently poison that key — a retry with the same
// key, which every 409 response explicitly instructs the caller to do,
// would 409 forever instead of ever re-executing.
//
// `in_progress` is deliberately never reclaimed here, even though it can
// go stale the same way (a hard crash before Fail/Complete ever runs):
// reclaiming it risks two concurrent executions of the same operation if
// the original caller is in fact still running, which is worse than the
// key staying stuck. That residual case needs a staleness/lease mechanism
// this method does not implement.
func (r *IdempotencyRepository) Begin(ctx context.Context, key string, tenantID uuid.UUID, operation string) (*domain.ProvisioningOperation, bool, error) {
	var out *domain.ProvisioningOperation
	var found bool
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO provisioning_operations (idempotency_key, tenant_id, operation, status)
			VALUES ($1, $2, $3, 'in_progress')
			ON CONFLICT (idempotency_key) DO UPDATE
				SET status = 'in_progress', result = NULL, created_at = now(), completed_at = NULL
				WHERE provisioning_operations.status = 'failed'
			RETURNING idempotency_key, tenant_id, operation, status, result, created_at, completed_at`,
			key, tenantID, operation)
		var op domain.ProvisioningOperation
		var result []byte
		scanErr := row.Scan(&op.IdempotencyKey, &op.TenantID, &op.Operation, &op.Status, &result, &op.CreatedAt, &op.CompletedAt)
		if scanErr == nil {
			// Fresh insert, or a reclaimed `failed` row — either way the
			// caller proceeds with the operation.
			found = false
			return nil
		}
		if !errors.Is(scanErr, pgx.ErrNoRows) {
			return scanErr
		}
		// The INSERT conflicted but the ON CONFLICT ... WHERE didn't match
		// (existing status is `succeeded` or `in_progress`), so nothing was
		// returned — fetch the existing row for the caller to inspect.
		found = true
		selRow := tx.QueryRow(ctx, `
			SELECT idempotency_key, tenant_id, operation, status, result, created_at, completed_at
			FROM provisioning_operations WHERE idempotency_key = $1`, key)
		var existing domain.ProvisioningOperation
		var existingResult []byte
		if scanErr := selRow.Scan(&existing.IdempotencyKey, &existing.TenantID, &existing.Operation, &existing.Status, &existingResult, &existing.CreatedAt, &existing.CompletedAt); scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				found = false
				return nil
			}
			return scanErr
		}
		existing.Result = json.RawMessage(existingResult)
		out = &existing
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
