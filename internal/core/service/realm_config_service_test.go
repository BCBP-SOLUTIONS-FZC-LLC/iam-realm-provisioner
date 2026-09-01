package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

func newRealmConfigTestService() (*RealmConfigService, *mockKeycloakAdmin, *mockTenantRealmRepository, *mockReconciliationRepository) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	recon := &mockReconciliationRepository{}
	return NewRealmConfigService(realms, recon, kc), kc, realms, recon
}

func TestApplyRealmConfig_UnsupportedKey_Returns400(t *testing.T) {
	svc, _, _, _ := newRealmConfigTestService()
	_, err := svc.ApplyRealmConfig(context.Background(), uuid.New(), "not_on_the_allowlist", "x")
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrUnsupportedRealmSetting, de.Code)
	assert.Equal(t, 400, de.Status())
}

func TestApplyRealmConfig_TenantNotFound(t *testing.T) {
	svc, _, _, _ := newRealmConfigTestService()
	_, err := svc.ApplyRealmConfig(context.Background(), uuid.New(), "local_accounts_enabled", true)
	require.Error(t, err)
}

// RP-8: successful apply returns {applied:true}, never touching the
// reconciliation ledger.
func TestApplyRealmConfig_Success(t *testing.T) {
	svc, kc, realms, recon := newRealmConfigTestService()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	recon.InsertFn = func(ctx context.Context, r *domain.RealmConfigReconciliation) error {
		t.Fatal("must not record a reconciliation row on a successful apply")
		return nil
	}
	var appliedRealm, appliedKey string
	var appliedValue any
	kc.ApplyRealmConfigFn = func(ctx context.Context, realmName, settingKey string, desiredValue any) error {
		appliedRealm, appliedKey, appliedValue = realmName, settingKey, desiredValue
		return nil
	}

	result, err := svc.ApplyRealmConfig(context.Background(), tenantID, "local_accounts_enabled", false)
	require.NoError(t, err)
	assert.True(t, result.Applied)
	assert.False(t, result.Reconciling)
	assert.Equal(t, "tenant-x", appliedRealm)
	assert.Equal(t, "local_accounts_enabled", appliedKey)
	assert.Equal(t, false, appliedValue)
}

// `idp:<alias>` federation-toggle keys are additionally allowed.
func TestApplyRealmConfig_IdpAliasKey_Allowed(t *testing.T) {
	svc, kc, realms, _ := newRealmConfigTestService()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	kc.ApplyRealmConfigFn = func(ctx context.Context, realmName, settingKey string, desiredValue any) error { return nil }

	result, err := svc.ApplyRealmConfig(context.Background(), tenantID, "idp:acme-sso", true)
	require.NoError(t, err)
	assert.True(t, result.Applied)
}

// RP-INV-6 (local-intent-first): a transient Keycloak failure records a
// pending row and returns {applied:false, reconciling:true} as a 200, NOT
// an error.
func TestApplyRealmConfig_KeycloakFailure_RecordsPendingReconciliation(t *testing.T) {
	svc, kc, realms, recon := newRealmConfigTestService()
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	kc.ApplyRealmConfigFn = func(ctx context.Context, realmName, settingKey string, desiredValue any) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	var recorded *domain.RealmConfigReconciliation
	recon.InsertFn = func(ctx context.Context, r *domain.RealmConfigReconciliation) error {
		recorded = r
		return nil
	}

	result, err := svc.ApplyRealmConfig(context.Background(), tenantID, "local_accounts_enabled", true)
	require.NoError(t, err, "a reconcilable Keycloak failure must not be an error response")
	assert.False(t, result.Applied)
	assert.True(t, result.Reconciling)
	require.NotNil(t, recorded)
	assert.Equal(t, tenantID, recorded.TenantID)
	assert.Equal(t, "local_accounts_enabled", recorded.SettingKey)
}

// A failure to record the pending reconciliation is itself best-effort —
// logged, not propagated; the caller still gets {applied:false, reconciling:true}.
func TestApplyRealmConfig_ReconciliationInsertFailure_LoggedNotPropagated(t *testing.T) {
	svc, kc, realms, recon := newRealmConfigTestService()
	logger := &mockLogger{}
	svc.WithLogger(logger)
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated}
	kc.ApplyRealmConfigFn = func(ctx context.Context, realmName, settingKey string, desiredValue any) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	recon.InsertFn = func(ctx context.Context, r *domain.RealmConfigReconciliation) error {
		return errors.New("ledger unavailable")
	}

	result, err := svc.ApplyRealmConfig(context.Background(), tenantID, "local_accounts_enabled", true)
	require.NoError(t, err)
	assert.False(t, result.Applied)
	assert.True(t, result.Reconciling)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}
