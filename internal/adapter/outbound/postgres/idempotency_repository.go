package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
)

// DefaultInProgressLease is how long an `in_progress` row is trusted to
// belong to a live execution. After it, Begin reclaims the row (a crashed
// pod, a SIGKILL, or a Fail that could not be written). It must exceed the
// longest operation this service runs — RP-3 conversion's detached budget
// (CONVERT_TIMEOUT, 10m default) — or a still-running conversion could be
// executed a second time concurrently.
const DefaultInProgressLease = 15 * time.Minute

// ledgerWriteTimeout bounds Complete/Fail, which run detached from the
// caller's context (see detachedLedgerContext).
const ledgerWriteTimeout = 10 * time.Second

// IdempotencyRepository implements port.IdempotencyStore against
// `provisioning_operations` (§4.4, §9.2).
type IdempotencyRepository struct {
	pool  *pgcommon.Pool
	lease time.Duration
}

// NewIdempotencyRepository constructs an IdempotencyRepository backed by
// pool, with DefaultInProgressLease.
func NewIdempotencyRepository(pool *pgcommon.Pool) *IdempotencyRepository {
	return &IdempotencyRepository{pool: pool, lease: DefaultInProgressLease}
}

// WithInProgressLease overrides the in_progress lease (IDEMPOTENCY_IN_PROGRESS_LEASE)
// and returns r. Non-positive values are ignored.
func (r *IdempotencyRepository) WithInProgressLease(d time.Duration) *IdempotencyRepository {
	if d > 0 {
		r.lease = d
	}
	return r
}

// detachedLedgerContext returns a context that keeps ctx's values (the
// joined transaction, the RLS GUC set, the trace) but not its cancellation,
// bounded by ledgerWriteTimeout. Complete and Fail run on it: they are
// called exactly when the request context is most likely to be done — the
// 30s request deadline fired, or the client disconnected — and a ledger
// write that fails then leaves the key `in_progress`, which every retry
// would answer with 409 operation_in_progress until the lease expires.
func detachedLedgerContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), ledgerWriteTimeout)
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
// An `in_progress` row is reclaimed too once it is older than the lease
// (r.lease, DefaultInProgressLease): by then the execution that claimed it
// is gone (a hard crash, a SIGKILL during shutdown) or could not record
// its outcome. Within the lease it is never reclaimed — two concurrent
// executions of the same operation are worse than a short 409 window.
// Every operation behind this ledger is a Keycloak-effect-first reconcile
// (an already-applied step replays as a no-op), so re-running a reclaimed
// key converges rather than duplicating.
func (r *IdempotencyRepository) Begin(ctx context.Context, key string, tenantID uuid.UUID, operation string) (*domain.ProvisioningOperation, bool, error) {
	var out *domain.ProvisioningOperation
	var found bool
	err := withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO provisioning_operations (idempotency_key, tenant_id, operation, status)
			VALUES ($1, $2, $3, 'in_progress')
			ON CONFLICT (idempotency_key) DO UPDATE
				SET status = 'in_progress', result = NULL, created_at = now(), completed_at = NULL
				WHERE provisioning_operations.status = 'failed'
				   OR (provisioning_operations.status = 'in_progress'
				       AND provisioning_operations.created_at < now() - make_interval(secs => $4))
			RETURNING idempotency_key, tenant_id, operation, status, result, created_at, completed_at`,
			key, tenantID, operation, r.lease.Seconds())
		var op domain.ProvisioningOperation
		var result []byte
		scanErr := row.Scan(&op.IdempotencyKey, &op.TenantID, &op.Operation, &op.Status, &result, &op.CreatedAt, &op.CompletedAt)
		if scanErr == nil {
			// Fresh insert, or a reclaimed `failed` row — either way the
			// caller proceeds with the operation.
			found = false
			return nil
		}
		if !errors.Is(scanErr, pgcommon.ErrNoRows) {
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
			if errors.Is(scanErr, pgcommon.ErrNoRows) {
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
// a duplicate call, §9.2). Runs detached from ctx's cancellation
// (detachedLedgerContext): the operation already succeeded.
func (r *IdempotencyRepository) Complete(ctx context.Context, key string, result json.RawMessage) error {
	ctx, cancel := detachedLedgerContext(ctx)
	defer cancel()
	return withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE provisioning_operations SET status = 'succeeded', result = $2, completed_at = now()
			WHERE idempotency_key = $1`, key, result)
		return err
	})
}

// Fail marks key failed (no result to replay — caller retries with the
// same key, which re-executes the operation since it never reached
// succeeded, §9.2). Runs detached from ctx's cancellation
// (detachedLedgerContext): a timed-out or abandoned request is exactly when
// this must still land.
func (r *IdempotencyRepository) Fail(ctx context.Context, key string) error {
	ctx, cancel := detachedLedgerContext(ctx)
	defer cancel()
	return withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE provisioning_operations SET status = 'failed', completed_at = now()
			WHERE idempotency_key = $1`, key)
		return err
	})
}
