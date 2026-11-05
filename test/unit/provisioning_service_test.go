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

func newProvisioningTestService(t *testing.T) (*service.ProvisioningService, *mockKeycloakAdmin, *mockTenantRealmRepository, *mockIdempotencyStore, *mockEventPublisher) {
	t.Helper()
	kc := &mockKeycloakAdmin{}
	ts := wirePlatformAutomationHappyPath(kc)
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, pub := newMockTxRunner()
	svc := service.NewProvisioningService(realms, idem, kc, ts, tx, &mockPendingAuditEntryRepository{}, "trial", nil).WithShardLoads(realms)
	return svc, kc, realms, idem, pub
}

func TestProvisionTrial_Success(t *testing.T) {
	svc, kc, realms, _, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	kcUserID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		assert.Equal(t, tenantID, req.TenantID)
		assert.Equal(t, "owner@acme.example", req.Email)
		return kcUserID, nil
	}

	result, err := svc.ProvisionTrial(context.Background(), "key-1", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, TenantName: "Acme", TenantSlug: "acme",
		AdminEmail: "owner@acme.example", AdminName: "Dana Owner", Plan: "pro",
	})

	require.NoError(t, err)
	assert.Equal(t, tenantID, result.TenantID)
	assert.Equal(t, "trial", result.Realm)
	assert.Equal(t, "shared", result.Strategy)
	assert.Equal(t, kcUserID, result.KeycloakUserID)
	assert.True(t, result.EmailVerificationSent)

	// Local commit landed.
	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStrategyShared, row.Strategy)
	assert.Equal(t, domain.RealmStatusActive, row.Status)
	require.NotNil(t, row.TenantSlug, "RP-D20: RP-3 later names the realm from it")
	assert.Equal(t, "acme", *row.TenantSlug)

	// Exactly one TenantIdentityProvisioned event (the BFF-2 signup
	// backstop), carrying the request's tenant fields verbatim.
	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTenantIdentityProvisioned, pub.events[0].Type)
	identity, ok := pub.events[0].Data.(domain.TenantIdentityProvisionedPayload)
	require.True(t, ok)
	assert.Equal(t, domain.TenantIdentityProvisionedPayload{
		TenantID: tenantID, OwnerUserID: kcUserID, Slug: "acme", Name: "Acme", Plan: "pro",
		SignupType: "trial", Realm: "trial", RealmType: "shared",
	}, identity)
	assert.Equal(t, domain.SystemActorID, pub.events[0].Actor)
}

// Keycloak-effect-first (§9.1): a Keycloak failure must NOT leave a local
// tenant_realms row, and must mark the idempotency key failed (not
// succeeded) so a retry with the same key re-executes rather than replaying.
func TestProvisionTrial_KeycloakFailure_NoLocalCommit(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-2", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)

	_, findErr := realms.FindByTenantID(context.Background(), tenantID)
	assert.Error(t, findErr, "no local row should exist after a Keycloak failure")
	assert.Empty(t, pub.events, "no event should be enqueued after a Keycloak failure")

	row := idem.rows["key-2"]
	require.NotNil(t, row)
	assert.Equal(t, domain.OpStatusFailed, row.Status)
}

// §2.5/RP-17: RP-1 mints into the SHARED trial realm using a tenant-scoped
// clientId ("platform-automation-<tenant_id>"), never the literal base
// name — a shared realm cannot hold two clients named identically.
func TestProvisionTrial_MintPlatformAutomation_UsesTenantScopedClientID(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) { return uuid.New(), nil }
	var gotRealm, gotClientID string
	kc.CreateServiceAccountClientFn = func(ctx context.Context, realmName, clientID, jwksURL string) (uuid.UUID, bool, error) {
		gotRealm, gotClientID = realmName, clientID
		return uuid.New(), true, nil
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-mint-1", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.NoError(t, err)
	assert.Equal(t, "trial", gotRealm)
	assert.Equal(t, "platform-automation-"+tenantID.String(), gotClientID)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	require.NotNil(t, row.ServiceAccountPrincipalID)
	require.NotNil(t, row.ServiceAccountKeycloakClientID)
	assert.Equal(t, "platform-automation-"+tenantID.String(), *row.ServiceAccountKeycloakClientID)
}

// A mint failure at the CreateServiceAccountClient step itself has nothing
// to tear down (no Keycloak client was ever minted, only the shared trial
// user) — same no-teardown failure branch as a CreateTrialUser failure.
func TestProvisionTrial_MintPlatformAutomationFails_NoLocalCommit(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) { return uuid.New(), nil }
	kc.CreateServiceAccountClientFn = func(ctx context.Context, realmName, clientID, jwksURL string) (uuid.UUID, bool, error) {
		return uuid.Nil, false, domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}
	kc.DeleteServiceAccountClientIfExistsFn = func(ctx context.Context, realmName, clientID string) error {
		t.Fatal("no client was ever minted, so no cleanup should be attempted")
		return nil
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-mint-2", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)

	_, findErr := realms.FindByTenantID(context.Background(), tenantID)
	assert.Error(t, findErr, "no local row should exist after a mint failure")
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["key-mint-2"].Status)
}

// Unlike ProvisionDirect/Convert, RP-1 has no realm-level compensating
// teardown to fall back on (the shared trial realm is never deleted) — so
// if the Keycloak client mint itself succeeds but the following Token
// Service call (TS-4 register or TS-1 issue) fails, mintPlatformAutomation
// must itself clean up the now-orphaned client, or it is left live in the
// shared realm indefinitely with no local record to reconcile it.
func TestProvisionTrial_MintPlatformAutomation_RegisterPrincipalFails_OrphanedClientCleanedUp(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	ts := wirePlatformAutomationHappyPath(kc)
	ts.RegisterPrincipalFn = func(ctx context.Context, tid uuid.UUID, req port.RegisterPrincipalRequest) (*port.RegisterPrincipalResult, error) {
		return nil, domain.NewError(domain.ErrTokenServiceUnavailable, "boom")
	}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, _ := newMockTxRunner()
	svc := service.NewProvisioningService(realms, idem, kc, ts, tx, &mockPendingAuditEntryRepository{}, "trial", nil).WithShardLoads(realms)

	tenantID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) { return uuid.New(), nil }
	var deletedRealm, deletedClientID string
	kc.DeleteServiceAccountClientIfExistsFn = func(ctx context.Context, realmName, clientID string) error {
		deletedRealm, deletedClientID = realmName, clientID
		return nil
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-mint-3", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)
	assert.Equal(t, "trial", deletedRealm)
	assert.Equal(t, "platform-automation-"+tenantID.String(), deletedClientID)
}

// A RESUMED client (CreateServiceAccountClient's created=false — an earlier
// attempt already minted it) must never be deleted on a later Token
// Service failure: it predates this call and may already be correctly
// registered and actively serving traffic, so treating a later, unrelated
// failure as grounds to delete it risks breaking a working credential
// rather than just cleaning up an orphan.
func TestProvisionTrial_MintPlatformAutomation_RegisterPrincipalFails_ResumedClientNotDeleted(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	ts := wirePlatformAutomationHappyPath(kc)
	kc.CreateServiceAccountClientFn = func(ctx context.Context, realmName, clientID, jwksURL string) (uuid.UUID, bool, error) {
		return uuid.New(), false, nil // resumed, not freshly created
	}
	ts.RegisterPrincipalFn = func(ctx context.Context, tid uuid.UUID, req port.RegisterPrincipalRequest) (*port.RegisterPrincipalResult, error) {
		return nil, domain.NewError(domain.ErrTokenServiceUnavailable, "boom")
	}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, _ := newMockTxRunner()
	svc := service.NewProvisioningService(realms, idem, kc, ts, tx, &mockPendingAuditEntryRepository{}, "trial", nil).WithShardLoads(realms)

	tenantID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) { return uuid.New(), nil }
	kc.DeleteServiceAccountClientIfExistsFn = func(ctx context.Context, realmName, clientID string) error {
		t.Fatal("a resumed (not freshly-created) client must never be deleted on a later failure")
		return nil
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-mint-4", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)
}

// The same created-gated cleanup applies to the LATER Token Service call
// (TS-1 IssueOrRotateCredential) — a fresh client is cleaned up if it
// fails after RegisterPrincipal (TS-4) already succeeded.
// TestProvisionTrial_MintPlatformAutomation_IssueOrRotateCredentialFails_ClientRetainedAfterTS4Success
// verifies that when TS-4 (RegisterPrincipal) succeeds but TS-1
// (IssueOrRotateCredential) fails, the Keycloak service-account client is
// NOT deleted. Deleting it would leave Token Service with an orphaned principal
// record pointing to a gone client. The idempotent retry path self-heals:
//  1. CreateServiceAccountClient → created=false (client still exists)
//  2. RegisterPrincipal (TS-4) → idempotent replay
//  3. IssueOrRotateCredential (TS-1) → succeeds on retry
func TestProvisionTrial_MintPlatformAutomation_IssueOrRotateCredentialFails_ClientRetainedAfterTS4Success(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	ts := wirePlatformAutomationHappyPath(kc)
	ts.IssueOrRotateCredentialFn = func(ctx context.Context, tid, principalID uuid.UUID, req port.IssueCredentialRequest) (*port.IssueCredentialResult, error) {
		return nil, domain.NewError(domain.ErrTokenServiceUnavailable, "boom")
	}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, _ := newMockTxRunner()
	svc := service.NewProvisioningService(realms, idem, kc, ts, tx, &mockPendingAuditEntryRepository{}, "trial", nil).WithShardLoads(realms)

	tenantID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) { return uuid.New(), nil }
	kc.DeleteServiceAccountClientIfExistsFn = func(ctx context.Context, realmName, clientID string) error {
		t.Fatal("client must NOT be deleted when TS-4 (RegisterPrincipal) already succeeded — " +
			"the Token Service has a record of this client; a retry self-heals via idempotent replay")
		return nil
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-mint-5", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)
}

// A RESUMED client must never be deleted on an IssueOrRotateCredential
// failure either — same created-gating as the RegisterPrincipal case.
func TestProvisionTrial_MintPlatformAutomation_IssueOrRotateCredentialFails_ResumedClientNotDeleted(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	ts := wirePlatformAutomationHappyPath(kc)
	kc.CreateServiceAccountClientFn = func(ctx context.Context, realmName, clientID, jwksURL string) (uuid.UUID, bool, error) {
		return uuid.New(), false, nil // resumed, not freshly created
	}
	ts.IssueOrRotateCredentialFn = func(ctx context.Context, tid, principalID uuid.UUID, req port.IssueCredentialRequest) (*port.IssueCredentialResult, error) {
		return nil, domain.NewError(domain.ErrTokenServiceUnavailable, "boom")
	}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, _ := newMockTxRunner()
	svc := service.NewProvisioningService(realms, idem, kc, ts, tx, &mockPendingAuditEntryRepository{}, "trial", nil).WithShardLoads(realms)

	tenantID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) { return uuid.New(), nil }
	kc.DeleteServiceAccountClientIfExistsFn = func(ctx context.Context, realmName, clientID string) error {
		t.Fatal("a resumed (not freshly-created) client must never be deleted on a later failure")
		return nil
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-mint-6", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)
}

// §9.2 lost-response-retry resolution: CreateTrialUser 409s (the original
// create actually succeeded but its response never reached the caller, and
// a retry of the same request hits Keycloak's email-uniqueness
// constraint). FindUserByEmail reports the existing user under the SAME
// tenant_id this request carries — safe to resume as if CreateTrialUser had
// returned that id directly, no failure at all.
func TestProvisionTrial_EmailConflict_SameTenant_ResolvesAsIdempotentSuccess(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	existingID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrUserEmailConflict, "email already exists in realm")
	}
	kc.FindUserByEmailFn = func(ctx context.Context, realmName, email string) (*port.ExistingUser, error) {
		assert.Equal(t, "trial", realmName)
		assert.Equal(t, "owner@acme.example", email)
		return &port.ExistingUser{ID: existingID, TenantID: tenantID}, nil
	}

	result, err := svc.ProvisionTrial(context.Background(), "key-conflict-1", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "owner@acme.example", AdminName: "Dana Owner",
	})
	require.NoError(t, err)
	assert.Equal(t, existingID, result.KeycloakUserID)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err, "local commit must still land on a resolved conflict")
	assert.Equal(t, domain.RealmStatusActive, row.Status)
	require.Len(t, pub.events, 1)
	assert.Equal(t, existingID, pub.events[0].Data.(domain.TenantIdentityProvisionedPayload).OwnerUserID,
		"the resolved existing user is the tenant's owner")
	assert.Equal(t, domain.OpStatusSucceeded, idem.rows["key-conflict-1"].Status, "a resolved conflict must complete as a success, not a failed operation")
}

// Same 409, but FindUserByEmail reports a DIFFERENT tenant_id — a genuine
// conflict, not this caller's own retry. The original ErrUserEmailConflict
// must propagate unchanged (a real 409, not a masked/wrong error) and the
// idempotency key must be marked failed so a legitimate retry can still
// re-attempt with a different email.
func TestProvisionTrial_EmailConflict_DifferentTenant_ReturnsConflict(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrUserEmailConflict, "email already exists in realm")
	}
	kc.FindUserByEmailFn = func(ctx context.Context, realmName, email string) (*port.ExistingUser, error) {
		return &port.ExistingUser{ID: uuid.New(), TenantID: uuid.New()}, nil
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-conflict-2", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "owner@acme.example", AdminName: "Dana Owner",
	})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrUserEmailConflict, de.Code)

	_, findErr := realms.FindByTenantID(context.Background(), tenantID)
	assert.Error(t, findErr, "no local row on a genuine conflict")
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["key-conflict-2"].Status)
}

// A caller reaching the conflict resolver with an unset TenantID (uuid.Nil
// — should never happen given real request binding, but the resolver must
// not silently trust it) must never match an attribute-less legacy user
// FindUserByEmail happens to return (also uuid.Nil, since it carries no
// tenant_id attribute) — that would hand back an unrelated stranger's
// Keycloak id as if it were this "tenant"'s own. FindUserByEmailFn is left
// unset here deliberately: if the nil-guard were ever removed, the mock
// would panic on the call this test proves never happens.
func TestProvisionTrial_EmailConflict_NilTenantID_NeverLooksUpOrMatches(t *testing.T) {
	svc, kc, _, idem, _ := newProvisioningTestService(t)
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrUserEmailConflict, "email already exists in realm")
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-conflict-nil", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: uuid.Nil, AdminEmail: "owner@acme.example", AdminName: "Dana Owner",
	})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrUserEmailConflict, de.Code)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["key-conflict-nil"].Status)
}

// If FindUserByEmail itself fails (Keycloak flaky right when we're trying
// to disambiguate), the caller must still see the ORIGINAL conflict error —
// not the unrelated lookup failure, which would be a confusing regression
// from today's (masked) behavior.
func TestProvisionTrial_EmailConflict_LookupFails_ReturnsOriginalConflict(t *testing.T) {
	svc, kc, _, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrUserEmailConflict, "email already exists in realm")
	}
	kc.FindUserByEmailFn = func(ctx context.Context, realmName, email string) (*port.ExistingUser, error) {
		return nil, domain.NewError(domain.ErrKeycloakUnavailable, "flaky")
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-conflict-3", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "owner@acme.example", AdminName: "Dana Owner",
	})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrUserEmailConflict, de.Code, "the original conflict must surface, not the lookup failure")
	assert.Equal(t, domain.OpStatusFailed, idem.rows["key-conflict-3"].Status)
}

// Idempotent replay (§9.2): a second call with the same key must return the
// exact stored result without calling Keycloak again.
func TestProvisionTrial_IdempotentReplay(t *testing.T) {
	svc, kc, _, _, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	kcUserID := uuid.New()
	calls := 0
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		calls++
		return kcUserID, nil
	}

	first, err := svc.ProvisionTrial(context.Background(), "key-3", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.NoError(t, err)

	second, err := svc.ProvisionTrial(context.Background(), "key-3", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.NoError(t, err)

	assert.Equal(t, 1, calls, "Keycloak must be called exactly once — the replay must not re-execute")
	assert.Equal(t, first, second)
	assert.Len(t, pub.events, 1, "only one event should ever be enqueued across both calls")
}

// A duplicate call still in_progress (concurrent request) must surface as
// 409 operation_in_progress, not silently succeed or re-execute.
func TestProvisionTrial_ConcurrentDuplicate_ReturnsConflict(t *testing.T) {
	svc, kc, _, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		t.Fatal("Keycloak must not be called for an in_progress duplicate")
		return uuid.Nil, nil
	}
	// Simulate a concurrent call already having begun this key.
	idem.rows["key-4"] = &domain.ProvisioningOperation{IdempotencyKey: "key-4", TenantID: tenantID, Status: domain.OpStatusInProgress}

	_, err := svc.ProvisionTrial(context.Background(), "key-4", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)
	var de *domain.Error
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrOperationInProgress, de.Code)
	assert.Equal(t, 409, de.Status())
}

// idempotency.Complete's own persistence failure is best-effort — logged,
// not propagated; the caller still sees a successful result.
func TestProvisionTrial_IdempotencyCompleteFailure_LoggedNotPropagated(t *testing.T) {
	svc, kc, _, idem, _ := newProvisioningTestService(t)
	logger := &mockLogger{}
	svc.WithLogger(logger)
	idem.CompleteErr = errors.New("ledger unavailable")
	tenantID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) { return uuid.New(), nil }

	result, err := svc.ProvisionTrial(context.Background(), "key-5", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// failIdempotency's own persistence failure is likewise best-effort — the
// caller still sees the ORIGINAL Keycloak error, not a ledger error.
func TestProvisionTrial_FailIdempotencyPersistFailure_LoggedNotEscalated(t *testing.T) {
	svc, kc, _, idem, _ := newProvisioningTestService(t)
	logger := &mockLogger{}
	svc.WithLogger(logger)
	idem.FailErr = errors.New("ledger unavailable")
	tenantID := uuid.New()
	originalErr := domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, originalErr
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-6", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)
	assert.Same(t, originalErr, err)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// A hard ledger failure from idempotency.Begin itself (distinct from a
// `found` row, which is not an error) must propagate immediately — no
// Keycloak call, no local commit.
func TestProvisionTrial_BeginError_Propagates(t *testing.T) {
	svc, kc, _, idem, _ := newProvisioningTestService(t)
	idem.BeginErr = errors.New("ledger unavailable")
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		t.Fatal("Keycloak must not be called when Begin itself fails")
		return uuid.Nil, nil
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-7", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: uuid.New(), AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)
}

// RP-1, §8.1: a tenant already offboarded (exported/deleted) must be
// rejected before any Keycloak call is made — re-provisioning a terminal
// tenant is never allowed.
func TestProvisionTrial_Offboarded_Returns409(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared,
		Status: domain.RealmStatusExported,
	}
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		t.Fatal("must not call Keycloak for an already-offboarded tenant")
		return uuid.Nil, nil
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-offboarded", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrTenantOffboarded, de.Code)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["key-offboarded"].Status)
}

// The outbox enqueue failing inside the local-commit tx (after Upsert
// already succeeded) marks the idempotency key failed and returns the
// publisher error — mirrors Convert's TestConvert_FirstEventEnqueueFails.
func TestProvisionTrial_EventEnqueueFails_MarksIdempotencyFailed(t *testing.T) {
	svc, kc, _, idem, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) { return uuid.New(), nil }
	pub.EnqueueErr = errors.New("outbox unavailable")
	pub.FailFromCall = 1

	_, err := svc.ProvisionTrial(context.Background(), "key-enqueue-fail", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["key-enqueue-fail"].Status)
}

// The local-commit tx's realms.Upsert failing (after a successful Keycloak
// call) marks the idempotency key failed and returns the repository error —
// no event is enqueued.
func TestProvisionTrial_UpsertFails_MarksIdempotencyFailed(t *testing.T) {
	svc, kc, realms, idem, pub := newProvisioningTestService(t)
	realms.UpsertErr = errors.New("db down")
	tenantID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) { return uuid.New(), nil }

	_, err := svc.ProvisionTrial(context.Background(), "key-8", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["key-8"].Status)
}

// When the tx runner wires no event publisher into ctx (the
// EventPublisherFromContext !ok short-circuit), the local commit still
// lands and the call still succeeds — just with no event enqueued.
func TestProvisionTrial_NoEventPublisherInContext_StillCommitsLocally(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	ts := wirePlatformAutomationHappyPath(kc)
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx := newMockTxRunnerNoPublisher()
	svc := service.NewProvisioningService(realms, idem, kc, ts, tx, &mockPendingAuditEntryRepository{}, "trial", nil).WithShardLoads(realms)
	tenantID := uuid.New()
	kcUserID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) { return kcUserID, nil }

	result, err := svc.ProvisionTrial(context.Background(), "key-9", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.NoError(t, err)
	assert.Equal(t, kcUserID, result.KeycloakUserID)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStatusActive, row.Status)
}

// A stored idempotency result that fails to decode as JSON (corrupted
// ledger row) surfaces as a decode error, not a silent empty result.
func TestReplayOrConflict_CorruptedStoredResult_DecodeError(t *testing.T) {
	svc, kc, _, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	idem.rows["key-corrupt"] = &domain.ProvisioningOperation{
		IdempotencyKey: "key-corrupt", TenantID: tenantID, Status: domain.OpStatusSucceeded,
		Result: []byte("{not valid json"),
	}
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		t.Fatal("a replay must not call Keycloak")
		return uuid.Nil, nil
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-corrupt", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode stored result")
}

// A tenant_id whose row trial-hard-delete or orphaned-signup-reap tombstoned
// is hidden from FindByTenantID, and its idempotency ledger was scrubbed, so
// only the tombstone check stops RP-1 from re-creating the Keycloak user
// (§8.1, LLD v1.38).
func TestProvisionTrial_TombstonedTenant_Returns409(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	deletedAt := time.Now().UTC()
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared,
		Status: domain.RealmStatusDeleted, DeletedAt: &deletedAt,
	}
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		t.Fatal("must not call Keycloak for a removed tenant")
		return uuid.Nil, nil
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-tombstoned", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X", Plan: "starter",
	})
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrTenantOffboarded, de.Code)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["key-tombstoned"].Status)
	assert.NotNil(t, realms.rows[tenantID].DeletedAt, "the tombstone stays")
}

// The guard fails closed: if the tombstone lookup itself fails, RP-1 does
// not proceed to Keycloak on the assumption that the tenant is new.
func TestProvisionTrial_TombstoneLookupFails_NoKeycloakCall(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	realms.IsTombstonedErr = errors.New("db down")
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		t.Fatal("must not call Keycloak when the tombstone check failed")
		return uuid.Nil, nil
	}

	_, err := svc.ProvisionTrial(context.Background(), "key-tombstone-err", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: uuid.New(), AdminEmail: "x@example.com", AdminName: "X", Plan: "starter",
	})
	require.Error(t, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["key-tombstone-err"].Status)
}

// C3 — retry-after-postgres-fail (§9.2): a failed idempotency key (caused by
// realms.Upsert failing in the local-commit tx while Keycloak had already
// created the user) is reclaimed on the second call with the same key. The
// second attempt must succeed via resolveUserCreateConflict (the user already
// exists in Keycloak from the first attempt), and CreateTrialUser must be
// called AT MOST twice total — never a third time that would signal a spurious
// duplicate creation.
func TestProvisionTrial_UpsertFails_ThenRetrySameKey_KeycloakNotCalledTwice(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	existingID := uuid.New()

	// First attempt: Keycloak succeeds, Postgres Upsert fails.
	realms.UpsertErr = errors.New("db down")
	createCalls := 0
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		createCalls++
		return existingID, nil
	}

	_, firstErr := svc.ProvisionTrial(context.Background(), "c3-retry-key", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "owner@acme.example", AdminName: "Dana Owner", Plan: "pro",
	})
	require.Error(t, firstErr, "first attempt must fail because Upsert failed")
	assert.Equal(t, domain.OpStatusFailed, idem.rows["c3-retry-key"].Status,
		"failed Upsert must mark the idempotency key failed so a retry re-executes")
	assert.Equal(t, 1, createCalls, "CreateTrialUser must be called exactly once on the first attempt")

	// Second attempt: Postgres heals; Keycloak 409s because the user already
	// exists from the first attempt. The retry must resolve the existing user
	// via resolveUserCreateConflict, NOT create a duplicate.
	realms.UpsertErr = nil
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		createCalls++
		return uuid.Nil, domain.NewError(domain.ErrUserEmailConflict, "email already exists in realm")
	}
	kc.FindUserByEmailFn = func(ctx context.Context, realmName, email string) (*port.ExistingUser, error) {
		assert.Equal(t, "trial", realmName)
		assert.Equal(t, "owner@acme.example", email)
		return &port.ExistingUser{ID: existingID, TenantID: tenantID}, nil
	}

	result, secondErr := svc.ProvisionTrial(context.Background(), "c3-retry-key", service.ProvisionTrialRequest{
		Terms:    validTerms(),
		TenantID: tenantID, AdminEmail: "owner@acme.example", AdminName: "Dana Owner", Plan: "pro",
	})
	require.NoError(t, secondErr, "second attempt must succeed via conflict resolution")
	assert.Equal(t, existingID, result.KeycloakUserID,
		"the resolved existing user is returned as the owner, not a newly created one")
	assert.Equal(t, domain.OpStatusSucceeded, idem.rows["c3-retry-key"].Status)

	// Critical: CreateTrialUser called at most twice across both attempts.
	// A third call would indicate spurious duplicate-user creation on the retry.
	assert.LessOrEqual(t, createCalls, 2,
		"CreateTrialUser must be called at most once per attempt — a third call indicates spurious duplicate-user creation")
}
