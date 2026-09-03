package unit_test

import (
	"context"
	"errors"
	"testing"

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
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, pub := newMockTxRunner()
	svc := service.NewProvisioningService(realms, idem, kc, tx, "trial", nil)
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

	// Exactly one TrialTenantProvisioned event, with the frozen payload shape.
	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTrialTenantProvisioned, pub.events[0].Type)
	payload, ok := pub.events[0].Data.(domain.TrialTenantProvisionedPayload)
	require.True(t, ok)
	assert.Equal(t, tenantID, payload.TenantID)
	assert.Equal(t, "trial", payload.Realm)
	assert.Equal(t, kcUserID, payload.KeycloakUserID)
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
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.NoError(t, err)

	second, err := svc.ProvisionTrial(context.Background(), "key-3", service.ProvisionTrialRequest{
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
		TenantID: uuid.New(), AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)
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
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx := newMockTxRunnerNoPublisher()
	svc := service.NewProvisioningService(realms, idem, kc, tx, "trial", nil)
	tenantID := uuid.New()
	kcUserID := uuid.New()
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) { return kcUserID, nil }

	result, err := svc.ProvisionTrial(context.Background(), "key-9", service.ProvisionTrialRequest{
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
		TenantID: tenantID, AdminEmail: "x@example.com", AdminName: "X",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode stored result")
}
