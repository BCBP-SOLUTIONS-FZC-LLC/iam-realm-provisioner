package service

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
)

func newLifecycleTestService() (*LifecycleService, *mockKeycloakAdmin, *mockTenantRealmRepository, *mockEventPublisher) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, pub := newMockTxRunner()
	svc := NewLifecycleService(realms, idem, kc, tx, "trial")
	return svc, kc, realms, pub
}

func newLifecycleTestServiceWithIdem() (*LifecycleService, *mockKeycloakAdmin, *mockTenantRealmRepository, *mockIdempotencyStore, *mockEventPublisher) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, pub := newMockTxRunner()
	svc := NewLifecycleService(realms, idem, kc, tx, "trial")
	return svc, kc, realms, idem, pub
}

// RP-INV-5: offboarded is terminal — Suspend must reject it.
func TestSuspend_RejectsOffboarded(t *testing.T) {
	svc, _, realms, _ := newLifecycleTestService()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusDeleted}

	_, err := svc.Suspend(context.Background(), "sus-1", tenantID, "abuse")
	require.Error(t, err)
	de := err.(*domain.Error)
	assert.Equal(t, domain.ErrTenantOffboarded, de.Code)
}

// RP-14: dedicated tenants disable the realm; shared-trial tenants disable
// only that tenant's users, NEVER the shared realm (RP-INV-2).
func TestSuspend_SharedVsDedicatedBranch(t *testing.T) {
	svc, kc, realms, pub := newLifecycleTestService()

	dedicatedID := uuid.New()
	shard := "kc-shard-1"
	realms.rows[dedicatedID] = &domain.TenantRealm{TenantID: dedicatedID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, KeycloakShard: &shard, Status: domain.RealmStatusActive}
	var disabledRealm string
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error { disabledRealm = realmName; return nil }
	_, err := svc.Suspend(context.Background(), "sus-2", dedicatedID, "fraud")
	require.NoError(t, err)
	assert.Equal(t, "tenant-x", disabledRealm)
	require.Len(t, pub.events, 1)
	payload := pub.events[0].Data.(domain.TenantSuspendedPayload)
	assert.Equal(t, domain.SuspensionSourceOperator, payload.Source)

	sharedID := uuid.New()
	realms.rows[sharedID] = &domain.TenantRealm{TenantID: sharedID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusActive}
	var disabledUsersRealm string
	var disabledUsersTenant uuid.UUID
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error {
		disabledUsersRealm, disabledUsersTenant = realmName, tid
		return nil
	}
	_, err = svc.Suspend(context.Background(), "sus-3", sharedID, "fraud")
	require.NoError(t, err)
	assert.Equal(t, "trial", disabledUsersRealm, "the shared realm itself must never be disabled")
	assert.Equal(t, sharedID, disabledUsersTenant)
}

// Idempotent: suspending an already-suspended tenant is a no-op replay,
// not a second Keycloak call.
func TestSuspend_AlreadySuspended_NoOp(t *testing.T) {
	svc, kc, realms, pub := newLifecycleTestService()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusSuspended}
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error {
		t.Fatal("must not call Keycloak for an already-suspended tenant")
		return nil
	}
	result, err := svc.Suspend(context.Background(), "sus-4", tenantID, "x")
	require.NoError(t, err)
	assert.Equal(t, "suspended", result.Status)
	assert.Empty(t, pub.events)
}

func TestReactivate_RejectsOffboarded(t *testing.T) {
	svc, _, realms, _ := newLifecycleTestService()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusExported}

	_, err := svc.Reactivate(context.Background(), "react-1", tenantID, domain.ReactivationSourceOperator, "", nil)
	require.Error(t, err)
	de := err.(*domain.Error)
	assert.Equal(t, domain.ErrTenantOffboarded, de.Code)
}

// Reactivating a suspended DEDICATED tenant (regardless of whether it was
// billing-lapse or RP-14 operator suspended) must emit TenantReactivated
// with source=operator on this endpoint (RP-D13) and re-enable the realm.
func TestReactivate_DedicatedTenant_EmitsOperatorSourcedEvent(t *testing.T) {
	svc, kc, realms, pub := newLifecycleTestService()
	tenantID := uuid.New()
	shard := "kc-shard-1"
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, KeycloakShard: &shard, Status: domain.RealmStatusSuspended}
	var enabledRealm string
	kc.EnableRealmFn = func(ctx context.Context, realmName string) error { enabledRealm = realmName; return nil }

	result, err := svc.Reactivate(context.Background(), "react-2", tenantID, domain.ReactivationSourceOperator, "resolved", nil)
	require.NoError(t, err)
	assert.Equal(t, "active", result.Status)
	assert.Equal(t, "tenant-x", enabledRealm)
	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTenantReactivated, pub.events[0].Type)
	payload := pub.events[0].Data.(domain.TenantReactivatedPayload)
	assert.Equal(t, domain.ReactivationSourceOperator, payload.Source)
}

func TestExpireTrial_DisablesUsersAndMarksRow(t *testing.T) {
	svc, kc, realms, pub := newLifecycleTestService()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusActive}
	var disabledTenant uuid.UUID
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error { disabledTenant = tid; return nil }

	_, err := svc.ExpireTrial(context.Background(), "exp-1", tenantID, 30)
	require.NoError(t, err)
	assert.Equal(t, tenantID, disabledTenant)
	row, _ := realms.FindByTenantID(context.Background(), tenantID)
	assert.Equal(t, domain.RealmStatusDisabled, row.Status)
	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTrialExpired, pub.events[0].Type)
}

func TestDeleteUser_Idempotent(t *testing.T) {
	svc, kc, realms, _ := newLifecycleTestService()
	tenantID := uuid.New()
	userID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	calls := 0
	kc.DeleteUserFn = func(ctx context.Context, realmName string, uid uuid.UUID) error {
		calls++
		assert.Equal(t, "tenant-x", realmName)
		assert.Equal(t, userID, uid)
		return nil // KeycloakAdmin.DeleteUser is itself idempotent (§9)
	}
	require.NoError(t, svc.DeleteUser(context.Background(), tenantID, userID))
	require.NoError(t, svc.DeleteUser(context.Background(), tenantID, userID))
	assert.Equal(t, 2, calls)
}

func TestDeleteUser_TenantNotFound(t *testing.T) {
	svc, _, _, _ := newLifecycleTestService()
	err := svc.DeleteUser(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
}

// ── RP-11: GetRealm ───────────────────────────────────────────────────────

func TestGetRealm_Success(t *testing.T) {
	svc, _, realms, _ := newLifecycleTestService()
	tenantID := uuid.New()
	shard := "kc-shard-1"
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated,
		KeycloakShard: &shard, Status: domain.RealmStatusActive,
	}

	result, err := svc.GetRealm(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, tenantID, result.TenantID)
	assert.Equal(t, "tenant-x", result.RealmName)
	assert.Equal(t, "dedicated", result.Strategy)
	require.NotNil(t, result.KeycloakShard)
	assert.Equal(t, "kc-shard-1", *result.KeycloakShard)
	assert.Equal(t, "active", result.Status)
}

func TestGetRealm_NotFound(t *testing.T) {
	svc, _, _, _ := newLifecycleTestService()
	_, err := svc.GetRealm(context.Background(), uuid.New())
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrTenantRealmNotFound, de.Code)
}

// ── RP-16: RevokeSessions ────────────────────────────────────────────────

func TestRevokeSessions_Idempotent(t *testing.T) {
	svc, kc, realms, _ := newLifecycleTestService()
	tenantID := uuid.New()
	userID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	calls := 0
	kc.RevokeUserSessionsFn = func(ctx context.Context, realmName string, uid uuid.UUID) error {
		calls++
		assert.Equal(t, "tenant-x", realmName)
		assert.Equal(t, userID, uid)
		return nil
	}
	require.NoError(t, svc.RevokeSessions(context.Background(), tenantID, userID))
	require.NoError(t, svc.RevokeSessions(context.Background(), tenantID, userID))
	assert.Equal(t, 2, calls)
}

func TestRevokeSessions_TenantNotFound(t *testing.T) {
	svc, _, _, _ := newLifecycleTestService()
	err := svc.RevokeSessions(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
}

// ── RP-10: Reactivate — remaining branches ───────────────────────────────

func TestReactivate_TenantNotFound(t *testing.T) {
	svc, _, _, idem, _ := newLifecycleTestServiceWithIdem()
	_, err := svc.Reactivate(context.Background(), "react-nf", uuid.New(), domain.ReactivationSourceOperator, "", nil)
	require.Error(t, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["react-nf"].Status)
}

// Idempotent no-op: an already-active tenant short-circuits without a
// second Keycloak call.
func TestReactivate_AlreadyActive_NoOp(t *testing.T) {
	svc, kc, realms, _, pub := newLifecycleTestServiceWithIdem()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusActive}
	kc.EnableRealmFn = func(ctx context.Context, realmName string) error {
		t.Fatal("must not call Keycloak for an already-active tenant")
		return nil
	}

	result, err := svc.Reactivate(context.Background(), "react-3", tenantID, domain.ReactivationSourceOperator, "", nil)
	require.NoError(t, err)
	assert.Equal(t, "active", result.Status)
	assert.Empty(t, pub.events)
}

// Reactivating a shared-strategy (trial) tenant enables just that tenant's
// users and emits TrialReactivated, optionally extending trial_ends_at.
func TestReactivate_SharedStrategy_EmitsTrialReactivated(t *testing.T) {
	svc, kc, realms, _, pub := newLifecycleTestServiceWithIdem()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusDisabled}
	var enabledRealm string
	var enabledTenant uuid.UUID
	kc.EnableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error {
		enabledRealm, enabledTenant = realmName, tid
		return nil
	}
	newEnds := time.Now().UTC().AddDate(0, 0, 14)

	result, err := svc.Reactivate(context.Background(), "react-4", tenantID, domain.ReactivationSourceBilling, "", &newEnds)
	require.NoError(t, err)
	assert.Equal(t, "active", result.Status)
	assert.Equal(t, "trial", enabledRealm)
	assert.Equal(t, tenantID, enabledTenant)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	require.NotNil(t, row.TrialEndsAt)
	assert.WithinDuration(t, newEnds, *row.TrialEndsAt, time.Second)

	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTrialReactivated, pub.events[0].Type)
	payload := pub.events[0].Data.(domain.TrialReactivatedPayload)
	assert.Equal(t, newEnds.Format(time.RFC3339), payload.NewTrialEndsAt)
}

// Shared-strategy reactivation with no newTrialEndsAt supplied still emits
// TrialReactivated, with an empty NewTrialEndsAt field.
func TestReactivate_SharedStrategy_NoNewTrialEndsAt(t *testing.T) {
	svc, kc, realms, _, pub := newLifecycleTestServiceWithIdem()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusSuspended}
	kc.EnableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error { return nil }

	_, err := svc.Reactivate(context.Background(), "react-5", tenantID, domain.ReactivationSourceBilling, "", nil)
	require.NoError(t, err)
	payload := pub.events[0].Data.(domain.TrialReactivatedPayload)
	assert.Equal(t, "", payload.NewTrialEndsAt)
}

// Keycloak-effect-first: a Keycloak failure on reactivate leaves the local
// row untouched and marks the idempotency key failed.
func TestReactivate_KeycloakFailure_NoLocalCommit(t *testing.T) {
	svc, kc, realms, idem, pub := newLifecycleTestServiceWithIdem()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusSuspended}
	kc.EnableRealmFn = func(ctx context.Context, realmName string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}

	_, err := svc.Reactivate(context.Background(), "react-6", tenantID, domain.ReactivationSourceOperator, "", nil)
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["react-6"].Status)

	row, _ := realms.FindByTenantID(context.Background(), tenantID)
	assert.Equal(t, domain.RealmStatusSuspended, row.Status, "the row must not have transitioned")
}

func TestReactivate_IdempotentReplay(t *testing.T) {
	svc, kc, realms, _, pub := newLifecycleTestServiceWithIdem()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusSuspended}
	calls := 0
	kc.EnableRealmFn = func(ctx context.Context, realmName string) error { calls++; return nil }

	first, err := svc.Reactivate(context.Background(), "react-7", tenantID, domain.ReactivationSourceOperator, "resolved", nil)
	require.NoError(t, err)
	second, err := svc.Reactivate(context.Background(), "react-7", tenantID, domain.ReactivationSourceOperator, "resolved", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, first, second)
	assert.Len(t, pub.events, 1)
}

// ── RP-12: ExpireTrial — remaining branches ──────────────────────────────

func TestExpireTrial_TenantNotFound(t *testing.T) {
	svc, _, _, idem, _ := newLifecycleTestServiceWithIdem()
	_, err := svc.ExpireTrial(context.Background(), "exp-nf", uuid.New(), 30)
	require.Error(t, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["exp-nf"].Status)
}

// ExpireTrial only applies to shared-strategy tenants.
func TestExpireTrial_RejectsNonSharedStrategy(t *testing.T) {
	svc, _, realms, idem, _ := newLifecycleTestServiceWithIdem()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusActive}

	_, err := svc.ExpireTrial(context.Background(), "exp-2", tenantID, 30)
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrInvalidRequest, de.Code)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["exp-2"].Status)
}

func TestExpireTrial_KeycloakFailure_NoLocalCommit(t *testing.T) {
	svc, kc, realms, idem, pub := newLifecycleTestServiceWithIdem()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusActive}
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}

	_, err := svc.ExpireTrial(context.Background(), "exp-3", tenantID, 30)
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["exp-3"].Status)
	row, _ := realms.FindByTenantID(context.Background(), tenantID)
	assert.Equal(t, domain.RealmStatusActive, row.Status)
}

func TestExpireTrial_IdempotentReplay(t *testing.T) {
	svc, kc, realms, _, pub := newLifecycleTestServiceWithIdem()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusActive}
	calls := 0
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error { calls++; return nil }

	first, err := svc.ExpireTrial(context.Background(), "exp-4", tenantID, 30)
	require.NoError(t, err)
	second, err := svc.ExpireTrial(context.Background(), "exp-4", tenantID, 30)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, first, second)
	assert.Len(t, pub.events, 1)
}

// A trial with no TrialEndsAt recorded falls back to "now" rather than
// panicking on a nil dereference.
func TestExpireTrial_NilTrialEndsAt_FallsBackToNow(t *testing.T) {
	svc, kc, realms, _, _ := newLifecycleTestServiceWithIdem()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusActive}
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error { return nil }

	result, err := svc.ExpireTrial(context.Background(), "exp-5", tenantID, 30)
	require.NoError(t, err)
	assert.NotEmpty(t, result.TrialEndsAt)
}

// ── RP-14: Suspend — remaining branches ──────────────────────────────────

func TestSuspend_TenantNotFound(t *testing.T) {
	svc, _, _, idem, _ := newLifecycleTestServiceWithIdem()
	_, err := svc.Suspend(context.Background(), "sus-nf", uuid.New(), "x")
	require.Error(t, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["sus-nf"].Status)
}

func TestSuspend_KeycloakFailure_NoLocalCommit(t *testing.T) {
	svc, kc, realms, idem, pub := newLifecycleTestServiceWithIdem()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusActive}
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}

	_, err := svc.Suspend(context.Background(), "sus-5", tenantID, "fraud")
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["sus-5"].Status)
	row, _ := realms.FindByTenantID(context.Background(), tenantID)
	assert.Equal(t, domain.RealmStatusActive, row.Status)
}

func TestSuspend_IdempotentReplay(t *testing.T) {
	svc, kc, realms, _, pub := newLifecycleTestServiceWithIdem()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusActive}
	calls := 0
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error { calls++; return nil }

	first, err := svc.Suspend(context.Background(), "sus-6", tenantID, "fraud")
	require.NoError(t, err)
	second, err := svc.Suspend(context.Background(), "sus-6", tenantID, "fraud")
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, first, second)
	assert.Len(t, pub.events, 1)
}

// ── Construction defaults, WithLogger, and best-effort logging branches ──

func TestNewLifecycleService_Defaults(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx, _ := newMockTxRunner()
	svc := NewLifecycleService(realms, idem, kc, tx, "")
	assert.Equal(t, "trial", svc.sharedRealm)
	assert.Equal(t, 7, svc.revertDays)
}

// failIdempotency's own persistence failure is logged, not escalated — the
// caller still sees the ORIGINAL Keycloak error.
func TestSuspend_FailIdempotencyPersistFailure_LoggedNotEscalated(t *testing.T) {
	svc, kc, realms, idem, _ := newLifecycleTestServiceWithIdem()
	logger := &mockLogger{}
	svc.WithLogger(logger)
	idem.FailErr = errors.New("ledger unavailable")
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusActive}
	originalErr := domain.NewError(domain.ErrKeycloakUnavailable, "down")
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error { return originalErr }

	_, err := svc.Suspend(context.Background(), "sus-7", tenantID, "fraud")
	require.Error(t, err)
	assert.Same(t, originalErr, err)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// completeIdempotency's persistence failure (the no-op short-circuit path)
// is likewise logged, not escalated.
func TestSuspend_AlreadySuspended_CompleteIdempotencyFailure_LoggedNotPropagated(t *testing.T) {
	svc, _, realms, idem, _ := newLifecycleTestServiceWithIdem()
	logger := &mockLogger{}
	svc.WithLogger(logger)
	idem.CompleteErr = errors.New("ledger unavailable")
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusSuspended}

	result, err := svc.Suspend(context.Background(), "sus-8", tenantID, "x")
	require.NoError(t, err)
	assert.Equal(t, "suspended", result.Status)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// The normal (non-no-op) success path's idempotency.Complete failure is
// also best-effort.
func TestReactivate_IdempotencyCompleteFailure_LoggedNotPropagated(t *testing.T) {
	svc, kc, realms, idem, _ := newLifecycleTestServiceWithIdem()
	logger := &mockLogger{}
	svc.WithLogger(logger)
	idem.CompleteErr = errors.New("ledger unavailable")
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusSuspended}
	kc.EnableRealmFn = func(ctx context.Context, realmName string) error { return nil }

	result, err := svc.Reactivate(context.Background(), "react-8", tenantID, domain.ReactivationSourceOperator, "", nil)
	require.NoError(t, err)
	assert.Equal(t, "active", result.Status)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// marshalIdempotencyResult's own marshal failure is logged, not propagated
// — the caller still gets whatever (possibly empty) bytes result.
func TestMarshalIdempotencyResult_MarshalFailure_LoggedNotPropagated(t *testing.T) {
	logger := &mockLogger{}
	sl := port.NewSlogStyleLogger(logger)
	body := marshalIdempotencyResult(context.Background(), sl, "test_op", make(chan int))
	assert.Empty(t, body)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// ── Begin hard-failure paths — a ledger outage, distinct from `found` ──────

func TestReactivate_BeginError_Propagates(t *testing.T) {
	svc, kc, _, idem, _ := newLifecycleTestServiceWithIdem()
	idem.BeginErr = errors.New("ledger unavailable")
	kc.EnableRealmFn = func(ctx context.Context, realmName string) error {
		t.Fatal("Keycloak must not be called when Begin itself fails")
		return nil
	}

	_, err := svc.Reactivate(context.Background(), "react-9", uuid.New(), domain.ReactivationSourceOperator, "", nil)
	require.Error(t, err)
}

func TestExpireTrial_BeginError_Propagates(t *testing.T) {
	svc, kc, _, idem, _ := newLifecycleTestServiceWithIdem()
	idem.BeginErr = errors.New("ledger unavailable")
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error {
		t.Fatal("Keycloak must not be called when Begin itself fails")
		return nil
	}

	_, err := svc.ExpireTrial(context.Background(), "exp-6", uuid.New(), 30)
	require.Error(t, err)
}

func TestSuspend_BeginError_Propagates(t *testing.T) {
	svc, kc, _, idem, _ := newLifecycleTestServiceWithIdem()
	idem.BeginErr = errors.New("ledger unavailable")
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error {
		t.Fatal("Keycloak must not be called when Begin itself fails")
		return nil
	}

	_, err := svc.Suspend(context.Background(), "sus-9", uuid.New(), "x")
	require.Error(t, err)
}

// ── Reactivate: shared-strategy Keycloak failure, Update failure, no
// publisher ────────────────────────────────────────────────────────────────

// The shared-strategy branch (EnableUsersInRealm) failing is
// Keycloak-effect-first too — leaves the local row untouched.
func TestReactivate_SharedStrategy_KeycloakFailure_NoLocalCommit(t *testing.T) {
	svc, kc, realms, idem, pub := newLifecycleTestServiceWithIdem()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusDisabled}
	kc.EnableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}

	_, err := svc.Reactivate(context.Background(), "react-10", tenantID, domain.ReactivationSourceBilling, "", nil)
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["react-10"].Status)
	row, _ := realms.FindByTenantID(context.Background(), tenantID)
	assert.Equal(t, domain.RealmStatusDisabled, row.Status)
}

// The local-commit tx's realms.Update failing (after a successful Keycloak
// call) marks the idempotency key failed and enqueues no events.
func TestReactivate_UpdateFails_MarksIdempotencyFailed(t *testing.T) {
	svc, kc, realms, idem, pub := newLifecycleTestServiceWithIdem()
	realms.UpdateErr = errors.New("db down")
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusSuspended}
	kc.EnableRealmFn = func(ctx context.Context, realmName string) error { return nil }

	_, err := svc.Reactivate(context.Background(), "react-11", tenantID, domain.ReactivationSourceOperator, "", nil)
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["react-11"].Status)
}

// When the tx runner wires no event publisher into ctx, the local commit
// still lands and the call still succeeds — just with no event enqueued.
func TestReactivate_NoEventPublisherInContext_StillCommitsLocally(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx := newMockTxRunnerNoPublisher()
	svc := NewLifecycleService(realms, idem, kc, tx, "trial")
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusSuspended}
	kc.EnableRealmFn = func(ctx context.Context, realmName string) error { return nil }

	result, err := svc.Reactivate(context.Background(), "react-12", tenantID, domain.ReactivationSourceOperator, "", nil)
	require.NoError(t, err)
	assert.Equal(t, "active", result.Status)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStatusActive, row.Status)
}

// ── ExpireTrial: TrialEndsAt-already-set branch, Update failure, no
// publisher, Complete failure ───────────────────────────────────────────────

// When the row already carries a TrialEndsAt, ExpireTrial reports that
// value rather than falling back to "now".
func TestExpireTrial_ExistingTrialEndsAt_ReportedVerbatim(t *testing.T) {
	svc, kc, realms, _, _ := newLifecycleTestServiceWithIdem()
	tenantID := uuid.New()
	trialEndsAt := time.Now().UTC().Add(-2 * time.Hour)
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared,
		Status: domain.RealmStatusActive, TrialEndsAt: &trialEndsAt,
	}
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error { return nil }

	result, err := svc.ExpireTrial(context.Background(), "exp-7", tenantID, 30)
	require.NoError(t, err)
	assert.Equal(t, trialEndsAt.Format(time.RFC3339), result.TrialEndsAt)
}

func TestExpireTrial_UpdateFails_MarksIdempotencyFailed(t *testing.T) {
	svc, kc, realms, idem, pub := newLifecycleTestServiceWithIdem()
	realms.UpdateErr = errors.New("db down")
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusActive}
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error { return nil }

	_, err := svc.ExpireTrial(context.Background(), "exp-8", tenantID, 30)
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["exp-8"].Status)
}

func TestExpireTrial_NoEventPublisherInContext_StillCommitsLocally(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx := newMockTxRunnerNoPublisher()
	svc := NewLifecycleService(realms, idem, kc, tx, "trial")
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusActive}
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error { return nil }

	result, err := svc.ExpireTrial(context.Background(), "exp-9", tenantID, 30)
	require.NoError(t, err)
	assert.NotEmpty(t, result.TrialEndsAt)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStatusDisabled, row.Status)
}

// idempotency.Complete's own persistence failure on the main (non-no-op)
// success path is best-effort — logged, not propagated.
func TestExpireTrial_IdempotencyCompleteFailure_LoggedNotPropagated(t *testing.T) {
	svc, kc, realms, idem, _ := newLifecycleTestServiceWithIdem()
	logger := &mockLogger{}
	svc.WithLogger(logger)
	idem.CompleteErr = errors.New("ledger unavailable")
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusActive}
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error { return nil }

	result, err := svc.ExpireTrial(context.Background(), "exp-10", tenantID, 30)
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// ── Suspend: shared-strategy Keycloak failure, Update failure, no
// publisher, Complete failure ───────────────────────────────────────────────

// The shared-strategy branch (DisableUsersInRealm) failing leaves the local
// row untouched (RP-INV-2: the shared realm itself is never disabled).
func TestSuspend_SharedStrategy_KeycloakFailure_NoLocalCommit(t *testing.T) {
	svc, kc, realms, idem, pub := newLifecycleTestServiceWithIdem()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusActive}
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}

	_, err := svc.Suspend(context.Background(), "sus-10", tenantID, "fraud")
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["sus-10"].Status)
	row, _ := realms.FindByTenantID(context.Background(), tenantID)
	assert.Equal(t, domain.RealmStatusActive, row.Status)
}

func TestSuspend_UpdateFails_MarksIdempotencyFailed(t *testing.T) {
	svc, kc, realms, idem, pub := newLifecycleTestServiceWithIdem()
	realms.UpdateErr = errors.New("db down")
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusActive}
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error { return nil }

	_, err := svc.Suspend(context.Background(), "sus-11", tenantID, "fraud")
	require.Error(t, err)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["sus-11"].Status)
}

func TestSuspend_NoEventPublisherInContext_StillCommitsLocally(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	idem := newMockIdempotencyStore()
	tx := newMockTxRunnerNoPublisher()
	svc := NewLifecycleService(realms, idem, kc, tx, "trial")
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusActive}
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error { return nil }

	result, err := svc.Suspend(context.Background(), "sus-12", tenantID, "fraud")
	require.NoError(t, err)
	assert.Equal(t, "suspended", result.Status)

	row, err := realms.FindByTenantID(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.RealmStatusSuspended, row.Status)
}

// idempotency.Complete's own persistence failure on the main (non-no-op)
// success path is best-effort — logged, not propagated.
func TestSuspend_IdempotencyCompleteFailure_LoggedNotPropagated(t *testing.T) {
	svc, kc, realms, idem, _ := newLifecycleTestServiceWithIdem()
	logger := &mockLogger{}
	svc.WithLogger(logger)
	idem.CompleteErr = errors.New("ledger unavailable")
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated, Status: domain.RealmStatusActive}
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error { return nil }

	result, err := svc.Suspend(context.Background(), "sus-13", tenantID, "fraud")
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}
