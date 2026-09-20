package unit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

func seedSharedRealm(realms *mockTenantRealmRepository, tenantID uuid.UUID) {
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared,
		Status: domain.RealmStatusActive, RecordVersion: 1,
	}
}

// Convert's post-success cleanup must target the PERSISTED
// ServiceAccountKeycloakClientID, not a name re-derived from the naming
// convention — a legacy/out-of-band row can diverge from that convention,
// and re-deriving would then silently target the wrong (nonexistent)
// client, leaking the real one with no error signal.
func TestConvert_Success_DeletesThePersistedOldClientID_NotADerivedName(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	legacyClientID := "legacy-client-name-not-matching-convention"
	realms.rows[tenantID].ServiceAccountKeycloakClientID = &legacyClientID

	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }
	var deletedClientID string
	kc.DeleteServiceAccountClientIfExistsFn = func(ctx context.Context, realmName, clientID string) error {
		deletedClientID = clientID
		return nil
	}

	_, err := svc.Convert(context.Background(), "conv-key-legacy-client", tenantID, service.ConvertRequest{Plan: "pro"})
	require.NoError(t, err)
	assert.Equal(t, legacyClientID, deletedClientID)
}

func TestConvert_Success(t *testing.T) {
	svc, kc, realms, _, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)

	user1, user2 := uuid.New(), uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		return []string{"tender-web", "tender-mobile", "tender-gateway"}, nil
	}
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		assert.Equal(t, "trial", realmName)
		return []uuid.UUID{user1, user2}, nil
	}
	migrated := map[uuid.UUID]bool{}
	kc.MigrateUserFn = func(ctx context.Context, sourceRealm, targetRealm string, uid uuid.UUID) (bool, error) {
		migrated[uid] = true
		return uid == user2, nil // user2 needs WebAuthn re-enroll
	}
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error {
		assert.Equal(t, "trial", realmName)
		return nil
	}
	var deletedOldClientRealm, deletedOldClientID string
	kc.DeleteServiceAccountClientIfExistsFn = func(ctx context.Context, realmName, clientID string) error {
		deletedOldClientRealm, deletedOldClientID = realmName, clientID
		return nil
	}

	result, err := svc.Convert(context.Background(), "conv-key-1", tenantID, service.ConvertRequest{Plan: "pro", BillingSubscriptionID: "sub_123"})
	require.NoError(t, err)

	// §2.5: once the principal has moved to the new dedicated realm's
	// client, the stale trial-realm client is retired.
	assert.Equal(t, "trial", deletedOldClientRealm)
	assert.Equal(t, "platform-automation-"+tenantID.String(), deletedOldClientID)

	assert.Equal(t, "dedicated", result.Strategy)
	assert.Equal(t, 2, result.UsersMigrated)
	assert.Equal(t, 1, result.WebauthnReenrollRequired)
	assert.True(t, result.SharedSessionsTerminated)
	assert.True(t, migrated[user1])
	assert.True(t, migrated[user2])

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStrategyDedicated, row.Strategy)
	require.NotNil(t, row.KeycloakShard)

	// TenantRealmReady + TenantConverted, both carrying direct_signup=false.
	require.Len(t, pub.events, 2)
	assert.Equal(t, domain.EventTenantRealmReady, pub.events[0].Type)
	assert.Equal(t, domain.EventTenantConverted, pub.events[1].Type)
	converted := pub.events[1].Data.(domain.TenantConvertedPayload)
	assert.False(t, converted.DirectSignup)
	assert.Equal(t, 2, converted.UsersMigrated)
	assert.Equal(t, "pro", converted.Plan)
}

// bestEffortDeleteServiceAccountClient's own failure is best-effort — it
// must not fail an otherwise-successful conversion, just get logged.
func TestConvert_Success_StaleClientDeleteFails_LoggedNotPropagated(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	logger := &mockLogger{}
	svc.WithLogger(logger)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)

	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }
	kc.DeleteServiceAccountClientIfExistsFn = func(ctx context.Context, realmName, clientID string) error {
		return errors.New("keycloak down")
	}

	result, err := svc.Convert(context.Background(), "conv-key-delete-fail", tenantID, service.ConvertRequest{Plan: "pro"})
	require.NoError(t, err, "the stale-client cleanup failing must not fail the conversion itself")
	assert.Equal(t, "dedicated", result.Strategy)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// Already-dedicated tenants must be rejected — Convert requires shared strategy.
func TestConvert_AlreadyDedicated_Rejected(t *testing.T) {
	svc, _, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	shard := "kc-shard-1"
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "tenant-acme", Strategy: domain.RealmStrategyDedicated,
		KeycloakShard: &shard, Status: domain.RealmStatusActive,
	}

	_, err := svc.Convert(context.Background(), "conv-key-2", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrRealmAlreadyDedicated, de.Code)
}

// A mid-migration Keycloak failure must trigger compensating teardown of
// the new dedicated realm (§9.3) and leave the shared realm row untouched.
func TestConvert_MigrationFailure_CompensatingTeardown(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)

	user1 := uuid.New()
	deletedRealms := []string{}
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		return []string{"tender-web"}, nil
	}
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return []uuid.UUID{user1}, nil
	}
	kc.MigrateUserFn = func(ctx context.Context, sourceRealm, targetRealm string, uid uuid.UUID) (bool, error) {
		return false, domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		deletedRealms = append(deletedRealms, realmName)
		return nil
	}

	_, err := svc.Convert(context.Background(), "conv-key-3", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrConversionFailed, de.Code)

	require.Len(t, deletedRealms, 1, "the partially-created dedicated realm must be torn down")

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStrategyShared, row.Strategy, "the shared realm row must be untouched")
	assert.Empty(t, pub.events, "no event should be enqueued on a failed conversion")

	opRow := idem.rows["conv-key-3"]
	require.NotNil(t, opRow)
	assert.Equal(t, domain.OpStatusFailed, opRow.Status)
}

// CRITICAL (§9.3): once one user has actually been migrated into the new
// dedicated realm, a later user's migration failure must NOT tear the realm
// down — MigrateUser deletes each user from the source realm before
// creating it in the target, so the already-migrated user now exists
// nowhere else. Deleting the realm at that point would destroy them
// permanently, not just roll back the failed attempt.
func TestConvert_MigrationFailure_AfterPartialSuccess_NoTeardown(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)

	user1, user2 := uuid.New(), uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		return []string{"tender-web"}, nil
	}
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return []uuid.UUID{user1, user2}, nil
	}
	kc.MigrateUserFn = func(ctx context.Context, sourceRealm, targetRealm string, uid uuid.UUID) (bool, error) {
		if uid == user1 {
			return false, nil // user1 migrates successfully
		}
		return false, domain.NewError(domain.ErrKeycloakUnavailable, "boom") // user2 fails
	}
	deleteCalls := 0
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		deleteCalls++
		return nil
	}

	_, err := svc.Convert(context.Background(), "conv-key-partial", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)

	assert.Zero(t, deleteCalls, "the realm must not be deleted once it holds a user migrated out of the shared realm")
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["conv-key-partial"].Status)
}

// CRITICAL (§9.3): a retry with the same Idempotency-Key after a
// partial-success failure must resume against the existing (already
// partially-populated) dedicated realm rather than erroring on
// ErrRealmAlreadyDedicated or deleting it, and must complete the conversion
// using only the users still remaining in the shared realm.
func TestConvert_Retry_AfterPartialMigrationFailure_ResumesAgainstExistingRealm(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)

	user1, user2 := uuid.New(), uuid.New()
	deleteCalls := 0
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		deleteCalls++
		return nil
	}
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		return []string{"tender-web"}, nil
	}

	// First attempt: realm creation succeeds, user1 migrates, user2 fails.
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return []uuid.UUID{user1, user2}, nil
	}
	kc.MigrateUserFn = func(ctx context.Context, sourceRealm, targetRealm string, uid uuid.UUID) (bool, error) {
		if uid == user1 {
			return false, nil
		}
		return false, domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}

	_, err := svc.Convert(context.Background(), "conv-key-retry", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
	require.Equal(t, domain.OpStatusFailed, idem.rows["conv-key-retry"].Status)
	require.Zero(t, deleteCalls)

	// Second attempt (retry, same Idempotency-Key): realm now already
	// exists — the shared realm row still only lists the one remaining
	// unmigrated user (user1 was already removed from it by MigrateUser).
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		return domain.NewError(domain.ErrRealmAlreadyDedicated, "realm already exists")
	}
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return []uuid.UUID{user2}, nil
	}
	kc.MigrateUserFn = func(ctx context.Context, sourceRealm, targetRealm string, uid uuid.UUID) (bool, error) {
		return false, nil
	}
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }

	result, err := svc.Convert(context.Background(), "conv-key-retry", tenantID, service.ConvertRequest{Plan: "pro"})
	require.NoError(t, err)
	assert.Equal(t, "dedicated", result.Strategy)
	assert.Zero(t, deleteCalls, "the resumed realm must never be deleted across either attempt")
	assert.Len(t, pub.events, 2)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStrategyDedicated, row.Strategy)
}

func TestConvert_TenantNotFound(t *testing.T) {
	svc, _, _, idem, _ := newProvisioningTestService(t)
	_, err := svc.Convert(context.Background(), "conv-key-nf", uuid.New(), service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["conv-key-nf"].Status)
}

func TestConvert_SelectShardFailure(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	realms.CountsByShardErr = errors.New("db down")
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		t.Fatal("Keycloak must not be called when shard selection fails")
		return nil
	}

	_, err := svc.Convert(context.Background(), "conv-key-4", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["conv-key-4"].Status)
}

// Keycloak-effect-first: CreateRealmFromTemplate is the very first Keycloak
// call — nothing was created yet, so no teardown is needed.
func TestConvert_CreateRealmFails_NoTeardownNeeded(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}

	_, err := svc.Convert(context.Background(), "conv-key-5", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["conv-key-5"].Status)
	row, _ := realms.FindByTenantID(context.Background(), tenantID)
	assert.Equal(t, domain.RealmStrategyShared, row.Strategy)
}

// Compensating teardown: RegisterOIDCClients failing tears down the new
// dedicated realm and returns 502 conversion_failed.
func TestConvert_RegisterOIDCClientsFails_TeardownCalled(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	var deletedRealm string
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		return nil, domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		deletedRealm = realmName
		return nil
	}

	_, err := svc.Convert(context.Background(), "conv-key-6", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrConversionFailed, de.Code)
	assert.NotEmpty(t, deletedRealm)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["conv-key-6"].Status)
}

// §2.5/RP-17: the mint step runs right after RegisterOIDCClients succeeds,
// before the user-migration loop — a failure there still tears down the
// new dedicated realm (realmMayHaveMigratedUsers is still false).
func TestConvert_MintPlatformAutomationFails_BeforeMigration_TeardownCalled(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	var deletedRealm string
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		return []string{"tender-web"}, nil
	}
	kc.CreateServiceAccountClientFn = func(ctx context.Context, realmName, clientID, jwksURL string) (uuid.UUID, bool, error) {
		return uuid.Nil, false, domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		t.Fatal("mint failure must short-circuit before ListUsersByTenant/migration")
		return nil, nil
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		deletedRealm = realmName
		return nil
	}

	_, err := svc.Convert(context.Background(), "conv-key-6b", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrConversionFailed, de.Code)
	assert.NotEmpty(t, deletedRealm)
}

// "Carry across RP-3": the same Token Service principal is re-registered
// against the new dedicated-realm client — a new principal_sub, same
// principal_id — and that principal_id lands on the Update'd local row.
func TestConvert_MintPlatformAutomation_CarriesPrincipalAcrossRealms(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }
	var gotClientID string
	kc.CreateServiceAccountClientFn = func(ctx context.Context, realmName, clientID, jwksURL string) (uuid.UUID, bool, error) {
		gotClientID = clientID
		return uuid.New(), true, nil
	}

	_, err := svc.Convert(context.Background(), "conv-key-6c", tenantID, service.ConvertRequest{Plan: "pro"})
	require.NoError(t, err)
	assert.Equal(t, "platform-automation", gotClientID, "the dedicated realm uses the literal base name, not the trial-realm tenant-scoped one")

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	require.NotNil(t, row.ServiceAccountPrincipalID)
	require.NotNil(t, row.ServiceAccountKeycloakClientID)
	assert.Equal(t, "platform-automation", *row.ServiceAccountKeycloakClientID)
}

// Compensating teardown: ListUsersByTenant failing (after the realm and
// OIDC clients were created) also tears down the new dedicated realm.
func TestConvert_ListUsersByTenantFails_TeardownCalled(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	var deletedRealm string
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		return []string{"tender-web"}, nil
	}
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return nil, domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		deletedRealm = realmName
		return nil
	}

	_, err := svc.Convert(context.Background(), "conv-key-7", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrConversionFailed, de.Code)
	assert.NotEmpty(t, deletedRealm)
}

// ForceLogoutRealm failing is best-effort — logged WARN, non-fatal, the
// conversion still completes successfully.
func TestConvert_ForceLogoutRealmFails_NonFatal(t *testing.T) {
	svc, kc, realms, _, pub := newProvisioningTestService(t)
	logger := &mockLogger{}
	svc.WithLogger(logger)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}

	result, err := svc.Convert(context.Background(), "conv-key-8", tenantID, service.ConvertRequest{Plan: "pro"})
	require.NoError(t, err)
	assert.Equal(t, "dedicated", result.Strategy)
	assert.Len(t, pub.events, 2, "conversion still completes despite the non-fatal force-logout failure")
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

func TestConvert_IdempotentReplay(t *testing.T) {
	svc, kc, realms, _, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	calls := 0
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { calls++; return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }

	req := service.ConvertRequest{Plan: "pro"}
	first, err := svc.Convert(context.Background(), "conv-key-9", tenantID, req)
	require.NoError(t, err)
	second, err := svc.Convert(context.Background(), "conv-key-9", tenantID, req)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, first, second)
	assert.Len(t, pub.events, 2)
}

// idempotency.Complete's own persistence failure is best-effort, logged not
// propagated — the caller still gets a successful result.
func TestConvert_IdempotencyCompleteFailure_LoggedNotPropagated(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	logger := &mockLogger{}
	svc.WithLogger(logger)
	idem.CompleteErr = errors.New("ledger unavailable")
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }

	result, err := svc.Convert(context.Background(), "conv-key-10", tenantID, service.ConvertRequest{Plan: "pro"})
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// A hard ledger failure from idempotency.Begin propagates immediately — no
// tenant lookup, no Keycloak calls.
func TestConvert_BeginError_Propagates(t *testing.T) {
	svc, kc, _, idem, _ := newProvisioningTestService(t)
	idem.BeginErr = errors.New("ledger unavailable")
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		t.Fatal("Keycloak must not be called when Begin itself fails")
		return nil
	}

	_, err := svc.Convert(context.Background(), "conv-key-11", uuid.New(), service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
}

// The local-commit tx's realms.Update failing (after every Keycloak step
// succeeded, including per-user migration) marks the idempotency key
// failed and enqueues no events. Unlike a mid-migration Keycloak failure,
// this does NOT trigger compensating teardown — the dedicated realm is now
// the source of truth and a retry with the same key must find it via
// FindByTenantID + re-attempt the local commit, not delete it.
func TestConvert_UpdateFails_NoTeardown_MarksIdempotencyFailed(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	realms.UpdateErr = errors.New("db down")
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }
	var teardownCalled bool
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { teardownCalled = true; return nil }

	_, err := svc.Convert(context.Background(), "conv-key-12", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
	assert.False(t, teardownCalled, "a local-commit failure must not tear down the already-provisioned dedicated realm")
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["conv-key-12"].Status)
}

// The TenantRealmReady enqueue failing propagates and stops TenantConverted
// from being enqueued.
func TestConvert_FirstEventEnqueueFails(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }
	pub.EnqueueErr = errors.New("outbox unavailable")
	pub.FailFromCall = 1

	_, err := svc.Convert(context.Background(), "conv-key-13", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["conv-key-13"].Status)
}

// When the tx runner wires no event publisher into ctx, the local commit
// still lands and the call still succeeds — just with no events enqueued.
func TestConvert_NoEventPublisherInContext_StillCommitsLocally(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	ts := wirePlatformAutomationHappyPath(kc)
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx := newMockTxRunnerNoPublisher()
	svc := service.NewProvisioningService(realms, idem, kc, ts, tx, "trial", nil)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }

	result, err := svc.Convert(context.Background(), "conv-key-14", tenantID, service.ConvertRequest{Plan: "pro"})
	require.NoError(t, err)
	assert.Equal(t, "dedicated", result.Strategy)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStrategyDedicated, row.Strategy)
}
