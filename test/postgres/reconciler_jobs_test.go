//go:build integration

// Package postgres_test also covers the cmd/reconciler/jobs bodies that
// need a real BYPASSRLS sysPool: TrialHardDelete (RP-C2, raw SQL via
// runInTxWithSysPool) and ProcessedEventsPrune (port.ProcessedEventsStore.Prune
// against the same sysPool). Everything else these jobs depend on (Keycloak)
// is still mocked — only the DB parts need to be real.
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/cmd/reconciler/jobs"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/test/dbseed"
)

// ── a full functional port.KeycloakAdmin fake — TrialHardDelete only
// exercises HardDeleteUsersInRealm; every other method panics if called
// (unset Fn), matching test/unit's mock convention. ─────────────────────────

type fakeJobsKeycloakAdmin struct {
	HardDeleteUsersInRealmFn func(ctx context.Context, realmName string, tenantID uuid.UUID) error
}

var _ port.KeycloakAdmin = (*fakeJobsKeycloakAdmin)(nil)

func (f *fakeJobsKeycloakAdmin) Ready(ctx context.Context) error { panic("not implemented") }
func (f *fakeJobsKeycloakAdmin) CreateRealmFromTemplate(ctx context.Context, realmName, templateVersion string) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) RegisterOIDCClients(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) DisableRealm(ctx context.Context, realmName string) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) EnableRealm(ctx context.Context, realmName string) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) ExportRealm(ctx context.Context, realmName string) ([]byte, error) {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) DeleteRealm(ctx context.Context, realmName string) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) DeleteRealmIfExists(ctx context.Context, realmName string) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) CreateTrialUser(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) CreateInvitedUser(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) FindUserByEmail(ctx context.Context, realmName, email string) (*port.ExistingUser, error) {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) MigrateUser(ctx context.Context, sourceRealm, targetRealm string, keycloakUserID uuid.UUID) (bool, error) {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) ListUsersByTenant(ctx context.Context, realmName string, tenantID uuid.UUID) ([]uuid.UUID, error) {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) DisableUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) EnableUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) HardDeleteUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) error {
	return f.HardDeleteUsersInRealmFn(ctx, realmName, tenantID)
}
func (f *fakeJobsKeycloakAdmin) DeleteUser(ctx context.Context, realmName string, keycloakUserID uuid.UUID) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) RevokeUserSessions(ctx context.Context, realmName string, keycloakUserID uuid.UUID) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) ForceLogoutRealm(ctx context.Context, realmName string) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) AssignRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) RevokeRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) HasRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) (bool, error) {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) ResetMFA(ctx context.Context, realmName string, userID uuid.UUID, requiresMFA bool) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) CreateOrUpdateIdp(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) DeleteIdp(ctx context.Context, realmName, alias string) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) ApplyRealmConfig(ctx context.Context, realmName, settingKey string, desiredValue any) error {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) CreateServiceAccountClient(ctx context.Context, realmName, clientID string) (uuid.UUID, error) {
	panic("not implemented")
}
func (f *fakeJobsKeycloakAdmin) SetClientSecret(ctx context.Context, realmName, clientID, secret string) error {
	panic("not implemented")
}

// noopLogger discards everything — these tests assert on DB state and
// mock call counts, not log output.
type noopLogger struct{}

func (noopLogger) Debug(msg string, fields map[string]any) {}
func (noopLogger) Info(msg string, fields map[string]any)  {}
func (noopLogger) Warn(msg string, fields map[string]any)  {}
func (noopLogger) Error(msg string, fields map[string]any) {}

// clearTenantRealms wipes tenant_realms so each subtest below (deliberately
// run sequentially, not in parallel — TrialHardDelete's query is a global,
// cross-tenant sweep with no per-subtest isolation column) starts from a
// known-empty table.
func clearTenantRealms(t testing.TB, ctx context.Context, rawPool *dbseed.Pool) {
	t.Helper()
	_, err := rawPool.Exec(ctx, `DELETE FROM tenant_realms`)
	require.NoError(t, err)
}

func seedTenantRealmForHardDelete(t testing.TB, ctx context.Context, rawPool *dbseed.Pool, tenantID uuid.UUID, status string, disabledAt time.Time) {
	t.Helper()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO tenant_realms (tenant_id, realm_name, strategy, status, disabled_at)
		VALUES ($1, 'trial', 'shared', $2, $3)`, tenantID, status, disabledAt)
	require.NoError(t, err)
}

func TestTrialHardDelete(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("SelectsOnlyDisabledSharedRowsPastGraceAndHardDeletes", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		candidateOld := uuid.New()
		candidateRecentDisable := uuid.New()
		notYetPastGrace := uuid.New()
		wrongStatus := uuid.New()
		alreadyDeleted := uuid.New()

		seedTenantRealmForHardDelete(t, ctx, rawPool, candidateOld, "disabled", now.Add(-40*24*time.Hour))
		seedTenantRealmForHardDelete(t, ctx, rawPool, candidateRecentDisable, "disabled", now.Add(-31*24*time.Hour))
		seedTenantRealmForHardDelete(t, ctx, rawPool, notYetPastGrace, "disabled", now.Add(-1*time.Hour))
		seedTenantRealmForHardDelete(t, ctx, rawPool, wrongStatus, "active", now.Add(-40*24*time.Hour))
		seedTenantRealmForHardDelete(t, ctx, rawPool, alreadyDeleted, "disabled", now.Add(-40*24*time.Hour))
		_, err := rawPool.Exec(ctx, `UPDATE tenant_realms SET deleted_at = now() WHERE tenant_id = $1`, alreadyDeleted)
		require.NoError(t, err)

		var deletedRealms []uuid.UUID
		kc := &fakeJobsKeycloakAdmin{
			HardDeleteUsersInRealmFn: func(ctx context.Context, realmName string, tenantID uuid.UUID) error {
				assert.Equal(t, "trial", realmName)
				deletedRealms = append(deletedRealms, tenantID)
				return nil
			},
		}
		jctx := &jobs.Context{
			SysPool:        sysPool,
			Keycloak:       kc,
			Logger:         port.NewSlogStyleLogger(noopLogger{}),
			BatchLimit:     10,
			TrialGraceDays: 30,
		}

		res, err := jobs.TrialHardDelete(ctx, jctx)
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 2, Succeeded: 2}, res)
		assert.ElementsMatch(t, []uuid.UUID{candidateOld, candidateRecentDisable}, deletedRealms)

		// The two candidates must be soft-deleted for real.
		for _, tid := range []uuid.UUID{candidateOld, candidateRecentDisable} {
			var status string
			var deletedAt *time.Time
			require.NoError(t, rawPool.QueryRow(ctx, `SELECT status, deleted_at FROM tenant_realms WHERE tenant_id = $1`, tid).Scan(&status, &deletedAt))
			assert.Equal(t, "deleted", status)
			assert.NotNil(t, deletedAt)
		}
		// The excluded rows must be untouched.
		for _, tid := range []uuid.UUID{notYetPastGrace, wrongStatus} {
			var status string
			require.NoError(t, rawPool.QueryRow(ctx, `SELECT status FROM tenant_realms WHERE tenant_id = $1`, tid).Scan(&status))
			assert.NotEqual(t, "deleted", status)
		}
	})

	t.Run("BatchLimitCapsCandidatesOldestFirst", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		older := uuid.New()
		newer := uuid.New()
		seedTenantRealmForHardDelete(t, ctx, rawPool, older, "disabled", now.Add(-60*24*time.Hour))
		seedTenantRealmForHardDelete(t, ctx, rawPool, newer, "disabled", now.Add(-35*24*time.Hour))

		var processed []uuid.UUID
		kc := &fakeJobsKeycloakAdmin{
			HardDeleteUsersInRealmFn: func(ctx context.Context, realmName string, tenantID uuid.UUID) error {
				processed = append(processed, tenantID)
				return nil
			},
		}
		jctx := &jobs.Context{
			SysPool:        sysPool,
			Keycloak:       kc,
			Logger:         port.NewSlogStyleLogger(noopLogger{}),
			BatchLimit:     1,
			TrialGraceDays: 30,
		}

		res, err := jobs.TrialHardDelete(ctx, jctx)
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 1, Succeeded: 1}, res)
		require.Len(t, processed, 1)
		assert.Equal(t, older, processed[0], "ORDER BY disabled_at ASC — the oldest disable comes first")

		var newerStatus string
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT status FROM tenant_realms WHERE tenant_id = $1`, newer).Scan(&newerStatus))
		assert.Equal(t, "disabled", newerStatus, "the row outside the batch limit must be untouched")
	})

	t.Run("KeycloakFailure_RowLeftUntouched", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		tenantID := uuid.New()
		seedTenantRealmForHardDelete(t, ctx, rawPool, tenantID, "disabled", now.Add(-45*24*time.Hour))

		kc := &fakeJobsKeycloakAdmin{
			HardDeleteUsersInRealmFn: func(ctx context.Context, realmName string, tid uuid.UUID) error {
				return assert.AnError
			},
		}
		jctx := &jobs.Context{
			SysPool:        sysPool,
			Keycloak:       kc,
			Logger:         port.NewSlogStyleLogger(noopLogger{}),
			BatchLimit:     10,
			TrialGraceDays: 30,
		}

		res, err := jobs.TrialHardDelete(ctx, jctx)
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 1, Failed: 1}, res)

		var status string
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT status FROM tenant_realms WHERE tenant_id = $1`, tenantID).Scan(&status))
		assert.Equal(t, "disabled", status, "a Keycloak failure must not soft-delete the local row")
	})

	t.Run("NoCandidates_ZeroResult", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		kc := &fakeJobsKeycloakAdmin{}
		jctx := &jobs.Context{
			SysPool:        sysPool,
			Keycloak:       kc,
			Logger:         port.NewSlogStyleLogger(noopLogger{}),
			BatchLimit:     10,
			TrialGraceDays: 30,
		}
		res, err := jobs.TrialHardDelete(ctx, jctx)
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{}, res)
	})
}

func TestProcessedEventsPrune(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()

	t.Run("DeletesOnlyRowsOlderThanTTL", func(t *testing.T) {
		oldEventA := uuid.NewString()
		oldEventB := uuid.NewString()
		recentEvent := uuid.NewString()
		insertProcessedEventAt(t, ctx, rawPool, oldEventA, "tenant", time.Now().UTC().Add(-10*24*time.Hour))
		insertProcessedEventAt(t, ctx, rawPool, oldEventB, "membership", time.Now().UTC().Add(-8*24*time.Hour))
		insertProcessedEventAt(t, ctx, rawPool, recentEvent, "billing", time.Now().UTC().Add(-1*24*time.Hour))

		jctx := &jobs.Context{
			ProcessedEvents:        pgadapter.NewProcessedEventsRepository(sysPool),
			Logger:                 port.NewSlogStyleLogger(noopLogger{}),
			ProcessedEventsTTLDays: 7,
			BatchLimit:             500,
		}

		res, err := jobs.ProcessedEventsPrune(ctx, jctx)
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 2, Succeeded: 2}, res)

		assertProcessedEventGone(t, ctx, rawPool, oldEventA, "tenant")
		assertProcessedEventGone(t, ctx, rawPool, oldEventB, "membership")
		assertProcessedEventPresent(t, ctx, rawPool, recentEvent, "billing")
	})

	t.Run("NoRowsPastTTL_ZeroResult", func(t *testing.T) {
		recentEvent := uuid.NewString()
		insertProcessedEventAt(t, ctx, rawPool, recentEvent, "tenant", time.Now().UTC())

		jctx := &jobs.Context{
			ProcessedEvents:        pgadapter.NewProcessedEventsRepository(sysPool),
			Logger:                 port.NewSlogStyleLogger(noopLogger{}),
			ProcessedEventsTTLDays: 7,
			BatchLimit:             500,
		}

		res, err := jobs.ProcessedEventsPrune(ctx, jctx)
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{}, res)
		assertProcessedEventPresent(t, ctx, rawPool, recentEvent, "tenant")
	})
}

func insertProcessedEventAt(t testing.TB, ctx context.Context, rawPool *dbseed.Pool, eventID, consumer string, processedAt time.Time) {
	t.Helper()
	_, err := rawPool.Exec(ctx, `INSERT INTO processed_events (event_id, consumer, processed_at) VALUES ($1, $2, $3)`, eventID, consumer, processedAt)
	require.NoError(t, err)
}

func assertProcessedEventGone(t testing.TB, ctx context.Context, rawPool *dbseed.Pool, eventID, consumer string) {
	t.Helper()
	var count int
	require.NoError(t, rawPool.QueryRow(ctx, `SELECT count(*) FROM processed_events WHERE event_id = $1 AND consumer = $2`, eventID, consumer).Scan(&count))
	assert.Equal(t, 0, count)
}

func assertProcessedEventPresent(t testing.TB, ctx context.Context, rawPool *dbseed.Pool, eventID, consumer string) {
	t.Helper()
	var count int
	require.NoError(t, rawPool.QueryRow(ctx, `SELECT count(*) FROM processed_events WHERE event_id = $1 AND consumer = $2`, eventID, consumer).Scan(&count))
	assert.Equal(t, 1, count)
}
