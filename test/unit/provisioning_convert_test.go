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
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

func seedSharedRealm(realms *mockTenantRealmRepository, tenantID uuid.UUID) {
	slug := "acme"
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared,
		Status: domain.RealmStatusActive, RecordVersion: 1, TenantSlug: &slug,
	}
}

func stubConvertKeycloak(kc *mockKeycloakAdmin) {
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }
}

// G-1 (HLD v1.59 §17.2): the converted realm is tenant-<slug>, from the slug
// recorded at RP-1, never tenant-<tenant_id>.
func TestConvert_NamesTheRealmFromTheRecordedSlug(t *testing.T) {
	svc, kc, realms, _, pub := newProvisioningTestService(t)
	svc.WithRealmOwners(realms)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	stubConvertKeycloak(kc)
	var createdRealm, clientsSlug string
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { createdRealm = realmName; return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		clientsSlug = slug
		return nil, nil
	}

	result, err := svc.Convert(context.Background(), "conv-key-slug", tenantID, service.ConvertRequest{Plan: "pro"})
	require.NoError(t, err)
	assert.Equal(t, "tenant-acme", createdRealm)
	assert.Equal(t, "acme", clientsSlug)
	assert.Equal(t, "tenant-acme", result.Realm)
	row := realms.rows[tenantID]
	assert.Equal(t, "tenant-acme", row.RealmName)
	require.NotNil(t, row.TenantSlug)
	assert.Equal(t, "acme", *row.TenantSlug, "the slug survives the conversion update")
	for _, ev := range pub.events {
		switch d := ev.Data.(type) {
		case domain.TenantConvertedPayload:
			assert.Equal(t, "tenant-acme", d.Realm)
		case domain.TenantRealmReadyPayload:
			assert.Equal(t, "tenant-acme", d.Realm)
		}
	}
}

func TestConvert_NoRecordedSlug_RealmNameUnavailable_NoKeycloakCall(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	realms.rows[tenantID].TenantSlug = nil
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		t.Fatal("no realm may be created without a slug")
		return nil
	}

	_, err := svc.Convert(context.Background(), "conv-key-noslug", tenantID, service.ConvertRequest{Plan: "pro"})
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRealmNameUnavailable, de.Code)
	assert.Equal(t, 409, de.Status())
	assert.Equal(t, domain.OpStatusFailed, idem.rows["conv-key-noslug"].Status)
}

// The realm-exists conflict is resumed as this tenant's own earlier attempt,
// so a tenant-<slug> held by another live tenant must stop the conversion
// before Keycloak is touched: resuming would migrate this tenant's users
// into the other tenant's realm.
func TestConvert_RealmHeldByAnotherTenant_Refused(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	svc.WithRealmOwners(realms)
	tenantID, otherID := uuid.New(), uuid.New()
	seedSharedRealm(realms, tenantID)
	shard := "kc-shard-1"
	realms.rows[otherID] = &domain.TenantRealm{
		TenantID: otherID, RealmName: "tenant-acme", Strategy: domain.RealmStrategyDedicated,
		KeycloakShard: &shard, Status: domain.RealmStatusActive, RecordVersion: 1,
	}
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		t.Fatal("must not create or resume into another tenant's realm")
		return nil
	}
	kc.MigrateUserFn = func(ctx context.Context, src, dst string, uid uuid.UUID) (bool, error) {
		t.Fatal("must not migrate users")
		return false, nil
	}

	_, err := svc.Convert(context.Background(), "conv-key-taken", tenantID, service.ConvertRequest{Plan: "pro"})
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRealmNameUnavailable, de.Code)
	assert.Equal(t, domain.RealmStrategyShared, realms.rows[tenantID].Strategy)
}

func TestConvert_RealmOwnerLookupFails_FailsClosed(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	realms.DedicatedRealmOwnerErr = errors.New("sys pool down")
	svc.WithRealmOwners(realms)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		t.Fatal("must not proceed without the ownership check")
		return nil
	}

	_, err := svc.Convert(context.Background(), "conv-key-lookup", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
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
	svc := service.NewProvisioningService(realms, idem, kc, ts, tx, &mockPendingAuditEntryRepository{}, "trial", nil).WithShardLoads(realms)
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

// RP-3 conversion is O(users) Keycloak calls and can outrun the request
// deadline. When the request is done first, the caller gets 409
// operation_in_progress (its contract: retry with the same key) while the
// conversion keeps running on a detached context instead of being cut off
// half-way through the user migration; the retry then replays the result.
func TestConvert_RequestDoneFirst_ReturnsInProgress_ConversionCompletes(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	user := uuid.New()

	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return []uuid.UUID{user}, nil
	}
	started, release := make(chan struct{}), make(chan struct{})
	kc.MigrateUserFn = func(ctx context.Context, sourceRealm, targetRealm string, uid uuid.UUID) (bool, error) {
		close(started)
		<-release
		assert.NoError(t, ctx.Err(), "the migration must not run on the request's cancelled context")
		return false, nil
	}
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }
	kc.DeleteServiceAccountClientIfExistsFn = func(ctx context.Context, realmName, clientID string) error { return nil }

	reqCtx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	_, err := svc.Convert(reqCtx, "conv-slow", tenantID, service.ConvertRequest{Plan: "pro"})
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrOperationInProgress, de.Code)

	close(release)
	require.NoError(t, svc.Drain(context.Background()))
	assert.Equal(t, domain.OpStatusSucceeded, idem.rows["conv-slow"].Status, "the detached conversion finished and recorded its outcome")

	result, err := svc.Convert(context.Background(), "conv-slow", tenantID, service.ConvertRequest{Plan: "pro"})
	require.NoError(t, err, "the retry replays the stored result")
	assert.Equal(t, 1, result.UsersMigrated)
}

// A conversion longer than its own budget is cut off and recorded failed
// (retryable), never left in_progress.
func TestConvert_DetachedBudgetExceeded_FailsTheKey(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	svc.WithConvertTimeout(50 * time.Millisecond)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)

	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		assert.NoError(t, ctx.Err(), "compensating teardown must not inherit the expired budget")
		return nil
	}

	_, err := svc.Convert(context.Background(), "conv-budget", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["conv-budget"].Status)
}

// A panic inside the detached conversion goroutine must not crash the
// process: it is recovered, the key is recorded failed (retryable) and the
// caller gets an error.
func TestConvert_DetachedPanic_RecoveredAndFailsTheKey(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)

	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		panic("keycloak adapter bug")
	}

	_, err := svc.Convert(context.Background(), "conv-panic", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keycloak adapter bug")
	assert.Equal(t, domain.OpStatusFailed, idem.rows["conv-panic"].Status)
	require.NoError(t, svc.Drain(context.Background()))
}

// Drain returns the shutdown context's error when a conversion is still
// running at the deadline, rather than blocking past the grace period.
func TestDrain_DeadlineBeforeConversionFinishes_ReturnsContextError(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)

	started, release := make(chan struct{}), make(chan struct{})
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		close(started)
		<-release
		return errors.New("stopped")
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }

	reqCtx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	_, err := svc.Convert(reqCtx, "conv-drain", tenantID, service.ConvertRequest{Plan: "pro"})
	require.Error(t, err)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer drainCancel()
	assert.ErrorIs(t, svc.Drain(drainCtx), context.DeadlineExceeded)

	close(release)
	require.NoError(t, svc.Drain(context.Background()))
}

// With nothing in flight Drain returns immediately.
func TestDrain_NothingInFlight_ReturnsNil(t *testing.T) {
	svc, _, _, _, _ := newProvisioningTestService(t)
	require.NoError(t, svc.Drain(context.Background()))
}

// CRITICAL (RP-GAP-004, TS-RP-GAP-002): RP-3 must call TS-4 (RegisterPrincipal
// with the new dedicated-realm sub) → TS-1 (IssueOrRotateCredential for the
// new credential) → TS-2 (RevokeCredential on the old shared-realm credential)
// in that strict order, all before deleting the old shared-realm Keycloak
// client. If TS-2 ran before TS-1 there would be a window where the
// platform-automation principal has no valid credential; if the client deletion
// ran before TS-2 the old credential would outlive the client it was issued for.
func TestConvert_PrincipalLifecycleSequence_TS4_TS1_TS2_ThenClientDelete(t *testing.T) {
	var callOrder []string

	kc := &mockKeycloakAdmin{}
	kc.CreateServiceAccountClientFn = func(ctx context.Context, realmName, clientID, jwksURL string) (uuid.UUID, bool, error) {
		return uuid.New(), true, nil
	}
	kc.DeleteServiceAccountClientIfExistsFn = func(ctx context.Context, realmName, clientID string) error {
		callOrder = append(callOrder, "delete-old-client")
		return nil
	}

	oldPrincipalID := uuid.New()
	ts := &mockTokenServiceClient{
		JWKSURLForFn: func(tenantID uuid.UUID) string {
			return "http://ts.test/jwks/" + tenantID.String()
		},
		RegisterPrincipalFn: func(ctx context.Context, tenantID uuid.UUID, req port.RegisterPrincipalRequest) (*port.RegisterPrincipalResult, error) {
			callOrder = append(callOrder, "TS-4")
			return &port.RegisterPrincipalResult{PrincipalID: oldPrincipalID, Created: false, RecordVersion: 2}, nil
		},
		IssueOrRotateCredentialFn: func(ctx context.Context, tenantID, principalID uuid.UUID, req port.IssueCredentialRequest) (*port.IssueCredentialResult, error) {
			callOrder = append(callOrder, "TS-1")
			return &port.IssueCredentialResult{Version: 2, Secret: "new-secret", OpenBaoPath: "iam/test/v2", RecordVersion: 2}, nil
		},
		RevokeCredentialFn: func(ctx context.Context, tenantID, principalID uuid.UUID) error {
			callOrder = append(callOrder, "TS-2")
			assert.Equal(t, oldPrincipalID, principalID, "TS-2 must target the old shared-realm principal")
			return nil
		},
	}

	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, _ := newMockTxRunner()
	svc := service.NewProvisioningService(realms, idem, kc, ts, tx, &mockPendingAuditEntryRepository{}, "trial", nil).WithShardLoads(realms)

	tenantID := uuid.New()
	oldClientID := "platform-automation-" + tenantID.String()
	seedSharedRealm(realms, tenantID)
	realms.rows[tenantID].ServiceAccountPrincipalID = &oldPrincipalID
	realms.rows[tenantID].ServiceAccountKeycloakClientID = &oldClientID

	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }

	result, err := svc.Convert(context.Background(), "conv-seq-key", tenantID, service.ConvertRequest{Plan: "pro"})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, []string{"TS-4", "TS-1", "TS-2", "delete-old-client"}, callOrder,
		"strict ordering required: TS-4 (RegisterPrincipal) → TS-1 (IssueOrRotate) → "+
			"TS-2 (RevokeCredential) → delete old shared-realm Keycloak client")
}

// TS-2 failure is best-effort: a revocation error must not fail an already-
// committed conversion, only get logged as WARN.
func TestConvert_TS2RevokeCredentialFails_NonFatal(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	ts := wirePlatformAutomationHappyPath(kc)
	logger := &mockLogger{}
	ts.RevokeCredentialFn = func(ctx context.Context, tenantID, principalID uuid.UUID) error {
		return domain.NewError(domain.ErrTokenServiceUnavailable, "ts down")
	}

	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, _ := newMockTxRunner()
	svc := service.NewProvisioningService(realms, idem, kc, ts, tx, &mockPendingAuditEntryRepository{}, "trial", nil).WithShardLoads(realms)
	svc.WithLogger(logger)

	tenantID := uuid.New()
	oldPrincipalID := uuid.New()
	oldClientID := "platform-automation-" + tenantID.String()
	seedSharedRealm(realms, tenantID)
	realms.rows[tenantID].ServiceAccountPrincipalID = &oldPrincipalID
	realms.rows[tenantID].ServiceAccountKeycloakClientID = &oldClientID

	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }

	result, err := svc.Convert(context.Background(), "conv-ts2-fail", tenantID, service.ConvertRequest{Plan: "pro"})
	require.NoError(t, err, "TS-2 failure must not fail an already-committed conversion")
	assert.Equal(t, "dedicated", result.Strategy)
	assert.GreaterOrEqual(t, logger.warnCalls, 1, "TS-2 failure must be logged as WARN")
}

// When ServiceAccountPrincipalID is nil (legacy row provisioned before
// mintPlatformAutomation was added), TS-2 must not be called — there is
// no old credential to revoke.
func TestConvert_NoPriorPrincipal_TS2Skipped(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	ts := wirePlatformAutomationHappyPath(kc)
	ts.RevokeCredentialFn = func(ctx context.Context, tenantID, principalID uuid.UUID) error {
		t.Fatal("TS-2 must not be called when there is no prior principal (oldPrincipalID == nil)")
		return nil
	}

	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, _ := newMockTxRunner()
	svc := service.NewProvisioningService(realms, idem, kc, ts, tx, &mockPendingAuditEntryRepository{}, "trial", nil).WithShardLoads(realms)

	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)
	// ServiceAccountPrincipalID intentionally left nil — legacy row with no
	// prior platform-automation mint.

	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	kc.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }

	result, err := svc.Convert(context.Background(), "conv-no-old-principal", tenantID, service.ConvertRequest{Plan: "pro"})
	require.NoError(t, err)
	assert.Equal(t, "dedicated", result.Strategy)
}
