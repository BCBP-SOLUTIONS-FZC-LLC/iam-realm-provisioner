package unit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

func seedDedicatedRealm(realms *mockTenantRealmRepository, tenantID uuid.UUID, convertedAt *time.Time) {
	shard := "kc-shard-1"
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "tenant-abcd1234", Strategy: domain.RealmStrategyDedicated,
		KeycloakShard: &shard, Status: domain.RealmStatusActive, ConvertedAt: convertedAt, RecordVersion: 1,
	}
}

// RP-4: within the revert window, RevertConversion migrates users back to
// the shared realm and tears down the dedicated realm.
func TestRevertConversion_Success(t *testing.T) {
	svc, kc, realms, _, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	convertedAt := time.Now().UTC().Add(-24 * time.Hour)
	seedDedicatedRealm(realms, tenantID, &convertedAt)

	user1 := uuid.New()
	var migratedFrom, migratedTo string
	var deletedRealm string
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		assert.Equal(t, "tenant-abcd1234", realmName)
		return []uuid.UUID{user1}, nil
	}
	kc.MigrateUserFn = func(ctx context.Context, sourceRealm, targetRealm string, uid uuid.UUID) (bool, error) {
		migratedFrom, migratedTo = sourceRealm, targetRealm
		return false, nil
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		deletedRealm = realmName
		return nil
	}

	result, err := svc.RevertConversion(context.Background(), "rev-1", tenantID, "", 7, 14)
	require.NoError(t, err)
	assert.Equal(t, "trial", result.Realm)
	assert.Equal(t, "shared", result.Strategy)
	assert.Equal(t, "tenant-abcd1234", migratedFrom)
	assert.Equal(t, "trial", migratedTo)
	assert.Equal(t, "tenant-abcd1234", deletedRealm)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStrategyShared, row.Strategy)
	assert.Equal(t, "trial", row.RealmName)
	assert.Nil(t, row.KeycloakShard)
	assert.Nil(t, row.ConvertedAt)
	require.NotNil(t, row.TrialEndsAt)

	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTrialReactivated, pub.events[0].Type)
}

func TestRevertConversion_NotDedicated_Returns400(t *testing.T) {
	svc, _, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)

	_, err := svc.RevertConversion(context.Background(), "rev-2", tenantID, "", 7, 14)
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrInvalidRequest, de.Code)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["rev-2"].Status)
}

// RP-4: past the window (using .details.converted_at shape).
func TestRevertConversion_WindowExpired_ReturnsConvertedAtDetail(t *testing.T) {
	svc, _, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	convertedAt := time.Now().UTC().AddDate(0, 0, -10)
	seedDedicatedRealm(realms, tenantID, &convertedAt)

	_, err := svc.RevertConversion(context.Background(), "rev-3", tenantID, "", 7, 14)
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrRevertWindowExpired, de.Code)
	assert.Equal(t, 409, de.Status())
	require.NotNil(t, de.Details)
	assert.Equal(t, convertedAt.Format(time.RFC3339), de.Details["converted_at"])
}

// A dedicated tenant with no recorded ConvertedAt is treated as
// past-the-window (defensive — should never happen in practice) and the
// detail is an empty string rather than panicking on a nil dereference.
func TestRevertConversion_NilConvertedAt_TreatedAsExpired(t *testing.T) {
	svc, _, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)

	_, err := svc.RevertConversion(context.Background(), "rev-4", tenantID, "", 7, 14)
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrRevertWindowExpired, de.Code)
	assert.Equal(t, "", de.Details["converted_at"])
}

func TestRevertConversion_ListUsersFails_Returns502(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	convertedAt := time.Now().UTC().Add(-time.Hour)
	seedDedicatedRealm(realms, tenantID, &convertedAt)
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return nil, domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}

	_, err := svc.RevertConversion(context.Background(), "rev-5", tenantID, "", 7, 14)
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrConversionFailed, de.Code)
}

func TestRevertConversion_MigrateUserFails_Returns502(t *testing.T) {
	svc, kc, realms, _, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	convertedAt := time.Now().UTC().Add(-time.Hour)
	seedDedicatedRealm(realms, tenantID, &convertedAt)
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return []uuid.UUID{uuid.New()}, nil
	}
	kc.MigrateUserFn = func(ctx context.Context, sourceRealm, targetRealm string, uid uuid.UUID) (bool, error) {
		return false, domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}

	_, err := svc.RevertConversion(context.Background(), "rev-6", tenantID, "", 7, 14)
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrConversionFailed, de.Code)
	assert.Empty(t, pub.events)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStrategyDedicated, row.Strategy, "row must be untouched on failure")
}

func TestRevertConversion_DeleteRealmFails_PropagatesRawError(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	convertedAt := time.Now().UTC().Add(-time.Hour)
	seedDedicatedRealm(realms, tenantID, &convertedAt)
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return nil, nil
	}
	deleteErr := domain.NewError(domain.ErrKeycloakUnavailable, "delete failed")
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return deleteErr }

	_, err := svc.RevertConversion(context.Background(), "rev-7", tenantID, "", 7, 14)
	require.Error(t, err)
	assert.Same(t, deleteErr, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["rev-7"].Status)
}

// CRITICAL (§9.1): a retry after DeleteRealmIfExists succeeded but the
// local commit failed (see TestRevertConversion_UpdateFails_MarksIdempotencyFailed)
// must resume rather than getting permanently stuck — the local row still
// says Strategy=dedicated pointing at a realm that no longer exists in
// Keycloak. ListUsersByTenant returning ErrTenantRealmNotFound is the
// signal that migration and deletion already happened; the retry must
// skip straight to fixing up the local row, never re-attempt MigrateUser
// or DeleteRealmIfExists against the now-absent realm.
func TestRevertConversion_DedicatedRealmAlreadyGone_ResumesToLocalCommit(t *testing.T) {
	svc, kc, realms, _, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	convertedAt := time.Now().UTC().Add(-time.Hour)
	seedDedicatedRealm(realms, tenantID, &convertedAt)
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return nil, domain.NewError(domain.ErrTenantRealmNotFound, "keycloak realm not found")
	}
	kc.MigrateUserFn = func(ctx context.Context, sourceRealm, targetRealm string, uid uuid.UUID) (bool, error) {
		t.Fatal("must not attempt to migrate users out of a realm that no longer exists")
		return false, nil
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		t.Fatal("must not re-attempt deleting a realm that is already gone")
		return nil
	}

	result, err := svc.RevertConversion(context.Background(), "rev-resume", tenantID, "", 7, 14)
	require.NoError(t, err)
	assert.Equal(t, "shared", result.Strategy)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStrategyShared, row.Strategy)
	assert.Equal(t, "trial", row.RealmName)
	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTrialReactivated, pub.events[0].Type)
}

func TestRevertConversion_IdempotentReplay(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	convertedAt := time.Now().UTC().Add(-time.Hour)
	seedDedicatedRealm(realms, tenantID, &convertedAt)
	calls := 0
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		calls++
		return nil, nil
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }

	first, err := svc.RevertConversion(context.Background(), "rev-8", tenantID, "", 7, 14)
	require.NoError(t, err)
	second, err := svc.RevertConversion(context.Background(), "rev-8", tenantID, "", 7, 14)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, first, second)
}

func TestRevertConversion_TenantNotFound(t *testing.T) {
	svc, _, _, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()

	_, err := svc.RevertConversion(context.Background(), "rev-9", tenantID, "", 7, 14)
	require.Error(t, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["rev-9"].Status)
}

// A hard ledger failure from idempotency.Begin propagates immediately — no
// tenant lookup, no Keycloak calls.
func TestRevertConversion_BeginError_Propagates(t *testing.T) {
	svc, kc, _, idem, _ := newProvisioningTestService(t)
	idem.BeginErr = errors.New("ledger unavailable")
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		t.Fatal("Keycloak must not be called when Begin itself fails")
		return nil, nil
	}

	_, err := svc.RevertConversion(context.Background(), "rev-10", uuid.New(), "", 7, 14)
	require.Error(t, err)
}

// The local-commit tx's realms.Update failing (after the dedicated realm
// was already deleted and its users migrated back) marks the idempotency
// key failed and enqueues no events.
func TestRevertConversion_UpdateFails_MarksIdempotencyFailed(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	realms.UpdateErr = errors.New("db down")
	tenantID := uuid.New()
	convertedAt := time.Now().UTC().Add(-time.Hour)
	seedDedicatedRealm(realms, tenantID, &convertedAt)
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }

	_, err := svc.RevertConversion(context.Background(), "rev-11", tenantID, "", 7, 14)
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["rev-11"].Status)
}

// When the tx runner wires no event publisher into ctx, the local commit
// still lands and the call still succeeds — just with no event enqueued.
func TestRevertConversion_NoEventPublisherInContext_StillCommitsLocally(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx := newMockTxRunnerNoPublisher()
	svc := service.NewProvisioningService(realms, idem, kc, tx, "trial", nil)
	tenantID := uuid.New()
	convertedAt := time.Now().UTC().Add(-time.Hour)
	seedDedicatedRealm(realms, tenantID, &convertedAt)
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }

	result, err := svc.RevertConversion(context.Background(), "rev-12", tenantID, "", 7, 14)
	require.NoError(t, err)
	assert.Equal(t, "shared", result.Strategy)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStrategyShared, row.Strategy)
}

// idempotency.Complete's own persistence failure is best-effort — logged,
// not propagated; the caller still sees a successful result.
func TestRevertConversion_IdempotencyCompleteFailure_LoggedNotPropagated(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	logger := &mockLogger{}
	svc.WithLogger(logger)
	idem.CompleteErr = errors.New("ledger unavailable")
	tenantID := uuid.New()
	convertedAt := time.Now().UTC().Add(-time.Hour)
	seedDedicatedRealm(realms, tenantID, &convertedAt)
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }

	result, err := svc.RevertConversion(context.Background(), "rev-13", tenantID, "", 7, 14)
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}
