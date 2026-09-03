package unit_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

type mockOrgMembershipClient struct {
	HasResidualFn func(ctx context.Context, tenantID, userID uuid.UUID) (bool, error)
}

func (m *mockOrgMembershipClient) HasResidualMFARequirement(ctx context.Context, tenantID, userID uuid.UUID) (bool, error) {
	return m.HasResidualFn(ctx, tenantID, userID)
}

func newMfaTestService() (*service.MfaService, *mockKeycloakAdmin, *mockTenantRealmRepository, *mockOrgMembershipClient) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	orgMem := &mockOrgMembershipClient{}
	return service.NewMfaService(realms, kc, orgMem), kc, realms, orgMem
}

func TestResetMFA_NoCredentials_Idempotent(t *testing.T) {
	svc, kc, realms, _ := newMfaTestService()
	tenantID, userID := uuid.New(), uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	kc.HasRequiresMFAFn = func(ctx context.Context, realmName string, uid uuid.UUID) (bool, error) { return false, nil }
	kc.ResetMFAFn = func(ctx context.Context, realmName string, uid uuid.UUID, requiresMFA bool) error {
		assert.False(t, requiresMFA)
		return nil
	}
	require.NoError(t, svc.ResetMFA(context.Background(), tenantID, userID))
}

func TestResetMFA_KeycloakFailure_FailClosed(t *testing.T) {
	svc, kc, realms, _ := newMfaTestService()
	tenantID, userID := uuid.New(), uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	kc.HasRequiresMFAFn = func(ctx context.Context, realmName string, uid uuid.UUID) (bool, error) { return false, nil }
	kc.ResetMFAFn = func(ctx context.Context, realmName string, uid uuid.UUID, requiresMFA bool) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	err := svc.ResetMFA(context.Background(), tenantID, userID)
	require.Error(t, err)
	de := err.(*domain.Error)
	assert.Equal(t, domain.ErrKeycloakUnavailable, de.Code)
}

// Grant is unconditional (assign-if-absent) — no O&M call needed.
func TestSyncRequiresMfa_Grant_Unconditional(t *testing.T) {
	svc, kc, realms, orgMem := newMfaTestService()
	tenantID, userID := uuid.New(), uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	orgMem.HasResidualFn = func(ctx context.Context, tid, uid uuid.UUID) (bool, error) {
		t.Fatal("grant must not consult O&M's I-8")
		return false, nil
	}
	var assigned bool
	kc.AssignRequiresMFAFn = func(ctx context.Context, realmName string, uid uuid.UUID) error { assigned = true; return nil }

	require.NoError(t, svc.SyncRequiresMfa(context.Background(), tenantID, userID, true))
	assert.True(t, assigned)
}

// Revoke consults O&M's I-8 first; a residual requirement blocks the revoke.
func TestSyncRequiresMfa_Revoke_ResidualRequirementBlocks(t *testing.T) {
	svc, kc, realms, orgMem := newMfaTestService()
	tenantID, userID := uuid.New(), uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	orgMem.HasResidualFn = func(ctx context.Context, tid, uid uuid.UUID) (bool, error) { return true, nil }
	kc.RevokeRequiresMFAFn = func(ctx context.Context, realmName string, uid uuid.UUID) error {
		t.Fatal("must not revoke when a residual MFA requirement exists")
		return nil
	}

	require.NoError(t, svc.SyncRequiresMfa(context.Background(), tenantID, userID, false))
}

func TestSyncRequiresMfa_Revoke_NoResidual_Revokes(t *testing.T) {
	svc, kc, realms, orgMem := newMfaTestService()
	tenantID, userID := uuid.New(), uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	orgMem.HasResidualFn = func(ctx context.Context, tid, uid uuid.UUID) (bool, error) { return false, nil }
	var revoked bool
	kc.RevokeRequiresMFAFn = func(ctx context.Context, realmName string, uid uuid.UUID) error { revoked = true; return nil }

	require.NoError(t, svc.SyncRequiresMfa(context.Background(), tenantID, userID, false))
	assert.True(t, revoked)
}

func TestResetMFA_TenantNotFound(t *testing.T) {
	svc, _, _, _ := newMfaTestService()
	err := svc.ResetMFA(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
}

func TestResetMFA_HasRequiresMFAFailure_FailClosed(t *testing.T) {
	svc, kc, realms, _ := newMfaTestService()
	tenantID, userID := uuid.New(), uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	kc.HasRequiresMFAFn = func(ctx context.Context, realmName string, uid uuid.UUID) (bool, error) {
		return false, domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	err := svc.ResetMFA(context.Background(), tenantID, userID)
	require.Error(t, err)
}

func TestSyncRequiresMfa_TenantNotFound(t *testing.T) {
	svc, _, _, _ := newMfaTestService()
	err := svc.SyncRequiresMfa(context.Background(), uuid.New(), uuid.New(), true)
	require.Error(t, err)
}

func TestSyncRequiresMfa_Grant_KeycloakFailure(t *testing.T) {
	svc, kc, realms, _ := newMfaTestService()
	tenantID, userID := uuid.New(), uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	kc.AssignRequiresMFAFn = func(ctx context.Context, realmName string, uid uuid.UUID) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	err := svc.SyncRequiresMfa(context.Background(), tenantID, userID, true)
	require.Error(t, err)
}

func TestSyncRequiresMfa_Revoke_OrgMembershipFailure(t *testing.T) {
	svc, _, realms, orgMem := newMfaTestService()
	tenantID, userID := uuid.New(), uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	orgMem.HasResidualFn = func(ctx context.Context, tid, uid uuid.UUID) (bool, error) {
		return false, domain.NewError(domain.ErrOrgMembershipUnavailable, "down")
	}
	err := svc.SyncRequiresMfa(context.Background(), tenantID, userID, false)
	require.Error(t, err)
}

func TestSyncRequiresMfa_Revoke_KeycloakFailure(t *testing.T) {
	svc, kc, realms, orgMem := newMfaTestService()
	tenantID, userID := uuid.New(), uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	orgMem.HasResidualFn = func(ctx context.Context, tid, uid uuid.UUID) (bool, error) { return false, nil }
	kc.RevokeRequiresMFAFn = func(ctx context.Context, realmName string, uid uuid.UUID) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	err := svc.SyncRequiresMfa(context.Background(), tenantID, userID, false)
	require.Error(t, err)
}

func TestMfaService_WithLogger(t *testing.T) {
	svc, kc, realms, orgMem := newMfaTestService()
	logger := &mockLogger{}
	got := svc.WithLogger(logger)
	assert.Same(t, svc, got)

	tenantID, userID := uuid.New(), uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	orgMem.HasResidualFn = func(ctx context.Context, tid, uid uuid.UUID) (bool, error) { return true, nil }
	kc.RevokeRequiresMFAFn = func(ctx context.Context, realmName string, uid uuid.UUID) error {
		t.Fatal("must not revoke when a residual MFA requirement exists")
		return nil
	}
	require.NoError(t, svc.SyncRequiresMfa(context.Background(), tenantID, userID, false))
}
