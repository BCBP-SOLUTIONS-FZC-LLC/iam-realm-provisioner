package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

func TestProvisionDirect_Success(t *testing.T) {
	svc, kc, realms, _, pub := newProvisioningTestService(t)
	tenantID := uuid.New()
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
		return uuid.New(), nil
	}

	result, err := svc.ProvisionDirect(context.Background(), "pd-1", ProvisionDirectRequest{
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
		Plan: "pro", BillingSubscriptionID: "sub_1",
	})
	require.NoError(t, err)
	assert.Equal(t, "tenant-acme", result.Realm)
	assert.Equal(t, "dedicated", result.Strategy)
	assert.Equal(t, "kc-shard-1", result.KeycloakShard)
	assert.Equal(t, []string{"tender-web", "tender-mobile"}, result.OIDCClients)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStrategyDedicated, row.Strategy)
	require.NotNil(t, row.KeycloakShard)
	assert.Equal(t, "kc-shard-1", *row.KeycloakShard)

	require.Len(t, pub.events, 3)
	assert.Equal(t, domain.EventTenantRealmReady, pub.events[0].Type)
	assert.Equal(t, domain.EventDirectPaidSignup, pub.events[1].Type)
	assert.Equal(t, domain.EventTenantConverted, pub.events[2].Type)
	converted := pub.events[2].Data.(domain.TenantConvertedPayload)
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

	_, err := svc.ProvisionDirect(context.Background(), "pd-2", ProvisionDirectRequest{
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A", Plan: "pro",
	})
	require.NoError(t, err)
	converted := pub.events[2].Data.(domain.TenantConvertedPayload)
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

	_, err := svc.ProvisionDirect(context.Background(), "pd-3", ProvisionDirectRequest{
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

	_, err := svc.ProvisionDirect(context.Background(), "pd-4", ProvisionDirectRequest{
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

	_, err := svc.ProvisionDirect(context.Background(), "pd-5", ProvisionDirectRequest{
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
	assert.Equal(t, "tenant-acme", deletedRealm)
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

	_, err := svc.ProvisionDirect(context.Background(), "pd-6", ProvisionDirectRequest{
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
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}

	req := ProvisionDirectRequest{TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A"}
	first, err := svc.ProvisionDirect(context.Background(), "pd-7", req)
	require.NoError(t, err)
	second, err := svc.ProvisionDirect(context.Background(), "pd-7", req)
	require.NoError(t, err)

	assert.Equal(t, 1, calls, "Keycloak must not be re-invoked on replay")
	assert.Equal(t, first, second)
	assert.Len(t, pub.events, 3)
}

// RP-2 shard placement: selectShard picks the least-loaded shard, breaking
// ties lexicographically, reading counts from the local mirror only.
func TestSelectShard_LeastLoadedWithLexicographicTieBreak(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, _ := newMockTxRunner()
	svc := NewProvisioningService(realms, idem, kc, tx, "trial", []string{"kc-shard-2", "kc-shard-1", "kc-shard-3"})

	seedDedicated := func(shard string) {
		id := uuid.New()
		realms.rows[id] = &domain.TenantRealm{TenantID: id, Strategy: domain.RealmStrategyDedicated, KeycloakShard: &shard}
	}
	// shard-1: 2, shard-2: 1, shard-3: 1 -> least-loaded tie between 2 & 3,
	// lexicographic tie-break picks "kc-shard-2".
	seedDedicated("kc-shard-1")
	seedDedicated("kc-shard-1")
	seedDedicated("kc-shard-2")
	seedDedicated("kc-shard-3")

	got, err := svc.selectShard(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "kc-shard-2", got)
}

// With no load recorded anywhere, every shard ties at count zero, so the
// lexicographic tie-break alone decides — independent of shardIDs order.
func TestSelectShard_NoLoadYet_TieBreaksLexicographically(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, _ := newMockTxRunner()
	svc := NewProvisioningService(realms, idem, kc, tx, "trial", []string{"kc-shard-9", "kc-shard-1"})

	got, err := svc.selectShard(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "kc-shard-1", got)
}

func TestSelectShard_CountsByShardFailure_Propagates(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	realms.CountsByShardErr = errors.New("db down")
	idem := newMockIdempotencyStore()
	tx, _ := newMockTxRunner()
	svc := NewProvisioningService(realms, idem, kc, tx, "trial", nil)

	_, err := svc.selectShard(context.Background())
	require.Error(t, err)
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

	_, err := svc.ProvisionDirect(context.Background(), "pd-8", ProvisionDirectRequest{
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

	_, err := svc.ProvisionDirect(context.Background(), "pd-9", ProvisionDirectRequest{
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

	_, err := svc.ProvisionDirect(context.Background(), "pd-10", ProvisionDirectRequest{
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

	_, err := svc.ProvisionDirect(context.Background(), "pd-11", ProvisionDirectRequest{
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["pd-11"].Status)
}

// The DirectPaidSignup enqueue (the second of three) failing propagates,
// even though the first (TenantRealmReady) succeeded.
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

	_, err := svc.ProvisionDirect(context.Background(), "pd-12", ProvisionDirectRequest{
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.Error(t, err)
	require.Len(t, pub.events, 1, "the first event enqueued before the failure is still recorded by the mock")
	assert.Equal(t, domain.EventTenantRealmReady, pub.events[0].Type)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["pd-12"].Status)
}

// When the tx runner wires no event publisher into ctx, the local commit
// still lands and the call still succeeds — just with no events enqueued.
func TestProvisionDirect_NoEventPublisherInContext_StillCommitsLocally(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx := newMockTxRunnerNoPublisher()
	svc := NewProvisioningService(realms, idem, kc, tx, "trial", nil)
	tenantID := uuid.New()
	kc.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, v string) error { return nil }
	kc.RegisterOIDCClientsFn = func(ctx context.Context, realmName, slug string) ([]string, error) { return nil, nil }
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}

	result, err := svc.ProvisionDirect(context.Background(), "pd-13", ProvisionDirectRequest{
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

	result, err := svc.ProvisionDirect(context.Background(), "pd-14", ProvisionDirectRequest{
		TenantID: tenantID, TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
	})
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

func TestNewProvisioningService_Defaults(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, _ := newMockTxRunner()
	svc := NewProvisioningService(realms, idem, kc, tx, "", nil)
	assert.Equal(t, "trial", svc.sharedRealm)
	assert.Equal(t, []string{"kc-shard-1"}, svc.shardIDs)
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

	result, err := svc.CreateInvitedUser(context.Background(), "ciu-1", tenantID, CreateInvitedUserRequest{
		Email: "b@acme.example", FullName: "B", RequiredActions: []string{"VERIFY_EMAIL"},
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

	_, err := svc.CreateInvitedUser(context.Background(), "ciu-6", uuid.New(), CreateInvitedUserRequest{Email: "x@example.com"})
	require.Error(t, err)
}

func TestCreateInvitedUser_TenantNotFound(t *testing.T) {
	svc, _, _, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()

	_, err := svc.CreateInvitedUser(context.Background(), "ciu-2", tenantID, CreateInvitedUserRequest{Email: "x@example.com"})
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

	_, err := svc.CreateInvitedUser(context.Background(), "ciu-3", tenantID, CreateInvitedUserRequest{Email: "x@example.com"})
	require.Error(t, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["ciu-3"].Status)
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

	req := CreateInvitedUserRequest{Email: "x@example.com"}
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

	result, err := svc.CreateInvitedUser(context.Background(), "ciu-5", tenantID, CreateInvitedUserRequest{Email: "x@example.com"})
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}
