//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/cmd/reconciler/jobs"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/test/dbseed"
)

// fakeJobsTokenService is a port.TokenServiceClient whose only live method
// is RevokeAllCredentials (the orphaned-signup-reap teardown, §8.7); every
// other method panics so an unexpected call fails loudly.
type fakeJobsTokenService struct {
	RevokeAllCredentialsFn func(ctx context.Context, tenantID, principalID uuid.UUID) error
	revoked                []uuid.UUID
}

var _ port.TokenServiceClient = (*fakeJobsTokenService)(nil)

func (f *fakeJobsTokenService) RegisterPrincipal(context.Context, uuid.UUID, port.RegisterPrincipalRequest) (*port.RegisterPrincipalResult, error) {
	panic("not implemented")
}
func (f *fakeJobsTokenService) IssueOrRotateCredential(context.Context, uuid.UUID, uuid.UUID, port.IssueCredentialRequest) (*port.IssueCredentialResult, error) {
	panic("not implemented")
}
func (f *fakeJobsTokenService) RevokeCredential(context.Context, uuid.UUID, uuid.UUID) error {
	panic("not implemented")
}
func (f *fakeJobsTokenService) RevokeAllCredentials(ctx context.Context, tenantID, principalID uuid.UUID) error {
	f.revoked = append(f.revoked, principalID)
	if f.RevokeAllCredentialsFn != nil {
		return f.RevokeAllCredentialsFn(ctx, tenantID, principalID)
	}
	return nil
}
func (f *fakeJobsTokenService) ReadPrincipal(context.Context, uuid.UUID, uuid.UUID) (*port.ReadPrincipalResult, error) {
	panic("not implemented")
}
func (f *fakeJobsTokenService) JWKSURLFor(uuid.UUID) string { panic("not implemented") }

func countPII(t testing.TB, ctx context.Context, rawPool *dbseed.Pool, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, rawPool.QueryRow(ctx, `SELECT count(*) FROM provisioning_operations WHERE tenant_id = $1`, tenantID).Scan(&n))
	return n
}

// withAutomation is a seedSignupRow mutation recording RP-1's minted
// platform-automation principal (§2.5).
func withAutomation(principalID uuid.UUID) string {
	return "service_account_principal_id = '" + principalID.String() + "', service_account_keycloak_client_id = 'platform-automation-' || tenant_id"
}

func TestTrialHardDelete_AutomationClientTeardown(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()
	disabledAt := time.Now().UTC().Add(-40 * 24 * time.Hour)

	hardDeleteCtx := func(kc *fakeJobsKeycloakAdmin) *jobs.Context {
		return &jobs.Context{SysPool: sysPool, Keycloak: kc, Logger: port.NewSlogStyleLogger(noopLogger{}), BatchLimit: 10, TrialGraceDays: 30}
	}

	t.Run("DeletesTheRecordedClientOrTheDerivedName", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		recorded, unrecorded := uuid.New(), uuid.New()
		seedTenantRealmForHardDelete(t, ctx, rawPool, recorded, "disabled", disabledAt)
		_, err := rawPool.Exec(ctx, `UPDATE tenant_realms SET service_account_principal_id = gen_random_uuid(), service_account_keycloak_client_id = 'recorded-client' WHERE tenant_id = $1`, recorded)
		require.NoError(t, err)
		seedTenantRealmForHardDelete(t, ctx, rawPool, unrecorded, "disabled", disabledAt)

		kc := &fakeJobsKeycloakAdmin{HardDeleteUsersInRealmFn: func(context.Context, string, uuid.UUID) error { return nil }}
		res, err := jobs.TrialHardDelete(ctx, hardDeleteCtx(kc))
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 2, Succeeded: 2}, res)
		assert.ElementsMatch(t, []string{
			"trial/recorded-client",
			"trial/" + domain.SharedPlatformAutomationClientID(unrecorded),
		}, kc.deletedClients)
	})

	t.Run("ClientDeleteFailure_RowStaysDisabledForTheNextRun", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		tenantID := uuid.New()
		seedTenantRealmForHardDelete(t, ctx, rawPool, tenantID, "disabled", disabledAt)
		kc := &fakeJobsKeycloakAdmin{
			HardDeleteUsersInRealmFn:             func(context.Context, string, uuid.UUID) error { return nil },
			DeleteServiceAccountClientIfExistsFn: func(context.Context, string, string) error { return assert.AnError },
		}

		res, err := jobs.TrialHardDelete(ctx, hardDeleteCtx(kc))
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 1, Failed: 1}, res)
		status, gone := realmStatus(t, ctx, rawPool, tenantID)
		assert.Equal(t, "disabled", status)
		assert.False(t, gone, "the row must stay selectable so the next run retries the whole teardown")
	})
}

func TestOrphanedSignupReap_AutomationTeardown(t *testing.T) {
	t.Parallel()
	registerReapMetrics()
	_, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()
	old := time.Now().UTC().Add(-72 * time.Hour)

	reapCtx := func(kc *fakeJobsKeycloakAdmin, ts port.TokenServiceClient) *jobs.Context {
		jctx := reapJobContext(sysPool, kc)
		jctx.TokenService = ts
		return jctx
	}
	usersOK := func(context.Context, string, uuid.UUID) error { return nil }

	t.Run("DeletesClientAndRevokesCredentials", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		tenantID, principalID := uuid.New(), uuid.New()
		seedSignupRow(t, ctx, rawPool, tenantID, old, withAutomation(principalID))
		var order []string
		kc := &fakeJobsKeycloakAdmin{
			HardDeleteUsersInRealmFn: func(context.Context, string, uuid.UUID) error { order = append(order, "users"); return nil },
			DeleteServiceAccountClientIfExistsFn: func(context.Context, string, string) error {
				order = append(order, "client")
				return nil
			},
		}
		ts := &fakeJobsTokenService{RevokeAllCredentialsFn: func(_ context.Context, tid, _ uuid.UUID) error {
			assert.Equal(t, tenantID, tid)
			order = append(order, "credentials")
			return nil
		}}

		res, err := jobs.OrphanedSignupReap(ctx, reapCtx(kc, ts))
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 1, Succeeded: 1}, res)
		assert.Equal(t, []string{"users", "client", "credentials"}, order)
		assert.Equal(t, []string{"trial/" + domain.SharedPlatformAutomationClientID(tenantID)}, kc.deletedClients)
		assert.Equal(t, []uuid.UUID{principalID}, ts.revoked)
		status, gone := realmStatus(t, ctx, rawPool, tenantID)
		assert.Equal(t, "deleted", status)
		assert.True(t, gone)
		assertPIITablesEmpty(t, ctx, rawPool, tenantID)
	})

	t.Run("TokenServiceFailure_RollsBack_ThenConvergesOnRetry", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		tenantID, principalID := uuid.New(), uuid.New()
		seedSignupRow(t, ctx, rawPool, tenantID, old, withAutomation(principalID))
		kc := &fakeJobsKeycloakAdmin{HardDeleteUsersInRealmFn: usersOK}
		ts := &fakeJobsTokenService{RevokeAllCredentialsFn: func(context.Context, uuid.UUID, uuid.UUID) error {
			return domain.NewError(domain.ErrTokenServiceUnavailable, "down")
		}}

		res, err := jobs.OrphanedSignupReap(ctx, reapCtx(kc, ts))
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 1, Failed: 1}, res)
		status, gone := realmStatus(t, ctx, rawPool, tenantID)
		assert.Equal(t, "active", status, "a failed step rolls the whole tenant back")
		assert.False(t, gone)
		assert.Equal(t, 1, countPII(t, ctx, rawPool, tenantID), "the scrub rolled back too")

		// Next run: Keycloak's users and client are already gone (the
		// adapters treat that as success), Token Service is back.
		ts.RevokeAllCredentialsFn = nil
		res, err = jobs.OrphanedSignupReap(ctx, reapCtx(kc, ts))
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 1, Succeeded: 1}, res)
		assert.Len(t, kc.deletedClients, 2, "the idempotent client delete is simply repeated")
		_, gone = realmStatus(t, ctx, rawPool, tenantID)
		assert.True(t, gone)
	})

	t.Run("ClientDeleteFailure_RollsBack_TokenServiceNotCalled", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		tenantID := uuid.New()
		seedSignupRow(t, ctx, rawPool, tenantID, old, withAutomation(uuid.New()))
		kc := &fakeJobsKeycloakAdmin{
			HardDeleteUsersInRealmFn:             usersOK,
			DeleteServiceAccountClientIfExistsFn: func(context.Context, string, string) error { return assert.AnError },
		}
		ts := &fakeJobsTokenService{}

		res, err := jobs.OrphanedSignupReap(ctx, reapCtx(kc, ts))
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 1, Failed: 1}, res)
		assert.Empty(t, ts.revoked)
		_, gone := realmStatus(t, ctx, rawPool, tenantID)
		assert.False(t, gone)
	})

	t.Run("NoTokenServiceWired_TenantWithPrincipalIsDeferred", func(t *testing.T) {
		clearTenantRealms(t, ctx, rawPool)
		withPrincipal, without := uuid.New(), uuid.New()
		seedSignupRow(t, ctx, rawPool, withPrincipal, old, withAutomation(uuid.New()))
		seedSignupRow(t, ctx, rawPool, without, old.Add(time.Minute), "")
		kc := &fakeJobsKeycloakAdmin{HardDeleteUsersInRealmFn: usersOK}

		res, err := jobs.OrphanedSignupReap(ctx, reapCtx(kc, nil))
		require.NoError(t, err)
		assert.Equal(t, jobs.Result{Attempted: 2, Succeeded: 1, Failed: 1}, res)
		_, gone := realmStatus(t, ctx, rawPool, withPrincipal)
		assert.False(t, gone, "a principal that cannot be revoked must not be abandoned")
		_, gone = realmStatus(t, ctx, rawPool, without)
		assert.True(t, gone, "no principal was ever registered, so there is nothing to revoke")
	})
}

// The repository must never revive a tombstone, whatever the caller does
// (LLD v1.38): Upsert answers ErrTenantOffboarded and leaves the row alone,
// and Update treats it as a lost optimistic lock.
func TestTenantRealmRepository_TombstoneIsNeverResurrected(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewTenantRealmRepository(appPool)

	tenantID := uuid.New()
	tctx := withTenant(ctx, tenantID)
	require.NoError(t, repo.Upsert(tctx, newSharedTrial(tenantID)))
	stale, err := repo.FindByTenantID(tctx, tenantID)
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx, `UPDATE tenant_realms SET status = 'deleted', deleted_at = now() WHERE tenant_id = $1`, tenantID)
	require.NoError(t, err)
	var version int64
	require.NoError(t, rawPool.QueryRow(ctx, `SELECT record_version FROM tenant_realms WHERE tenant_id = $1`, tenantID).Scan(&version))

	err = repo.Upsert(tctx, newSharedTrial(tenantID))
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrTenantOffboarded, de.Code)

	stale.RecordVersion = version // even a current record_version must not write through
	stale.Status = domain.RealmStatusActive
	err = repo.Update(tctx, stale)
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrOptimisticLockConflict, de.Code)

	status, gone := realmStatus(t, ctx, rawPool, tenantID)
	assert.Equal(t, "deleted", status)
	assert.True(t, gone)
}

// RP-1 and RP-2 refuse a tenant_id whose row was tombstoned, with the
// existing 409 TENANT_OFFBOARDED, before any Keycloak call (§8.1/§8.2, LLD
// v1.38). Keycloak and Token Service are fakes that panic if reached.
func TestProvisioning_RejectsTombstonedTenant(t *testing.T) {
	t.Parallel()
	registerReapMetrics() // RP-1/RP-2 count ProvisionTotal
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	realms := pgadapter.NewTenantRealmRepository(appPool)
	idem := pgadapter.NewIdempotencyRepository(appPool)
	svc := service.NewProvisioningService(realms, idem, &fakeJobsKeycloakAdmin{}, &fakeJobsTokenService{},
		pgadapter.NewTxRunner(appPool, nil), "trial", nil)

	tombstone := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.New()
		seedSignupRow(t, ctx, rawPool, id, time.Now().UTC().Add(-72*time.Hour), "status = 'deleted', deleted_at = now()")
		// The scrub deleted the RP-1 ledger row, so the original key is
		// not a replay.
		_, err := rawPool.Exec(ctx, `DELETE FROM provisioning_operations WHERE tenant_id = $1`, id)
		require.NoError(t, err)
		return id
	}
	assertRejected := func(t *testing.T, tenantID uuid.UUID, err error) {
		t.Helper()
		var de *domain.Error
		require.True(t, errors.As(err, &de), "got %v", err)
		assert.Equal(t, domain.ErrTenantOffboarded, de.Code)
		status, gone := realmStatus(t, ctx, rawPool, tenantID)
		assert.Equal(t, "deleted", status)
		assert.True(t, gone, "the tombstone is untouched")
	}

	t.Run("RP1", func(t *testing.T) {
		tenantID := tombstone(t)
		_, err := svc.ProvisionTrial(withTenant(ctx, tenantID), "rp1-"+tenantID.String(), service.ProvisionTrialRequest{
			TenantID: tenantID, TenantName: "Acme", TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A", Plan: "starter",
		})
		assertRejected(t, tenantID, err)
	})

	t.Run("RP2", func(t *testing.T) {
		tenantID := tombstone(t)
		_, err := svc.ProvisionDirect(withTenant(ctx, tenantID), "rp2-"+tenantID.String(), service.ProvisionDirectRequest{
			TenantID: tenantID, TenantName: "Acme", TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A", Plan: "pro",
		})
		assertRejected(t, tenantID, err)
	})
}
