package unit_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// TestRefreshServiceAccountKeys exercises RP-17's standalone rotate/
// revoke/break-glass key-cache refresh path (§2.5, EXT-6) — clearing
// Keycloak's cached JWKS keys for the tenant's realm so a just-issued/
// rotated credential is recognized and a just-revoked one stops
// authenticating.
func TestRefreshServiceAccountKeys_Success(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	clientID := "platform-automation"
	principalID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "tenant-" + tenantID.String(), Strategy: domain.RealmStrategyDedicated,
		Status: domain.RealmStatusActive, ServiceAccountKeycloakClientID: &clientID, ServiceAccountPrincipalID: &principalID,
	}

	var gotRealm string
	kc.ClearServiceAccountKeysCacheFn = func(ctx context.Context, realmName string) error {
		gotRealm = realmName
		return nil
	}

	err := svc.RefreshServiceAccountKeys(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, "tenant-"+tenantID.String(), gotRealm)
}

func TestRefreshServiceAccountKeys_TenantNotFound(t *testing.T) {
	svc, _, _, _, _ := newProvisioningTestService(t)

	err := svc.RefreshServiceAccountKeys(context.Background(), uuid.New())
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrTenantRealmNotFound, de.Code)
}

func TestRefreshServiceAccountKeys_NeverMinted(t *testing.T) {
	svc, _, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusActive,
	}

	err := svc.RefreshServiceAccountKeys(context.Background(), tenantID)
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrServiceAccountNotProvisioned, de.Code)
}

func TestRefreshServiceAccountKeys_KeycloakFailure_Propagates(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	clientID := "platform-automation"
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "tenant-" + tenantID.String(), Strategy: domain.RealmStrategyDedicated,
		Status: domain.RealmStatusActive, ServiceAccountKeycloakClientID: &clientID,
	}
	kc.ClearServiceAccountKeysCacheFn = func(ctx context.Context, realmName string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "keycloak down")
	}

	err := svc.RefreshServiceAccountKeys(context.Background(), tenantID)
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrKeycloakUnavailable, de.Code)
}
