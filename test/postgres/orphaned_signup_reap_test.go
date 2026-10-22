//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/cmd/reconciler/jobs"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// OrphanedSignupReap increments a Tier-3 counter, so the package's metrics
// must be registered (cmd/reconciler does this at startup; this package
// has no TestMain).
var registerReapMetricsOnce sync.Once

func registerReapMetrics() {
	registerReapMetricsOnce.Do(func() {
		_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
			Domain: "iam", Environment: "test", ServiceName: "iam-realm-provisioner", BuildVersion: "test",
		})
		metrics.Register()
	})
}

// seedSignupRow inserts the tenant_realms row RP-1 writes (shared, active,
// no trial_ends_at), backdated to createdAt; mutate adjusts it into the
// non-orphan shapes the predicate must exclude.
func seedSignupRow(t testing.TB, ctx context.Context, rawPool *dbseed.Pool, tenantID uuid.UUID, createdAt time.Time, mutate string) {
	t.Helper()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO tenant_realms (tenant_id, realm_name, strategy, status, created_at)
		VALUES ($1, 'trial', 'shared', 'active', $2)`, tenantID, createdAt)
	require.NoError(t, err)
	if mutate != "" {
		_, err = rawPool.Exec(ctx, `UPDATE tenant_realms SET `+mutate+` WHERE tenant_id = $1`, tenantID)
		require.NoError(t, err)
	}
	// A provisioning_operations row (the RP-1 ledger entry, which can carry
	// the admin email) so the scrub is proven against real PII.
	_, err = rawPool.Exec(ctx, `
		INSERT INTO provisioning_operations (idempotency_key, tenant_id, operation, status, result)
		VALUES ($1, $2, 'provision_trial', 'succeeded', '{"admin_email":"a@example.com"}')`, "rp1:"+tenantID.String(), tenantID)
	require.NoError(t, err)
	// The owner's TermsAccepted, still undelivered: the reap must keep it.
	seedPendingTermsAudit(t, ctx, rawPool, tenantID)
}

func realmStatus(t testing.TB, ctx context.Context, rawPool *dbseed.Pool, tenantID uuid.UUID) (string, bool) {
	t.Helper()
	var status string
	var deletedAt *time.Time
	require.NoError(t, rawPool.QueryRow(ctx, `SELECT status, deleted_at FROM tenant_realms WHERE tenant_id = $1`, tenantID).Scan(&status, &deletedAt))
	return status, deletedAt != nil
}

func reapJobContext(sysPool *pgcommon.Pool, kc port.KeycloakAdmin) *jobs.Context {
	return &jobs.Context{
		SysPool:           sysPool,
		Keycloak:          kc,
		Logger:            port.NewSlogStyleLogger(noopLogger{}),
		BatchLimit:        10,
		OrphanSignupAfter: 48 * time.Hour,
	}
}

func TestOrphanedSignupReap(t *testing.T) {
	t.Parallel()
	registerReapMetrics()
	_, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour)

	t.Run("SelectsOnlyNeverStartedSharedSignupsPastTheWindow", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		orphan := uuid.New()
		excluded := map[string]uuid.UUID{}
		seedSignupRow(t, ctx, rawPool, orphan, old, "")
		for name, mutate := range map[string]string{
			"too-young":          "", // created inside the window
			"trial-started":      "trial_ends_at = now() + interval '14 days'",
			"direct-paid":        "strategy = 'dedicated', realm_name = 'tenant-' || tenant_id, keycloak_shard = 'kc-1', converted_at = now()",
			"converted-at-set":   "converted_at = now()",
			"expired-disabled":   "status = 'disabled', disabled_at = now()",
			"operator-suspended": "status = 'suspended', suspension_source = 'operator', disabled_at = now()",
			"exported":           "exported_at = now()",
			"already-deleted":    "status = 'deleted', deleted_at = now()",
			"provisioning":       "status = 'provisioning'",
		} {
			id := uuid.New()
			excluded[name] = id
			created := old
			if name == "too-young" {
				created = now.Add(-47 * time.Hour)
			}
			seedSignupRow(t, ctx, rawPool, id, created, mutate)
		}

		var deleted []uuid.UUID
		kc := &fakeJobsKeycloakAdmin{HardDeleteUsersInRealmFn: func(_ context.Context, realm string, id uuid.UUID) error {
			assert.Equal(t, "trial", realm)
			deleted = append(deleted, id)
			return nil
		}}
		before := testutil.ToFloat64(metrics.OrphanedSignupReapTotal.WithLabelValues("reaped"))

		res, err := jobs.OrphanedSignupReap(ctx, reapJobContext(sysPool, kc))
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 1, Succeeded: 1}, res)
		assert.Equal(t, []uuid.UUID{orphan}, deleted)
		assert.Equal(t, before+1, testutil.ToFloat64(metrics.OrphanedSignupReapTotal.WithLabelValues("reaped")))

		status, gone := realmStatus(t, ctx, rawPool, orphan)
		assert.Equal(t, "deleted", status)
		assert.True(t, gone)
		assertPIITablesEmpty(t, ctx, rawPool, orphan)

		for name, id := range excluded {
			var n int
			require.NoError(t, rawPool.QueryRow(ctx, `SELECT count(*) FROM provisioning_operations WHERE tenant_id = $1`, id).Scan(&n))
			assert.Equal(t, 1, n, "%s: must not be scrubbed", name)
			if name == "already-deleted" {
				continue
			}
			_, gone := realmStatus(t, ctx, rawPool, id)
			assert.False(t, gone, "%s: must not be tombstoned", name)
		}
	})

	t.Run("RaceRecheck_TrialStartedAfterSelection_Skipped", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		first, late := uuid.New(), uuid.New()
		seedSignupRow(t, ctx, rawPool, first, old.Add(-time.Hour), "") // ORDER BY created_at: reaped first
		seedSignupRow(t, ctx, rawPool, late, old, "")

		var deleted []uuid.UUID
		kc := &fakeJobsKeycloakAdmin{HardDeleteUsersInRealmFn: func(_ context.Context, _ string, id uuid.UUID) error {
			deleted = append(deleted, id)
			if id == first {
				// O&M's I-1 finally lands for `late` after both were
				// selected: its TrialStarted sets trial_ends_at.
				_, err := rawPool.Exec(ctx, `UPDATE tenant_realms SET trial_ends_at = now() + interval '14 days' WHERE tenant_id = $1`, late)
				require.NoError(t, err)
			}
			return nil
		}}
		before := testutil.ToFloat64(metrics.OrphanedSignupReapTotal.WithLabelValues("skipped"))

		res, err := jobs.OrphanedSignupReap(ctx, reapJobContext(sysPool, kc))
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 2, Succeeded: 1, Skipped: 1}, res)
		assert.Equal(t, []uuid.UUID{first}, deleted, "the FOR UPDATE re-check must stop the Keycloak delete for the started trial")
		assert.Equal(t, before+1, testutil.ToFloat64(metrics.OrphanedSignupReapTotal.WithLabelValues("skipped")))

		status, gone := realmStatus(t, ctx, rawPool, late)
		assert.Equal(t, "active", status)
		assert.False(t, gone)
	})

	t.Run("RowLockedAcrossKeycloakCall_ConcurrentWriterBlocks", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		orphan := uuid.New()
		seedSignupRow(t, ctx, rawPool, orphan, old, "")

		kc := &fakeJobsKeycloakAdmin{HardDeleteUsersInRealmFn: func(_ context.Context, _ string, id uuid.UUID) error {
			// A concurrent TrialStarted write while the reaper is mid-way
			// must wait on the row lock, not slip in between the re-check
			// and the tombstone.
			err := rawPool.WithTx(ctx, func(ctx context.Context, tx pgcommon.Tx) error {
				if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '200ms'`); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `UPDATE tenant_realms SET trial_ends_at = now() WHERE tenant_id = $1`, id)
				return err
			})
			var pgErr *pgconn.PgError
			require.True(t, errors.As(err, &pgErr), "expected a lock timeout, got %v", err)
			assert.Equal(t, "55P03", pgErr.Code)
			return nil
		}}

		res, err := jobs.OrphanedSignupReap(ctx, reapJobContext(sysPool, kc))
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 1, Succeeded: 1}, res)
		status, gone := realmStatus(t, ctx, rawPool, orphan)
		assert.Equal(t, "deleted", status)
		assert.True(t, gone)
	})

	t.Run("KeycloakFailure_DeferredRowUntouched", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		orphan := uuid.New()
		seedSignupRow(t, ctx, rawPool, orphan, old, "")
		kc := &fakeJobsKeycloakAdmin{HardDeleteUsersInRealmFn: func(context.Context, string, uuid.UUID) error {
			return assert.AnError
		}}
		before := testutil.ToFloat64(metrics.OrphanedSignupReapTotal.WithLabelValues("deferred"))

		res, err := jobs.OrphanedSignupReap(ctx, reapJobContext(sysPool, kc))
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 1, Failed: 1}, res)
		assert.Equal(t, before+1, testutil.ToFloat64(metrics.OrphanedSignupReapTotal.WithLabelValues("deferred")))

		status, gone := realmStatus(t, ctx, rawPool, orphan)
		assert.Equal(t, "active", status)
		assert.False(t, gone)
		var n int
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT count(*) FROM provisioning_operations WHERE tenant_id = $1`, orphan).Scan(&n))
		assert.Equal(t, 1, n, "the scrub rolls back with the failed reap")
	})

	t.Run("CustomWindow_And_BatchLimit_OldestFirst", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		older, newer := uuid.New(), uuid.New()
		seedSignupRow(t, ctx, rawPool, older, now.Add(-30*time.Hour), "")
		seedSignupRow(t, ctx, rawPool, newer, now.Add(-25*time.Hour), "")

		var deleted []uuid.UUID
		kc := &fakeJobsKeycloakAdmin{HardDeleteUsersInRealmFn: func(_ context.Context, _ string, id uuid.UUID) error {
			deleted = append(deleted, id)
			return nil
		}}
		jctx := reapJobContext(sysPool, kc)
		jctx.OrphanSignupAfter = 24 * time.Hour
		jctx.BatchLimit = 1

		res, err := jobs.OrphanedSignupReap(ctx, jctx)
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 1, Succeeded: 1}, res)
		assert.Equal(t, []uuid.UUID{older}, deleted)
	})

	t.Run("NoCandidates_ZeroResult", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		res, err := jobs.OrphanedSignupReap(ctx, reapJobContext(sysPool, &fakeJobsKeycloakAdmin{}))
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{}, res)
	})
}
