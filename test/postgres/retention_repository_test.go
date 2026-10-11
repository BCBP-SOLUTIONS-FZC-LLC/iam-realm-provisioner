//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RetentionRepository (ledger-prune) deletes only rows past the window, on
// the BYPASSRLS pool across tenants, in batches until none is left.
func TestRetentionRepository(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()
	repo := pgadapter.NewRetentionRepository(sysPool)

	insertOp := func(age string) string {
		key := "k-" + uuid.NewString()
		_, err := rawPool.Exec(ctx, `INSERT INTO provisioning_operations (idempotency_key, tenant_id, operation, status, created_at)
			VALUES ($1, $2, 'provision_trial', 'succeeded', now() - $3::interval)`, key, uuid.New(), age)
		require.NoError(t, err)
		return key
	}
	old1, old2, old3 := insertOp("40 days"), insertOp("31 days"), insertOp("35 days")
	fresh := insertOp("1 day")

	n, err := repo.PruneProvisioningOperations(ctx, 30*24*time.Hour, 2) // batch 2 forces the loop
	require.NoError(t, err)
	assert.EqualValues(t, 3, n)
	for key, want := range map[string]bool{old1: false, old2: false, old3: false, fresh: true} {
		var exists bool
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM provisioning_operations WHERE idempotency_key = $1)`, key).Scan(&exists))
		assert.Equal(t, want, exists, key)
	}

	for _, age := range []string{"100 days", "91 days", "10 days"} {
		_, err := rawPool.Exec(ctx, `INSERT INTO rls_violation_log (table_name, violation_type, occurred_at) VALUES ('tenant_realms', 'cross_tenant_access', now() - $1::interval)`, age)
		require.NoError(t, err)
	}
	n, err = repo.PruneRLSViolationLog(ctx, 90*24*time.Hour, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)
	var left int
	require.NoError(t, rawPool.QueryRow(ctx, `SELECT count(*) FROM rls_violation_log`).Scan(&left))
	assert.Equal(t, 1, left)

	_, err = repo.PruneRLSViolationLog(ctx, 0, 10)
	require.Error(t, err, "a non-positive window would delete everything")
}

// A canceled job context stops the prune loop between batches rather than
// running to completion.
func TestRetentionRepository_CanceledContext_Stops(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	repo := pgadapter.NewRetentionRepository(setupSysPool(t, rawPool))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	n, err := repo.PruneProvisioningOperations(ctx, 24*time.Hour, 10)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, n)
}
