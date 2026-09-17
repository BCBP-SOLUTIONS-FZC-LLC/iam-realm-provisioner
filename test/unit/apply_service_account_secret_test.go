package unit_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// TestApplyServiceAccountSecret exercises RP-17's standalone rotate/
// break-glass-apply path (§2.5) — applying an already-issued secret to a
// tenant's already-minted platform-automation Keycloak client.
func TestApplyServiceAccountSecret_Success(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	clientID := "platform-automation"
	principalID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "tenant-" + tenantID.String(), Strategy: domain.RealmStrategyDedicated,
		Status: domain.RealmStatusActive, ServiceAccountKeycloakClientID: &clientID, ServiceAccountPrincipalID: &principalID,
	}

	var gotRealm, gotClientID, gotSecret string
	kc.SetClientSecretFn = func(ctx context.Context, realmName, clientID, secret string) error {
		gotRealm, gotClientID, gotSecret = realmName, clientID, secret
		return nil
	}

	err := svc.ApplyServiceAccountSecret(context.Background(), tenantID, "rotated-secret")
	require.NoError(t, err)
	assert.Equal(t, "tenant-"+tenantID.String(), gotRealm)
	assert.Equal(t, "platform-automation", gotClientID)
	assert.Equal(t, "rotated-secret", gotSecret)
}

func TestApplyServiceAccountSecret_TenantNotFound(t *testing.T) {
	svc, _, _, _, _ := newProvisioningTestService(t)

	err := svc.ApplyServiceAccountSecret(context.Background(), uuid.New(), "s")
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrTenantRealmNotFound, de.Code)
}

func TestApplyServiceAccountSecret_NeverMinted(t *testing.T) {
	svc, _, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusActive,
	}

	err := svc.ApplyServiceAccountSecret(context.Background(), tenantID, "s")
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrServiceAccountNotProvisioned, de.Code)
}

func TestApplyServiceAccountSecret_KeycloakFailure_Propagates(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	clientID := "platform-automation"
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "tenant-" + tenantID.String(), Strategy: domain.RealmStrategyDedicated,
		Status: domain.RealmStatusActive, ServiceAccountKeycloakClientID: &clientID,
	}
	kc.SetClientSecretFn = func(ctx context.Context, realmName, clientID, secret string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "keycloak down")
	}

	err := svc.ApplyServiceAccountSecret(context.Background(), tenantID, "s")
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrKeycloakUnavailable, de.Code)
}
