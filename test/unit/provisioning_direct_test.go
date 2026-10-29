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

func TestProvisionDirect_Success(t *testing.T) {
	svc, kc, realms, _, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	adminID := uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		assert.Equal(t, "tenant-acme", realmName)
		assert.Equal(t, "v1", v)
		return nil
	}
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		return []string{"tender-web", "tender-mobile"}, nil
	}
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		assert.Contains(t, req.RequiredActions, "VERIFY_EMAIL")
		return adminID, nil
	}

	result, err := svc.ProvisionDirect(context.Background(), "pd-1", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantName: "Acme Ltd", TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
		Plan: "pro", BillingSubscriptionID: "sub_1",
	})
	require.NoError(t, err)
	assert.Equal(t, "tenant-acme", result.Realm)
	assert.Equal(t, "dedicated", result.Strategy)
	assert.Equal(t, "kc-shard-1", result.KeycloakShard)
	assert.Equal(t, []string{"tender-web", "tender-mobile"}, result.OIDCClients)
	assert.Equal(t, adminID, result.KeycloakUserID,
		"RP-2 returns the owner's Keycloak sub, like RP-1 — the BFF's I-1 owner_user_id")

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStrategyDedicated, row.Strategy)
	require.NotNil(t, row.KeycloakShard)
	assert.Equal(t, "kc-shard-1", *row.KeycloakShard)

	// TenantIdentityProvisioned first (BFF-2 backstop), owner = the admin
	// user RP-2 just created, then the three existing events in order.
	require.Len(t, pub.events, 4)
	assert.Equal(t, domain.EventTenantIdentityProvisioned, pub.events[0].Type)
	assert.Equal(t, domain.TenantIdentityProvisionedPayload{
		TenantID: tenantID, OwnerUserID: adminID, Slug: "acme", Name: "Acme Ltd", Plan: "pro",
		SignupType: "direct_paid", Realm: "tenant-acme", RealmType: "dedicated",
	}, pub.events[0].Data.(domain.TenantIdentityProvisionedPayload))
	assert.Equal(t, domain.EventTenantRealmReady, pub.events[1].Type)
	assert.Equal(t, domain.EventDirectPaidSignup, pub.events[2].Type)
	assert.Equal(t, domain.EventTenantConverted, pub.events[3].Type)
	converted := pub.events[3].Data.(domain.TenantConvertedPayload)
	assert.True(t, converted.DirectSignup)
	assert.Equal(t, 0, converted.UsersMigrated)
	require.NotNil(t, converted.BillingSubscriptionID)
	assert.Equal(t, "sub_1", *converted.BillingSubscriptionID)
}

// No billing_subscription_id supplied -> the pointer stays nil rather than "".
func TestProvisionDirect_NoBillingSubscriptionID(t *testing.T) {
	svc, kc, _, _, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}

	_, err := svc.ProvisionDirect(context.Background(), "pd-2", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A", Plan: "pro",
	})
	require.NoError(t, err)
	converted := pub.events[3].Data.(domain.TenantConvertedPayload)
	assert.Nil(t, converted.BillingSubscriptionID)
}

// Keycloak-effect-first (§9.1): CreateRealmFromTemplate failing is the very
// first Keycloak call — nothing was created yet, so no teardown call is
// made (DeleteRealmIfExistsFn is left nil; a call would panic the test).
func TestProvisionDirect_CreateRealmFails_NoTeardownNeeded(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}

	_, err := svc.ProvisionDirect(context.Background(), "pd-3", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
	_, findErr := realms.FindByTenantID(context.Background(), tenantID)
	assert.Error(t, findErr)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["pd-3"].Status)
}

// Compensating teardown (§9.3): RegisterOIDCClients failing after the realm
// was created triggers bestEffortDeleteRealm on that realm.
func TestProvisionDirect_RegisterOIDCClientsFails_TeardownCalled(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	var deletedRealm string
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		return nil, domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		deletedRealm = realmName
		return nil
	}

	_, err := svc.ProvisionDirect(context.Background(), "pd-4", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
	assert.Equal(t, "tenant-acme", deletedRealm)
	_, findErr := realms.FindByTenantID(context.Background(), tenantID)
	assert.Error(t, findErr, "no local row on a torn-down provisioning attempt")
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["pd-4"].Status)
}

// Compensating teardown, second step: CreateInvitedUser failing after both
// the realm AND the OIDC clients exist still tears down the same realm.
func TestProvisionDirect_CreateInvitedUserFails_TeardownCalled(t *testing.T) {
	svc, kc, _, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	var deletedRealm string
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		return []string{"tender-web"}, nil
	}
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		deletedRealm = realmName
		return nil
	}

	_, err := svc.ProvisionDirect(context.Background(), "pd-5", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
	assert.Equal(t, "tenant-acme", deletedRealm)
}

// §2.5/RP-17: the mint step runs after CreateInvitedUser succeeds — a
// failure there triggers the same compensating teardown as the two
// Keycloak-effect steps before it.
func TestProvisionDirect_MintPlatformAutomationFails_TeardownCalled(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	var deletedRealm string
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		return []string{"tender-web"}, nil
	}
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}
	kc.CreateServiceAccountClientFn = func(ctx context.Context, realmName, clientID, jwksURL string) (uuid.UUID, bool, error) {
		return uuid.Nil, false, domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		deletedRealm = realmName
		return nil
	}

	_, err := svc.ProvisionDirect(context.Background(), "pd-6", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
	assert.Equal(t, "tenant-acme", deletedRealm)
	_, findErr := realms.FindByTenantID(context.Background(), tenantID)
	assert.Error(t, findErr, "no local row on a torn-down provisioning attempt")
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["pd-6"].Status)
}

// The mint step's result (principal_id, keycloak_client_id) lands on the
// local row committed with the rest of RP-2's Upsert.
func TestProvisionDirect_MintPlatformAutomation_PersistsPrincipalAndClientID(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}
	var gotClientID, gotJWKSURL string
	kc.CreateServiceAccountClientFn = func(ctx context.Context, realmName, clientID, jwksURL string) (uuid.UUID, bool, error) {
		gotClientID, gotJWKSURL = clientID, jwksURL
		return uuid.New(), true, nil
	}

	_, err := svc.ProvisionDirect(context.Background(), "pd-7", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.NoError(t, err)
	assert.Equal(t, "platform-automation", gotClientID)
	assert.NotEmpty(t, gotJWKSURL, "the client must be minted with a JWKS URL, not a plain client-secret authenticator (EXT-6)")

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	require.NotNil(t, row.ServiceAccountPrincipalID)
	require.NotNil(t, row.ServiceAccountKeycloakClientID)
	assert.Equal(t, "platform-automation", *row.ServiceAccountKeycloakClientID)
}

// §9.2 lost-response-retry resolution: CreateInvitedUser 409s on a retry of
// this same request (the realm + OIDC clients already exist from the
// earlier attempt, and its user-create response was lost). FindUserByEmail
// reports the existing user under the SAME tenant_id — the resolved
// conflict must resume WITHOUT calling the compensating teardown, since
// destroying and recreating a perfectly good realm just to dodge this
// tenant's own already-created user would be pure waste (DeleteRealmIfExistsFn
// is left nil; a call would panic the test, proving teardown never ran).
func TestProvisionDirect_CreateInvitedUserConflict_SameTenant_ResolvesWithoutTeardown(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	existingID := uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		return []string{"tender-web"}, nil
	}
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrUserEmailConflict, "email already exists in realm")
	}
	kc.FindUserByEmailFn = func(ctx context.Context, realmName, email string) (*port.ExistingUser, error) {
		assert.Equal(t, "tenant-acme", realmName)
		return &port.ExistingUser{ID: existingID, TenantID: tenantID}, nil
	}

	result, err := svc.ProvisionDirect(context.Background(), "pd-conflict-1", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.NoError(t, err)
	assert.Equal(t, "tenant-acme", result.Realm)
	assert.Equal(t, existingID, result.KeycloakUserID,
		"a resolved same-tenant conflict returns the existing admin's sub")

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err, "local commit must still land on a resolved conflict")
	assert.Equal(t, domain.RealmStrategyDedicated, row.Strategy)
	require.NotEmpty(t, pub.events)
	assert.Equal(t, existingID, pub.events[0].Data.(domain.TenantIdentityProvisionedPayload).OwnerUserID,
		"a resolved same-tenant conflict reports the existing admin as the owner")
	assert.Equal(t, domain.OpStatusSucceeded, idem.rows["pd-conflict-1"].Status)
}

// Same 409, but FindUserByEmail reports a DIFFERENT tenant_id — a genuine
// conflict. The original ErrUserEmailConflict propagates unchanged AND the
// compensating teardown still runs (this attempt's realm+clients must not
// be left behind holding no valid user).
func TestProvisionDirect_CreateInvitedUserConflict_DifferentTenant_TeardownCalled(t *testing.T) {
	svc, kc, _, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	var deletedRealm string
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		return []string{"tender-web"}, nil
	}
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrUserEmailConflict, "email already exists in realm")
	}
	kc.FindUserByEmailFn = func(ctx context.Context, realmName, email string) (*port.ExistingUser, error) {
		return &port.ExistingUser{ID: uuid.New(), TenantID: uuid.New()}, nil
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		deletedRealm = realmName
		return nil
	}

	_, err := svc.ProvisionDirect(context.Background(), "pd-conflict-2", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrUserEmailConflict, de.Code)
	assert.Equal(t, "tenant-acme", deletedRealm)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["pd-conflict-2"].Status)
}

// Teardown failures are logged, not escalated — the caller still sees the
// ORIGINAL error, not the teardown error.
func TestProvisionDirect_TeardownFailure_LoggedNotEscalated(t *testing.T) {
	svc, kc, _, _, _ := newProvisioningTestService(t)
	logger := &mockLogger{}
	svc.WithLogger(logger)
	tenantID := uuid.New()
	originalErr := domain.NewError(domain.ErrKeycloakUnavailable, "register oidc failed")
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) {
		return nil, originalErr
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		return errors.New("teardown transport error")
	}

	_, err := svc.ProvisionDirect(context.Background(), "pd-6", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
	assert.Same(t, originalErr, err)
	assert.GreaterOrEqual(t, logger.warnCalls, 1, "the teardown failure must be logged")
}

func TestProvisionDirect_IdempotentReplay(t *testing.T) {
	svc, kc, _, _, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	calls := 0
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { calls++; return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	adminID := uuid.New()
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return adminID, nil
	}

	req := service.ProvisionDirectRequest{Terms: validTerms(), TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A"}
	first, err := svc.ProvisionDirect(context.Background(), "pd-7", req)
	require.NoError(t, err)
	second, err := svc.ProvisionDirect(context.Background(), "pd-7", req)
	require.NoError(t, err)

	assert.Equal(t, 1, calls, "Keycloak must not be re-invoked on replay")
	assert.Equal(t, first, second)
	assert.Equal(t, adminID, second.KeycloakUserID,
		"the replay (answered from the ledger) carries keycloak_user_id")
	assert.Len(t, pub.events, 4)
}

// selectShard's own error path also surfaces through ProvisionDirect,
// before any Keycloak call — failIdempotency + no local row.
func TestProvisionDirect_SelectShardFailure(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	realms.CountsByShardErr = errors.New("db down")
	tenantID := uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		t.Fatal("Keycloak must not be called when shard selection fails")
		return nil
	}

	_, err := svc.ProvisionDirect(context.Background(), "pd-8", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["pd-8"].Status)
}

// A hard ledger failure from idempotency.Begin propagates immediately, no
// Keycloak call and no shard selection.
func TestProvisionDirect_BeginError_Propagates(t *testing.T) {
	svc, kc, _, idem, _ := newProvisioningTestService(t)
	idem.BeginErr = errors.New("ledger unavailable")
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		t.Fatal("Keycloak must not be called when Begin itself fails")
		return nil
	}

	_, err := svc.ProvisionDirect(context.Background(), "pd-9", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: uuid.New(), TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
}

// The local-commit tx's realms.Upsert failing (after all Keycloak steps
// succeeded) marks the idempotency key failed and enqueues no events.
func TestProvisionDirect_UpsertFails_MarksIdempotencyFailed(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	realms.UpsertErr = errors.New("db down")
	tenantID := uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}

	_, err := svc.ProvisionDirect(context.Background(), "pd-10", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["pd-10"].Status)
}

// The TenantRealmReady enqueue (the first of three) failing propagates and
// stops the remaining two from being enqueued.
func TestProvisionDirect_FirstEventEnqueueFails(t *testing.T) {
	svc, kc, _, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}
	pub.EnqueueErr = errors.New("outbox unavailable")
	pub.FailFromCall = 1

	_, err := svc.ProvisionDirect(context.Background(), "pd-11", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["pd-11"].Status)
}

// The TenantRealmReady enqueue (the second of four) failing propagates,
// even though the first (TenantIdentityProvisioned) succeeded.
func TestProvisionDirect_SecondEventEnqueueFails(t *testing.T) {
	svc, kc, _, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}
	pub.EnqueueErr = errors.New("outbox unavailable")
	pub.FailFromCall = 2

	_, err := svc.ProvisionDirect(context.Background(), "pd-12", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
	require.Len(t, pub.events, 1, "the first event enqueued before the failure is still recorded by the mock")
	assert.Equal(t, domain.EventTenantIdentityProvisioned, pub.events[0].Type)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["pd-12"].Status)
}

// The DirectPaidSignup enqueue (the third of four) failing propagates, even
// though the first two (TenantIdentityProvisioned, TenantRealmReady)
// succeeded.
func TestProvisionDirect_ThirdEventEnqueueFails(t *testing.T) {
	svc, kc, _, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}
	pub.EnqueueErr = errors.New("outbox unavailable")
	pub.FailFromCall = 3

	_, err := svc.ProvisionDirect(context.Background(), "pd-third-enqueue", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A", BillingSubscriptionID: "sub_1",
	})
	require.Error(t, err)
	require.Len(t, pub.events, 2, "the first two events enqueued before the failure are still recorded by the mock")
	assert.Equal(t, domain.EventTenantIdentityProvisioned, pub.events[0].Type)
	assert.Equal(t, domain.EventTenantRealmReady, pub.events[1].Type)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["pd-third-enqueue"].Status)
}

// The TenantConverted enqueue (the last of four) failing propagates, even
// though the first three succeeded.
func TestProvisionDirect_FourthEventEnqueueFails(t *testing.T) {
	svc, kc, _, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}
	pub.EnqueueErr = errors.New("outbox unavailable")
	pub.FailFromCall = 4

	_, err := svc.ProvisionDirect(context.Background(), "pd-fourth-enqueue", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A", BillingSubscriptionID: "sub_1",
	})
	require.Error(t, err)
	require.Len(t, pub.events, 3, "the first three events enqueued before the failure are still recorded by the mock")
	assert.Equal(t, domain.EventDirectPaidSignup, pub.events[2].Type)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["pd-fourth-enqueue"].Status)
}

// RP-2, §8.2: a tenant already offboarded (exported/deleted) must be
// rejected before any Keycloak call is made.
func TestProvisionDirect_Offboarded_Returns409(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "tenant-acme", Strategy: domain.RealmStrategyDedicated,
		Status: domain.RealmStatusDeleted,
	}
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		t.Fatal("must not call Keycloak for an already-offboarded tenant")
		return nil
	}

	_, err := svc.ProvisionDirect(context.Background(), "pd-offboarded", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrTenantOffboarded, de.Code)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["pd-offboarded"].Status)
}

// When the tx runner wires no event publisher into ctx, the local commit
// still lands and the call still succeeds — just with no events enqueued.
func TestProvisionDirect_NoEventPublisherInContext_StillCommitsLocally(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	ts := wirePlatformAutomationHappyPath(kc)
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx := newMockTxRunnerNoPublisher()
	svc := service.NewProvisioningService(realms, idem, kc, ts, tx, &mockPendingAuditEntryRepository{}, "trial", nil)
	tenantID := uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}

	result, err := svc.ProvisionDirect(context.Background(), "pd-13", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.NoError(t, err)
	assert.Equal(t, "tenant-acme", result.Realm)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStatusActive, row.Status)
}

// idempotency.Complete's own persistence failure is best-effort — logged,
// not propagated; the caller still sees a successful result.
func TestProvisionDirect_IdempotencyCompleteFailure_LoggedNotPropagated(t *testing.T) {
	svc, kc, _, idem, _ := newProvisioningTestService(t)
	logger := &mockLogger{}
	svc.WithLogger(logger)
	idem.CompleteErr = errors.New("ledger unavailable")
	tenantID := uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}

	result, err := svc.ProvisionDirect(context.Background(), "pd-14", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// ── RP-5: CreateInvitedUser ──────────────────────────────────────────────

func TestCreateInvitedUser_Success(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	userID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Strategy: domain.RealmStrategyDedicated}
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		assert.Equal(t, "tenant-acme", realmName)
		assert.Equal(t, []string{"VERIFY_EMAIL"}, req.RequiredActions)
		return userID, nil
	}

	result, err := svc.CreateInvitedUser(context.Background(), "ciu-1", tenantID, service.CreateInvitedUserRequest{
		Email: "b@acme.example", FullName: "B", RequiredActions: []string{"VERIFY_EMAIL"}, TenantSlug: "acme",
	})
	require.NoError(t, err)
	assert.Equal(t, userID, result.KeycloakUserID)
}

// A hard ledger failure from idempotency.Begin propagates immediately — no
// tenant lookup, no Keycloak call.
func TestCreateInvitedUser_BeginError_Propagates(t *testing.T) {
	svc, kc, _, idem, _ := newProvisioningTestService(t)
	idem.BeginErr = errors.New("ledger unavailable")
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		t.Fatal("Keycloak must not be called when Begin itself fails")
		return uuid.Nil, nil
	}

	_, err := svc.CreateInvitedUser(context.Background(), "ciu-6", uuid.New(), service.CreateInvitedUserRequest{Email: "x@example.com", TenantSlug: "acme"})
	require.Error(t, err)
}

func TestCreateInvitedUser_TenantNotFound(t *testing.T) {
	svc, _, _, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()

	_, err := svc.CreateInvitedUser(context.Background(), "ciu-2", tenantID, service.CreateInvitedUserRequest{Email: "x@example.com", TenantSlug: "acme"})
	require.Error(t, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["ciu-2"].Status)
}

func TestCreateInvitedUser_KeycloakFailure(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Strategy: domain.RealmStrategyDedicated}
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}

	_, err := svc.CreateInvitedUser(context.Background(), "ciu-3", tenantID, service.CreateInvitedUserRequest{Email: "x@example.com", TenantSlug: "acme"})
	require.Error(t, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["ciu-3"].Status)
}

// §9.2 lost-response-retry resolution: same shape as ProvisionTrial's, for
// RP-5's standalone CreateInvitedUser endpoint.
func TestCreateInvitedUser_EmailConflict_SameTenant_ResolvesAsIdempotentSuccess(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	existingID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Strategy: domain.RealmStrategyDedicated}
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrUserEmailConflict, "email already exists in realm")
	}
	kc.FindUserByEmailFn = func(ctx context.Context, realmName, email string) (*port.ExistingUser, error) {
		assert.Equal(t, "tenant-acme", realmName)
		return &port.ExistingUser{ID: existingID, TenantID: tenantID}, nil
	}

	result, err := svc.CreateInvitedUser(context.Background(), "ciu-conflict-1", tenantID, service.CreateInvitedUserRequest{Email: "x@example.com", TenantSlug: "acme"})
	require.NoError(t, err)
	assert.Equal(t, existingID, result.KeycloakUserID)
	assert.Equal(t, domain.OpStatusSucceeded, idem.rows["ciu-conflict-1"].Status)
}

func TestCreateInvitedUser_EmailConflict_DifferentTenant_ReturnsConflict(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Strategy: domain.RealmStrategyDedicated}
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrUserEmailConflict, "email already exists in realm")
	}
	kc.FindUserByEmailFn = func(ctx context.Context, realmName, email string) (*port.ExistingUser, error) {
		return &port.ExistingUser{ID: uuid.New(), TenantID: uuid.New()}, nil
	}

	_, err := svc.CreateInvitedUser(context.Background(), "ciu-conflict-2", tenantID, service.CreateInvitedUserRequest{Email: "x@example.com", TenantSlug: "acme"})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrUserEmailConflict, de.Code)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["ciu-conflict-2"].Status)
}

func TestCreateInvitedUser_IdempotentReplay(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Strategy: domain.RealmStrategyDedicated}
	calls := 0
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		calls++
		return uuid.New(), nil
	}

	req := service.CreateInvitedUserRequest{Email: "x@example.com", TenantSlug: "acme"}
	first, err := svc.CreateInvitedUser(context.Background(), "ciu-4", tenantID, req)
	require.NoError(t, err)
	second, err := svc.CreateInvitedUser(context.Background(), "ciu-4", tenantID, req)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, first, second)
}

// Best-effort logging: a failure to persist idempotency.Complete must not
// fail an otherwise-successful call.
func TestCreateInvitedUser_IdempotencyCompleteFailure_LoggedNotPropagated(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	logger := &mockLogger{}
	svc.WithLogger(logger)
	idem.CompleteErr = errors.New("ledger unavailable")
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Strategy: domain.RealmStrategyDedicated}
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}

	result, err := svc.CreateInvitedUser(context.Background(), "ciu-5", tenantID, service.CreateInvitedUserRequest{Email: "x@example.com", TenantSlug: "acme"})
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

func TestProvisionDirect_TombstonedTenant_Returns409(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	deletedAt := time.Now().UTC()
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared,
		Status: domain.RealmStatusDeleted, DeletedAt: &deletedAt,
	}
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error {
		t.Fatal("must not call Keycloak for a removed tenant")
		return nil
	}

	_, err := svc.ProvisionDirect(context.Background(), "pd-tombstoned", service.ProvisionDirectRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A", Plan: "pro",
	})
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrTenantOffboarded, de.Code)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["pd-tombstoned"].Status)
}
