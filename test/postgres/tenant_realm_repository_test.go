//go:build integration

// Package postgres_test covers TenantRealmRepository (§4.2) — CRUD, the
// record_version optimistic lock, the two CHECK constraints and the
// partial-unique-index dedicated-realm-name invariant, and the two
// BYPASSRLS-pool cross-tenant methods (CountsByShard, ListExpiredTrials).
package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strPtr(s string) *string { return &s }

func newSharedTrial(tenantID uuid.UUID) *domain.TenantRealm {
	return &domain.TenantRealm{
		TenantID:  tenantID,
		RealmName: "trial",
		Strategy:  domain.RealmStrategyShared,
		Status:    domain.RealmStatusActive,
	}
}

func newDedicated(tenantID uuid.UUID, realmName, shard string) *domain.TenantRealm {
	return &domain.TenantRealm{
		TenantID:      tenantID,
		RealmName:     realmName,
		Strategy:      domain.RealmStrategyDedicated,
		KeycloakShard: strPtr(shard),
		Status:        domain.RealmStatusActive,
	}
}

func TestTenantRealmRepository(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()
	repo := pgadapter.NewTenantRealmRepository(appPool)
	sysRepo := pgadapter.NewTenantRealmRepository(sysPool)

	t.Run("UpsertInsertThenFindByTenantID_HappyPath", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		require.NoError(t, repo.Upsert(tctx, newSharedTrial(tenantID)))

		got, err := repo.FindByTenantID(tctx, tenantID)
		require.NoError(t, err)
		assert.Equal(t, tenantID, got.TenantID)
		assert.Equal(t, "trial", got.RealmName)
		assert.Equal(t, domain.RealmStrategyShared, got.Strategy)
		assert.Equal(t, domain.RealmStatusActive, got.Status)
		assert.Equal(t, int64(1), got.RecordVersion)
		assert.Nil(t, got.DeletedAt)
	})

	t.Run("Upsert_ServiceAccountColumnsRoundTrip", func(t *testing.T) {
		// §2.5/RP-17: service_account_principal_id/keycloak_client_id are
		// nil until minted, then round-trip through Upsert/Update like any
		// other column.
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)

		row := newSharedTrial(tenantID)
		require.NoError(t, repo.Upsert(tctx, row))
		got, err := repo.FindByTenantID(tctx, tenantID)
		require.NoError(t, err)
		assert.Nil(t, got.ServiceAccountPrincipalID)
		assert.Nil(t, got.ServiceAccountKeycloakClientID)

		principalID := uuid.New()
		clientID := "platform-automation-" + tenantID.String()
		row.ServiceAccountPrincipalID = &principalID
		row.ServiceAccountKeycloakClientID = &clientID
		require.NoError(t, repo.Upsert(tctx, row))

		got, err = repo.FindByTenantID(tctx, tenantID)
		require.NoError(t, err)
		require.NotNil(t, got.ServiceAccountPrincipalID)
		assert.Equal(t, principalID, *got.ServiceAccountPrincipalID)
		require.NotNil(t, got.ServiceAccountKeycloakClientID)
		assert.Equal(t, clientID, *got.ServiceAccountKeycloakClientID)

		// Update (the RP-3 conversion path) carries the columns through too.
		got.Strategy = domain.RealmStrategyDedicated
		got.RealmName = "tenant-" + tenantID.String()
		got.KeycloakShard = strPtr("kc-shard-1")
		newClientID := "platform-automation"
		got.ServiceAccountKeycloakClientID = &newClientID
		require.NoError(t, repo.Update(tctx, got))

		updated, err := repo.FindByTenantID(tctx, tenantID)
		require.NoError(t, err)
		require.NotNil(t, updated.ServiceAccountKeycloakClientID)
		assert.Equal(t, "platform-automation", *updated.ServiceAccountKeycloakClientID)
		require.NotNil(t, updated.ServiceAccountPrincipalID)
		assert.Equal(t, principalID, *updated.ServiceAccountPrincipalID, "carry-over keeps the same principal_id")
	})

	t.Run("Upsert_OnConflictUpdatesAndAlwaysBumpsRecordVersion", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		require.NoError(t, repo.Upsert(tctx, newSharedTrial(tenantID)))

		row := newSharedTrial(tenantID)
		row.Status = domain.RealmStatusDisabled
		row.DisabledAt = timePtr(time.Now().UTC())
		require.NoError(t, repo.Upsert(tctx, row))

		got, err := repo.FindByTenantID(tctx, tenantID)
		require.NoError(t, err)
		assert.Equal(t, domain.RealmStatusDisabled, got.Status)
		assert.Equal(t, int64(2), got.RecordVersion, "record_version bumps on every ON CONFLICT UPDATE")

		// A third Upsert with byte-for-byte identical field values still
		// bumps record_version — the SQL's own SET clause writes
		// `record_version = record_version + 1` unconditionally, so
		// touch_row's WHEN (OLD.* IS DISTINCT FROM NEW.*) guard never
		// actually gets a chance to skip the bump for this method: the
		// explicit SET already makes NEW differ from OLD before the
		// trigger evaluates its guard.
		require.NoError(t, repo.Upsert(tctx, row))
		got, err = repo.FindByTenantID(tctx, tenantID)
		require.NoError(t, err)
		assert.Equal(t, int64(3), got.RecordVersion, "Upsert bumps record_version even for a logically no-op replay")
	})

	t.Run("FindByTenantID_NotFound", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		_, err := repo.FindByTenantID(tctx, tenantID)
		require.Error(t, err)
		var derr *domain.Error
		require.True(t, errors.As(err, &derr))
		assert.Equal(t, domain.ErrTenantRealmNotFound, derr.Code)
	})

	t.Run("FindByTenantID_ExcludesSoftDeletedRows", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		seedTenantRealm(t, ctx, rawPool, tenantID)
		_, err := rawPool.Exec(ctx, `UPDATE tenant_realms SET deleted_at = now() WHERE tenant_id = $1`, tenantID)
		require.NoError(t, err)

		tctx := withTenant(ctx, tenantID)
		_, err = repo.FindByTenantID(tctx, tenantID)
		require.Error(t, err)
		var derr *domain.Error
		require.True(t, errors.As(err, &derr))
		assert.Equal(t, domain.ErrTenantRealmNotFound, derr.Code)
	})

	t.Run("FindByTenantID_RLSSanity_CrossTenantInvisible", func(t *testing.T) {
		t.Parallel()
		tenantA, tenantB := uuid.New(), uuid.New()
		require.NoError(t, repo.Upsert(withTenant(ctx, tenantA), newSharedTrial(tenantA)))

		_, err := repo.FindByTenantID(withTenant(ctx, tenantB), tenantA)
		require.Error(t, err)
		var derr *domain.Error
		require.True(t, errors.As(err, &derr))
		assert.Equal(t, domain.ErrTenantRealmNotFound, derr.Code, "tenant B must not be able to read tenant A's row through this method")
	})

	t.Run("Update_HappyPath_OptimisticLock", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		require.NoError(t, repo.Upsert(tctx, newSharedTrial(tenantID)))
		row, err := repo.FindByTenantID(tctx, tenantID)
		require.NoError(t, err)

		row.Status = domain.RealmStatusDisabled
		row.DisabledAt = timePtr(time.Now().UTC())
		require.NoError(t, repo.Update(tctx, row))

		got, err := repo.FindByTenantID(tctx, tenantID)
		require.NoError(t, err)
		assert.Equal(t, domain.RealmStatusDisabled, got.Status)
		assert.Equal(t, int64(2), got.RecordVersion)
		require.NotNil(t, got.DisabledAt)
	})

	t.Run("Update_StaleRecordVersionConflict", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		require.NoError(t, repo.Upsert(tctx, newSharedTrial(tenantID)))
		stale, err := repo.FindByTenantID(tctx, tenantID)
		require.NoError(t, err)

		fresh, err := repo.FindByTenantID(tctx, tenantID)
		require.NoError(t, err)
		fresh.Status = domain.RealmStatusDisabled
		require.NoError(t, repo.Update(tctx, fresh))

		stale.Status = domain.RealmStatusSuspended
		err = repo.Update(tctx, stale)
		require.Error(t, err)
		var derr *domain.Error
		require.True(t, errors.As(err, &derr))
		assert.Equal(t, domain.ErrOptimisticLockConflict, derr.Code)

		got, err := repo.FindByTenantID(tctx, tenantID)
		require.NoError(t, err)
		assert.Equal(t, domain.RealmStatusDisabled, got.Status, "the stale writer's conflicting update must not have applied")
	})

	t.Run("Update_UnknownTenant_AlsoOptimisticLockConflict", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		row := newSharedTrial(tenantID)
		row.RecordVersion = 1
		err := repo.Update(tctx, row)
		require.Error(t, err)
		var derr *domain.Error
		require.True(t, errors.As(err, &derr))
		assert.Equal(t, domain.ErrOptimisticLockConflict, derr.Code, "Update cannot distinguish a missing row from a version conflict — both are 0 rows affected")
	})

	t.Run("CheckConstraint_SharedStrategyMustBeNamedTrial", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		row := newSharedTrial(tenantID)
		row.RealmName = "not-trial"
		err := repo.Upsert(tctx, row)
		require.Error(t, err)
		var pgErr *pgconn.PgError
		require.True(t, errors.As(err, &pgErr))
		assert.Equal(t, "23514", pgErr.Code, "check_violation")
		assert.Equal(t, "chk_shared_is_trial", pgErr.ConstraintName)
	})

	t.Run("CheckConstraint_DedicatedStrategyRequiresShard", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		row := &domain.TenantRealm{
			TenantID:  tenantID,
			RealmName: "tenant-noshard",
			Strategy:  domain.RealmStrategyDedicated,
			Status:    domain.RealmStatusActive,
		}
		err := repo.Upsert(tctx, row)
		require.Error(t, err)
		var pgErr *pgconn.PgError
		require.True(t, errors.As(err, &pgErr))
		assert.Equal(t, "23514", pgErr.Code)
		assert.Equal(t, "chk_shard_dedicated", pgErr.ConstraintName)
	})

	t.Run("UniqueIndex_DedicatedRealmNameGloballyUnique", func(t *testing.T) {
		t.Parallel()
		tenantA, tenantB := uuid.New(), uuid.New()
		require.NoError(t, repo.Upsert(withTenant(ctx, tenantA), newDedicated(tenantA, "tenant-shared-name", "shard-1")))

		err := repo.Upsert(withTenant(ctx, tenantB), newDedicated(tenantB, "tenant-shared-name", "shard-1"))
		require.Error(t, err)
		var pgErr *pgconn.PgError
		require.True(t, errors.As(err, &pgErr))
		assert.Equal(t, "23505", pgErr.Code, "unique_violation")
		assert.Equal(t, "uq_tenant_realms_dedicated_name", pgErr.ConstraintName)
	})

	t.Run("UniqueIndex_IsPartial_SoftDeletedRealmNameIsReusable", func(t *testing.T) {
		t.Parallel()
		tenantA, tenantC := uuid.New(), uuid.New()
		require.NoError(t, repo.Upsert(withTenant(ctx, tenantA), newDedicated(tenantA, "tenant-reusable-name", "shard-1")))
		_, err := rawPool.Exec(ctx, `UPDATE tenant_realms SET deleted_at = now() WHERE tenant_id = $1`, tenantA)
		require.NoError(t, err)

		err = repo.Upsert(withTenant(ctx, tenantC), newDedicated(tenantC, "tenant-reusable-name", "shard-1"))
		assert.NoError(t, err, "the partial index excludes deleted_at IS NOT NULL rows, so the name must be reusable")
	})

	t.Run("CountsByShard_CrossTenantAggregation", func(t *testing.T) {
		t.Parallel()
		tA, tB, tC, tD := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		require.NoError(t, sysRepo.Upsert(ctx, newDedicated(tA, "tenant-counts-a", "shard-alpha")))
		require.NoError(t, sysRepo.Upsert(ctx, newDedicated(tB, "tenant-counts-b", "shard-alpha")))
		require.NoError(t, sysRepo.Upsert(ctx, newDedicated(tC, "tenant-counts-c", "shard-beta")))
		require.NoError(t, sysRepo.Upsert(ctx, newSharedTrial(tD))) // shared, must be excluded

		tDeleted := uuid.New()
		require.NoError(t, sysRepo.Upsert(ctx, newDedicated(tDeleted, "tenant-counts-deleted", "shard-beta")))
		_, err := rawPool.Exec(ctx, `UPDATE tenant_realms SET deleted_at = now() WHERE tenant_id = $1`, tDeleted)
		require.NoError(t, err)

		counts, err := sysRepo.CountsByShard(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, counts["shard-alpha"])
		assert.Equal(t, 1, counts["shard-beta"], "the soft-deleted dedicated row on shard-beta must not be counted")
	})

	t.Run("ListExpiredTrials_FiltersOrdersAndLimits", func(t *testing.T) {
		t.Parallel()
		now := time.Now().UTC()

		expired1 := uuid.New()
		expired2 := uuid.New()
		notYetExpired := uuid.New()
		wrongStatus := uuid.New()
		wrongStrategy := uuid.New()

		seedExpiredTrial(t, ctx, rawPool, expired1, now.Add(-2*time.Hour))
		seedExpiredTrial(t, ctx, rawPool, expired2, now.Add(-1*time.Hour))
		seedExpiredTrial(t, ctx, rawPool, notYetExpired, now.Add(1*time.Hour))

		_, err := rawPool.Exec(ctx, `
			INSERT INTO tenant_realms (tenant_id, realm_name, strategy, status, trial_ends_at)
			VALUES ($1, 'trial', 'shared', 'disabled', $2)`, wrongStatus, now.Add(-3*time.Hour))
		require.NoError(t, err)
		_, err = rawPool.Exec(ctx, `
			INSERT INTO tenant_realms (tenant_id, realm_name, strategy, keycloak_shard, status, trial_ends_at)
			VALUES ($1, 'tenant-wrong-strategy', 'dedicated', 'shard-1', 'active', $2)`, wrongStrategy, now.Add(-3*time.Hour))
		require.NoError(t, err)

		all, err := sysRepo.ListExpiredTrials(ctx, 10)
		require.NoError(t, err)
		var gotIDs []uuid.UUID
		for _, r := range all {
			gotIDs = append(gotIDs, r.TenantID)
		}
		assert.Contains(t, gotIDs, expired1)
		assert.Contains(t, gotIDs, expired2)
		assert.NotContains(t, gotIDs, notYetExpired)
		assert.NotContains(t, gotIDs, wrongStatus)
		assert.NotContains(t, gotIDs, wrongStrategy)

		limited, err := sysRepo.ListExpiredTrials(ctx, 1)
		require.NoError(t, err)
		require.Len(t, limited, 1)
		assert.Equal(t, expired1, limited[0].TenantID, "ORDER BY trial_ends_at ASC — the oldest expiry comes first")
	})

	t.Run("ListSuspendedPastRetention_FiltersOrdersAndLimits", func(t *testing.T) {
		t.Parallel()
		now := time.Now().UTC()

		pastRetention1 := uuid.New()
		pastRetention2 := uuid.New()
		withinRetention := uuid.New()
		operatorSourced := uuid.New()
		wrongStatus := uuid.New()

		seedSuspendedRealm(t, ctx, rawPool, pastRetention1, domain.SuspensionSourceBillingLapse, now.Add(-40*24*time.Hour))
		seedSuspendedRealm(t, ctx, rawPool, pastRetention2, domain.SuspensionSourceBillingLapse, now.Add(-35*24*time.Hour))
		seedSuspendedRealm(t, ctx, rawPool, withinRetention, domain.SuspensionSourceBillingLapse, now.Add(-5*24*time.Hour))
		seedSuspendedRealm(t, ctx, rawPool, operatorSourced, domain.SuspensionSourceOperator, now.Add(-40*24*time.Hour))
		seedSuspendedRealm(t, ctx, rawPool, wrongStatus, domain.SuspensionSourceBillingLapse, now.Add(-40*24*time.Hour))
		_, err := rawPool.Exec(ctx, `UPDATE tenant_realms SET status = 'active' WHERE tenant_id = $1`, wrongStatus)
		require.NoError(t, err)

		all, err := sysRepo.ListSuspendedPastRetention(ctx, 30, 10)
		require.NoError(t, err)
		var gotIDs []uuid.UUID
		for _, r := range all {
			gotIDs = append(gotIDs, r.TenantID)
		}
		assert.Contains(t, gotIDs, pastRetention1)
		assert.Contains(t, gotIDs, pastRetention2)
		assert.NotContains(t, gotIDs, withinRetention, "not yet past the retention window")
		assert.NotContains(t, gotIDs, operatorSourced, "operator suspensions must never be swept by RP-C3's billing-lapse offboard phase")
		assert.NotContains(t, gotIDs, wrongStatus, "must be status=suspended")

		limited, err := sysRepo.ListSuspendedPastRetention(ctx, 30, 1)
		require.NoError(t, err)
		require.Len(t, limited, 1)
		assert.Equal(t, pastRetention1, limited[0].TenantID, "ORDER BY disabled_at ASC — the oldest suspension comes first")
	})

	t.Run("CountsByShard_ConnectivityFailure_WrapsAsDatabaseUnavailable", func(t *testing.T) {
		t.Parallel()
		closingPool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: connStringForApp(t, rawPool)})
		require.NoError(t, err)
		closingPool.Close()

		_, err = pgadapter.NewTenantRealmRepository(closingPool).CountsByShard(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "database unavailable")
	})

	t.Run("ListExpiredTrials_ConnectivityFailure_WrapsAsDatabaseUnavailable", func(t *testing.T) {
		t.Parallel()
		closingPool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: connStringForApp(t, rawPool)})
		require.NoError(t, err)
		closingPool.Close()

		_, err = pgadapter.NewTenantRealmRepository(closingPool).ListExpiredTrials(ctx, 10)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "database unavailable")
	})

	t.Run("ListSuspendedPastRetention_ConnectivityFailure_WrapsAsDatabaseUnavailable", func(t *testing.T) {
		t.Parallel()
		closingPool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: connStringForApp(t, rawPool)})
		require.NoError(t, err)
		closingPool.Close()

		_, err = pgadapter.NewTenantRealmRepository(closingPool).ListSuspendedPastRetention(ctx, 30, 10)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "database unavailable")
	})

	t.Run("ConnectivityFailure_WrapsAsDatabaseUnavailable", func(t *testing.T) {
		t.Parallel()
		closingPool, err := pgcommon.NewPool(ctx, pgcommon.Config{
			DSN:         connStringForApp(t, rawPool),
			GUCProvider: pgcommon.GUCSetFromContext,
		})
		require.NoError(t, err)
		closingPool.Close()

		closedRepo := pgadapter.NewTenantRealmRepository(closingPool)
		_, err = closedRepo.FindByTenantID(withTenant(ctx, uuid.New()), uuid.New())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "database unavailable")
	})
}

func timePtr(t time.Time) *time.Time { return &t }

func seedExpiredTrial(t testing.TB, ctx context.Context, rawPool *dbseed.Pool, tenantID uuid.UUID, trialEndsAt time.Time) {
	t.Helper()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO tenant_realms (tenant_id, realm_name, strategy, status, trial_ends_at)
		VALUES ($1, 'trial', 'shared', 'active', $2)`, tenantID, trialEndsAt)
	require.NoError(t, err)
}

// seedSuspendedRealm inserts a dedicated realm already in status=suspended
// with the given suspension_source and disabled_at, for
// ListSuspendedPastRetention's RP-C3 offboard-phase query (§8.8).
func seedSuspendedRealm(t testing.TB, ctx context.Context, rawPool *dbseed.Pool, tenantID uuid.UUID, source domain.SuspensionSource, disabledAt time.Time) {
	t.Helper()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO tenant_realms (tenant_id, realm_name, strategy, keycloak_shard, status, suspension_source, disabled_at)
		VALUES ($1, $2, 'dedicated', 'shard-1', 'suspended', $3, $4)`,
		tenantID, "tenant-"+tenantID.String(), source, disabledAt)
	require.NoError(t, err)
}
