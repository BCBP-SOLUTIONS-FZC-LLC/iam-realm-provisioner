package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
)

// defaultRetentionBatch bounds one DELETE when the caller passes batch <= 0.
const defaultRetentionBatch = 5000

// RetentionRepository implements port.RetentionPruner (ledger-prune). It
// MUST be constructed with the BYPASSRLS sysPool: provisioning_operations
// is RLS-scoped and the sweep is cross-tenant, and rls_violation_log is
// readable/deletable only by the migrator role.
type RetentionRepository struct {
	pool *pgcommon.Pool
}

var _ port.RetentionPruner = (*RetentionRepository)(nil)

// NewRetentionRepository constructs a RetentionRepository over sysPool.
func NewRetentionRepository(sysPool *pgcommon.Pool) *RetentionRepository {
	return &RetentionRepository{pool: sysPool}
}

// PruneProvisioningOperations implements port.RetentionPruner.
func (r *RetentionRepository) PruneProvisioningOperations(ctx context.Context, olderThan time.Duration, batch int) (int64, error) {
	return r.pruneLoop(ctx, `
		DELETE FROM provisioning_operations
		WHERE ctid IN (
			SELECT ctid FROM provisioning_operations
			WHERE created_at < now() - make_interval(secs => $1)
			LIMIT $2)`, olderThan, batch)
}

// PruneRLSViolationLog implements port.RetentionPruner.
func (r *RetentionRepository) PruneRLSViolationLog(ctx context.Context, olderThan time.Duration, batch int) (int64, error) {
	return r.pruneLoop(ctx, `
		DELETE FROM rls_violation_log
		WHERE id IN (
			SELECT id FROM rls_violation_log
			WHERE occurred_at < now() - make_interval(secs => $1)
			LIMIT $2)`, olderThan, batch)
}

// pruneLoop runs one bounded DELETE per transaction until a batch deletes
// fewer than batch rows, so a large backlog never holds one long
// transaction or lock.
func (r *RetentionRepository) pruneLoop(ctx context.Context, sql string, olderThan time.Duration, batch int) (int64, error) {
	if olderThan <= 0 {
		return 0, fmt.Errorf("retention: olderThan must be positive, got %s", olderThan)
	}
	if batch <= 0 {
		batch = defaultRetentionBatch
	}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var n int64
		err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, sql, olderThan.Seconds(), batch)
			n = tag.RowsAffected()
			return err
		})
		total += n
		if err != nil {
			return total, err
		}
		if n < int64(batch) {
			return total, nil
		}
	}
}
