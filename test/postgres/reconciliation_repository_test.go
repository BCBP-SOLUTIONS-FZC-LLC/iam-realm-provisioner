//go:build integration

// Package postgres_test covers ReconciliationRepository (§4.4, RP-INV-6):
// Insert/ListPending/MarkApplied/MarkFailed against
// `realm_config_reconciliations`. ListPending carries no explicit
// tenant_id filter in its SQL — it relies entirely on RLS for scoping when
// run against the app pool, and on a BYPASSRLS pool for RP-C4's real
// cross-tenant sweep — both paths are exercised below.
package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newReconciliation(tenantID uuid.UUID, settingKey string) *domain.RealmConfigReconciliation {
	return &domain.RealmConfigReconciliation{
		TenantID:     tenantID,
		SettingKey:   settingKey,
		DesiredValue: json.RawMessage(`{"enabled":true}`),
	}
}

func TestReconciliationRepository(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()
	repo := pgadapter.NewReconciliationRepository(appPool)
	sysRepo := pgadapter.NewReconciliationRepository(sysPool)

	t.Run("Insert_ThenListPending_HappyPath", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		require.NoError(t, repo.Insert(tctx, newReconciliation(tenantID, "local_accounts_enabled")))

		pending, err := repo.ListPending(tctx, 10)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		rec := pending[0]
		assert.Equal(t, tenantID, rec.TenantID)
		assert.Equal(t, "local_accounts_enabled", rec.SettingKey)
		assert.Equal(t, domain.ReconStatusPending, rec.Status)
		assert.Equal(t, 0, rec.Attempts)
		assert.Nil(t, rec.LastError)
		assert.Nil(t, rec.AppliedAt)
		assert.JSONEq(t, `{"enabled":true}`, string(rec.DesiredValue))
	})

	t.Run("ListPending_RLSSanity_ScopedToOwnTenant", func(t *testing.T) {
		t.Parallel()
		tenantA, tenantB := uuid.New(), uuid.New()
		require.NoError(t, repo.Insert(withTenant(ctx, tenantA), newReconciliation(tenantA, "local_accounts_enabled")))
		require.NoError(t, repo.Insert(withTenant(ctx, tenantB), newReconciliation(tenantB, "local_accounts_enabled")))

		pendingA, err := repo.ListPending(withTenant(ctx, tenantA), 10)
		require.NoError(t, err)
		for _, rec := range pendingA {
			assert.Equal(t, tenantA, rec.TenantID, "tenant A's app-pool ListPending must never surface tenant B's row, even though the SQL has no explicit tenant_id filter")
		}
	})

	t.Run("ListPending_CrossTenantSweepAndLimit", func(t *testing.T) {
		// Deliberately not t.Parallel(): sysRepo.ListPending is a genuine
		// cross-tenant sweep with no tenant_id filter in its SQL at all, so
		// running it alongside sibling subtests that insert their own
		// pending rows (even under different tenants) would make the
		// oldest-first LIMIT assertion below flaky. Because this subtest
		// runs synchronously as soon as t.Run is called, it completes
		// before any parallel sibling — which pauses at its own
		// t.Parallel() call before writing anything — gets a chance to
		// insert.
		tenantA, tenantB, tenantC := uuid.New(), uuid.New(), uuid.New()
		require.NoError(t, sysRepo.Insert(ctx, newReconciliation(tenantA, "local_accounts_enabled")))
		time.Sleep(5 * time.Millisecond)
		require.NoError(t, sysRepo.Insert(ctx, newReconciliation(tenantB, "idp:acme-saml")))
		time.Sleep(5 * time.Millisecond)
		require.NoError(t, sysRepo.Insert(ctx, newReconciliation(tenantC, "local_accounts_enabled")))

		all, err := sysRepo.ListPending(ctx, 10)
		require.NoError(t, err)
		var gotTenants []uuid.UUID
		for _, rec := range all {
			gotTenants = append(gotTenants, rec.TenantID)
		}
		assert.Contains(t, gotTenants, tenantA)
		assert.Contains(t, gotTenants, tenantB)
		assert.Contains(t, gotTenants, tenantC)

		limited, err := sysRepo.ListPending(ctx, 1)
		require.NoError(t, err)
		require.Len(t, limited, 1)
		assert.Equal(t, tenantA, limited[0].TenantID, "ORDER BY created_at ASC — the oldest pending row comes first")
	})

	t.Run("MarkApplied_RemovesFromPendingSweep", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		require.NoError(t, sysRepo.Insert(ctx, newReconciliation(tenantID, "local_accounts_enabled")))
		id := reconciliationIDForTenant(t, ctx, rawPool, tenantID)

		require.NoError(t, sysRepo.MarkApplied(ctx, id))

		var status string
		var appliedAt *time.Time
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT status, applied_at FROM realm_config_reconciliations WHERE id = $1`, id).
			Scan(&status, &appliedAt))
		assert.Equal(t, "applied", status)
		require.NotNil(t, appliedAt)

		pending, err := sysRepo.ListPending(ctx, 10)
		require.NoError(t, err)
		for _, rec := range pending {
			assert.NotEqual(t, id, rec.ID)
		}
	})

	t.Run("MarkFailed_IncrementsAttemptsAndStaysPending", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		require.NoError(t, sysRepo.Insert(ctx, newReconciliation(tenantID, "local_accounts_enabled")))
		id := reconciliationIDForTenant(t, ctx, rawPool, tenantID)

		require.NoError(t, sysRepo.MarkFailed(ctx, id, "keycloak: connection refused"))
		require.NoError(t, sysRepo.MarkFailed(ctx, id, "keycloak: timeout"))

		var status, lastError string
		var attempts int
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT status, attempts, last_error FROM realm_config_reconciliations WHERE id = $1`, id).
			Scan(&status, &attempts, &lastError))
		assert.Equal(t, "pending", status, "a failed attempt stays pending for the next RP-C4 tick")
		assert.Equal(t, 2, attempts)
		assert.Equal(t, "keycloak: timeout", lastError)
	})

	t.Run("MarkApplied_CrossTenantThroughAppPoolSilentlyAffectsNothing", func(t *testing.T) {
		t.Parallel()
		tenantA, tenantB := uuid.New(), uuid.New()
		require.NoError(t, repo.Insert(withTenant(ctx, tenantA), newReconciliation(tenantA, "local_accounts_enabled")))
		id := reconciliationIDForTenant(t, ctx, rawPool, tenantA)

		// MarkApplied/MarkFailed never check RowsAffected, so an RLS-blocked
		// UPDATE against another tenant's row returns nil error while
		// leaving the row completely untouched — worth pinning down
		// explicitly since it is a silent no-op, not a surfaced conflict.
		err := repo.MarkApplied(withTenant(ctx, tenantB), id)
		assert.NoError(t, err)

		var status string
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT status FROM realm_config_reconciliations WHERE id = $1`, id).Scan(&status))
		assert.Equal(t, "pending", status, "tenant B must not be able to mark tenant A's reconciliation applied")
	})
}

func reconciliationIDForTenant(t testing.TB, ctx context.Context, rawPool *pgxpool.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, rawPool.QueryRow(ctx, `SELECT id FROM realm_config_reconciliations WHERE tenant_id = $1`, tenantID).Scan(&id))
	return id
}
