//go:build integration

// Package postgres_test — schema-corruption tests. Every column in this
// service's schema is NOT NULL (see database-schema.md), so there is no way
// to provoke a genuine Postgres-side query or scan error through ordinary
// CRUD — every write this service's own repositories perform is guaranteed
// well-typed. The only realistic way to exercise the "tx.Query/tx.Exec
// itself failed" and "a row scanned back doesn't match the Go struct"
// branches that pepper every repository (one per SELECT/UPDATE/INSERT) is to
// deliberately break the schema out from under an already-passing repository
// call, the way a lagging schema migration or a manual hotfix might in
// production.
//
// Each test here gets its own fresh container (via setupTestDB) so the
// DROP COLUMN below can never bleed into any other test's assertions.
package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTenantRealmRepository_SchemaCorruption drops tenant_realms.keycloak_shard
// (CASCADE takes chk_shard_dedicated with it) after seeding a normal row —
// every method in TenantRealmRepository references this column, so one
// corruption forces a genuine non-ErrNoRows scan error out of
// FindByTenantID, a genuine Exec failure out of Update, and a genuine Query
// failure (which also drives their outer error-wrapper branches) out of
// CountsByShard and ListExpiredTrials.
func TestTenantRealmRepository_SchemaCorruption(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()
	repo := pgadapter.NewTenantRealmRepository(appPool)
	sysRepo := pgadapter.NewTenantRealmRepository(sysPool)

	tenantID := uuid.New()
	tctx := withTenant(ctx, tenantID)
	require.NoError(t, repo.Upsert(tctx, newDedicated(tenantID, "corrupt-realm", "shard-x")))

	_, err := rawPool.Exec(ctx, `ALTER TABLE tenant_realms DROP COLUMN keycloak_shard CASCADE`)
	require.NoError(t, err)

	t.Run("FindByTenantID_GenericScanError_NotMisreportedAsNotFound", func(t *testing.T) {
		_, err := repo.FindByTenantID(tctx, tenantID)
		require.Error(t, err)
		var derr *domain.Error
		if errors.As(err, &derr) {
			assert.NotEqual(t, domain.ErrTenantRealmNotFound, derr.Code, "a genuine scan error must not be reported as the row simply not existing")
		}
	})

	t.Run("Update_ExecFailure", func(t *testing.T) {
		row := newDedicated(tenantID, "corrupt-realm", "shard-x")
		row.RecordVersion = 1
		err := repo.Update(tctx, row)
		require.Error(t, err)
		var derr *domain.Error
		assert.False(t, errors.As(err, &derr), "an Exec-level failure must not be misreported as an optimistic-lock conflict")
	})

	t.Run("CountsByShard_QueryFailure", func(t *testing.T) {
		_, err := sysRepo.CountsByShard(ctx)
		require.Error(t, err)
	})

	t.Run("ListExpiredTrials_QueryFailure", func(t *testing.T) {
		_, err := sysRepo.ListExpiredTrials(ctx, 10)
		require.Error(t, err)
	})
}

// TestTenantRealmRepository_ListSuspendedPastRetention_QueryFailure drops
// suspension_source (named only in this method's own WHERE clause, not the
// shared SELECT column list) so its tx.Query fails on its own terms,
// distinct from the whole-pool-closed connectivity case already covered.
func TestTenantRealmRepository_ListSuspendedPastRetention_QueryFailure(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()
	sysRepo := pgadapter.NewTenantRealmRepository(sysPool)

	_, err := rawPool.Exec(ctx, `ALTER TABLE tenant_realms DROP COLUMN suspension_source CASCADE`)
	require.NoError(t, err)

	_, err = sysRepo.ListSuspendedPastRetention(ctx, 30, 10)
	require.Error(t, err, "the WHERE clause names the now-missing suspension_source column")
}

// TestReconciliationRepository_SchemaCorruption drops
// realm_config_reconciliations.attempts (part of ListPending's SELECT list)
// after seeding a pending row, forcing a genuine Query failure.
func TestReconciliationRepository_SchemaCorruption(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewReconciliationRepository(appPool)

	tenantID := uuid.New()
	tctx := withTenant(ctx, tenantID)
	require.NoError(t, repo.Insert(tctx, newReconciliation(tenantID, "local_accounts_enabled")))

	_, err := rawPool.Exec(ctx, `ALTER TABLE realm_config_reconciliations DROP COLUMN attempts CASCADE`)
	require.NoError(t, err)

	_, err = repo.ListPending(tctx, 10)
	require.Error(t, err, "ListPending's SELECT explicitly names the now-missing column")
}

// TestProcessedEventsRepository_SchemaCorruption drops
// processed_events.processed_at — referenced by both CleanupExpired's WHERE
// clause and its own index — forcing a genuine Exec failure.
func TestProcessedEventsRepository_SchemaCorruption(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewProcessedEventsRepository(appPool)

	_, err := rawPool.Exec(ctx, `ALTER TABLE processed_events DROP COLUMN processed_at CASCADE`)
	require.NoError(t, err)

	_, err = repo.CleanupExpired(ctx, 24*time.Hour)
	require.Error(t, err)
}

// TestProcessedEventsRepository_IsProcessedAndPrune_SchemaCorruption drops
// processed_events.consumer — referenced by IsProcessed's WHERE clause and
// by Prune's subquery/DELETE — forcing a genuine Query/Exec failure in
// both. A separate container/test from the CleanupExpired one above since
// that one corrupts a different column (processed_at, which neither of
// these two methods' WHERE/column lists need for IsProcessed, though Prune
// does also reference it — dropping consumer alone is enough to break both
// without needing to reason about column drop ordering).
func TestProcessedEventsRepository_IsProcessedAndPrune_SchemaCorruption(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewProcessedEventsRepository(appPool)

	_, err := rawPool.Exec(ctx, `ALTER TABLE processed_events DROP COLUMN consumer CASCADE`)
	require.NoError(t, err)

	t.Run("IsProcessed_QueryFailure", func(t *testing.T) {
		_, err := repo.IsProcessed(ctx, domain.ConsumerMembership, "evt-1")
		require.Error(t, err)
	})

	t.Run("Prune_ExecFailure", func(t *testing.T) {
		_, err := repo.Prune(ctx, 90, 100)
		require.Error(t, err)
	})
}

// TestIdempotencyRepository_SchemaCorruption exercises two distinct Begin
// failure modes by corrupting provisioning_operations incrementally:
//
//  1. First, with the schema still intact, a fresh Begin succeeds and
//     inserts a real row.
//  2. `result` (only ever read by the ON-CONFLICT replay SELECT, never
//     written by the INSERT) is dropped. A second Begin on the same key
//     still inserts-conflicts correctly (RowsAffected()==0 from the PK, not
//     from the missing column), so it reaches the replay SELECT — which now
//     fails with a genuine non-ErrNoRows scan error.
//  3. `tenant_id` (referenced by the INSERT's own column list) is then also
//     dropped, so a brand-new key's very first Begin fails at the initial
//     INSERT itself.
func TestIdempotencyRepository_SchemaCorruption(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewIdempotencyRepository(appPool)

	tenantID := uuid.New()
	tctx := withTenant(ctx, tenantID)
	key1 := "corrupt-key-1:" + tenantID.String()

	_, found, err := repo.Begin(tctx, key1, tenantID, domain.OpProvisionTrial)
	require.NoError(t, err)
	require.False(t, found)

	_, err = rawPool.Exec(ctx, `ALTER TABLE provisioning_operations DROP COLUMN result CASCADE`)
	require.NoError(t, err)

	t.Run("Begin_ConflictReplay_GenericScanError", func(t *testing.T) {
		_, _, err := repo.Begin(tctx, key1, tenantID, domain.OpProvisionTrial)
		require.Error(t, err, "the conflict branch's replay SELECT still names the now-missing result column")
	})

	_, err = rawPool.Exec(ctx, `ALTER TABLE provisioning_operations DROP COLUMN tenant_id CASCADE`)
	require.NoError(t, err)

	t.Run("Begin_FreshInsert_ExecFailure", func(t *testing.T) {
		key2 := "corrupt-key-2:" + tenantID.String()
		_, _, err := repo.Begin(tctx, key2, tenantID, domain.OpProvisionTrial)
		require.Error(t, err, "the initial INSERT's own column list still names the now-missing tenant_id column")
	})
}

// TestIdpConfigRepository_SchemaCorruption drops keycloak_idp_configs.created_at
// — part of the shared idpConfigColumns SELECT list used by FindByAlias,
// ListByTenant, and ListAll, but not required by Upsert's INSERT/UPDATE
// (both rely on the column's DEFAULT now()) — so seeding still works after
// the corruption, isolating the break to the read paths.
func TestIdpConfigRepository_SchemaCorruption(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()
	repo := pgadapter.NewIdpConfigRepository(appPool)
	sysRepo := pgadapter.NewIdpConfigRepository(sysPool)

	tenantID := uuid.New()
	tctx := withTenant(ctx, tenantID)
	require.NoError(t, repo.Upsert(tctx, newIdpConfig(tenantID, "corrupt-alias")))

	_, err := rawPool.Exec(ctx, `ALTER TABLE keycloak_idp_configs DROP COLUMN created_at CASCADE`)
	require.NoError(t, err)

	t.Run("FindByAlias_GenericScanError_NotMisreportedAsNotFound", func(t *testing.T) {
		_, err := repo.FindByAlias(tctx, tenantID, "corrupt-alias")
		require.Error(t, err)
		var derr *domain.Error
		if errors.As(err, &derr) {
			assert.NotEqual(t, domain.ErrIdpConfigNotFound, derr.Code, "a genuine scan error must not be reported as the row simply not existing")
		}
	})

	t.Run("ListByTenant_QueryFailure", func(t *testing.T) {
		_, err := repo.ListByTenant(tctx, tenantID)
		require.Error(t, err)
	})

	t.Run("ListAll_QueryFailure", func(t *testing.T) {
		_, err := sysRepo.ListAll(ctx)
		require.Error(t, err)
	})
}

// TestIdpValidationRepository_SchemaCorruption drops
// keycloak_idp_config_validations.validated_at — named in both
// LatestForAlias's SELECT list and its ORDER BY — forcing a genuine
// non-ErrNoRows scan error. Insert's own INSERT list never references it
// (DEFAULT now()), so seeding still works after the corruption.
func TestIdpValidationRepository_SchemaCorruption(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewIdpValidationRepository(appPool)

	tenantID := uuid.New()
	tctx := withTenant(ctx, tenantID)
	require.NoError(t, repo.Insert(tctx, &domain.IdpConfigValidation{
		TenantID:         tenantID,
		IdpAlias:         "corrupt-alias",
		ValidatorVersion: 1,
		Outcome:          domain.ValidationOutcomePass,
		Checks:           map[string]string{"sso_region": "pass"},
		Acknowledgements: []domain.IdpValidationAck{},
	}))

	_, err := rawPool.Exec(ctx, `ALTER TABLE keycloak_idp_config_validations DROP COLUMN validated_at CASCADE`)
	require.NoError(t, err)

	_, err = repo.LatestForAlias(tctx, tenantID, "corrupt-alias")
	require.Error(t, err, "both the SELECT list and the ORDER BY name the now-missing validated_at column")
}

// TestIdpValidationRepository_Insert_SchemaCorruption drops
// keycloak_idp_config_validations.outcome — named directly in Insert's own
// INSERT column list — forcing a genuine Exec failure. A separate test
// function (rather than extending the one above) because Insert's INSERT
// list never references validated_at, so that corruption alone would never
// exercise this branch.
func TestIdpValidationRepository_Insert_SchemaCorruption(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewIdpValidationRepository(setupSysPool(t, rawPool))

	_, err := rawPool.Exec(ctx, `ALTER TABLE keycloak_idp_config_validations DROP COLUMN outcome CASCADE`)
	require.NoError(t, err)

	err = repo.Insert(ctx, &domain.IdpConfigValidation{
		TenantID:         uuid.New(),
		IdpAlias:         "corrupt-alias",
		ValidatorVersion: 1,
		Outcome:          domain.ValidationOutcomePass,
		Checks:           map[string]string{"sso_region": "pass"},
		Acknowledgements: []domain.IdpValidationAck{},
	})
	require.Error(t, err, "Insert's own INSERT column list names the now-missing outcome column")
}

// TestIdempotencyRepository_Begin_SchemaCorruption drops
// provisioning_operations.operation — named in Begin's own INSERT column
// list — forcing a genuine query failure distinct from the pgx.ErrNoRows
// case the rest of Begin's tests already exercise.
func TestIdempotencyRepository_Begin_SchemaCorruption(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewIdempotencyRepository(appPool)
	tenantID := uuid.New()
	tctx := withTenant(ctx, tenantID)

	_, err := rawPool.Exec(ctx, `ALTER TABLE provisioning_operations DROP COLUMN operation CASCADE`)
	require.NoError(t, err)

	_, _, err = repo.Begin(tctx, "corrupt-key", tenantID, domain.OpProvisionTrial)
	require.Error(t, err, "Begin's own INSERT column list names the now-missing operation column")
}

// TestReconciliationRepository_ListPending_SchemaCorruption drops
// realm_config_reconciliations.setting_key — named in ListPending's SELECT
// list — forcing tx.Query itself to fail.
func TestReconciliationRepository_ListPending_SchemaCorruption(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewReconciliationRepository(appPool)
	tenantID := uuid.New()
	tctx := withTenant(ctx, tenantID)

	_, err := rawPool.Exec(ctx, `ALTER TABLE realm_config_reconciliations DROP COLUMN setting_key CASCADE`)
	require.NoError(t, err)

	_, err = repo.ListPending(tctx, 10)
	require.Error(t, err, "ListPending's own SELECT list names the now-missing setting_key column")
}

// TestGaugeRepository_ActiveRealmsByStrategy_QueryFailure drops
// tenant_realms.status — named in groupedCounts' underlying
// activeRealmsByStrategySQL — forcing tx.Query itself to fail. Distinct
// from the existing GroupedCounts_ConnectivityFailure test, which exercises
// withPool's own connection-acquisition error rather than a query that
// reaches Postgres and fails there.
func TestGaugeRepository_ActiveRealmsByStrategy_QueryFailure(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	gauges := pgadapter.NewGaugeRepository(setupSysPool(t, rawPool))

	_, err := rawPool.Exec(ctx, `ALTER TABLE tenant_realms DROP COLUMN status CASCADE`)
	require.NoError(t, err)

	_, err = gauges.ActiveRealmsByStrategy(ctx)
	require.Error(t, err, "activeRealmsByStrategySQL names the now-missing status column")
}
