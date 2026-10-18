//go:build e2e

package e2e_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

type provisionDirectBody struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	TenantName string    `json:"tenant_name"`
	TenantSlug string    `json:"tenant_slug"`
	AdminEmail string    `json:"admin_email"`
	AdminName  string    `json:"admin_name"`
	Plan       string    `json:"plan"`
}

type configureIdpBody struct {
	IdpAlias    string         `json:"idp_alias"`
	Protocol    string         `json:"protocol"`
	DisplayName string         `json:"display_name"`
	Config      map[string]any `json:"config"`
}

// provisionDedicatedTenant provisions a fresh dedicated-strategy tenant via
// RP-2 (POST provision-direct) — the fastest real path to a dedicated realm,
// which RP-6 (ConfigureIdp) requires (IdP configuration is unavailable on
// the shared trial realm, §8.4).
func provisionDedicatedTenant(t *testing.T) (tenantID uuid.UUID, realmName string) {
	t.Helper()
	tenantID = newTenantID()
	slug := "e2e-idp-" + uuid.NewString()[:8]
	req := provisionDirectBody{
		TenantID: tenantID, TenantName: "E2E IdP Co", TenantSlug: slug,
		AdminEmail: slug + "@example.com", AdminName: "Direct Admin", Plan: "pro",
	}
	var result service.ProvisionDirectResult
	resp := doJSON(t, http.MethodPost, "/api/v1/internal/tenants/provision-direct",
		withHeader(sysHeaders(tenantID), "Idempotency-Key", newIdempotencyKey()), req, &result)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	return tenantID, result.Realm
}

// TestConfigureIdp_RegionPinnedURL_Blocked exercises RP-6's §11.6 CRITICAL
// gate: an OIDC issuer URL carrying an AWS region token must be rejected
// with 400 idp_region_portability_failed before anything reaches Keycloak
// or OpenBao (internal/core/service/idp_validator.go's carriesRegion check
// against internal/core/service/idp_service.go's ValidateIdpConfig gate).
func TestConfigureIdp_RegionPinnedURL_Blocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tenantID, dedicatedRealm := provisionDedicatedTenant(t)

	body := configureIdpBody{
		IdpAlias: "acme-oidc", Protocol: "oidc", DisplayName: "Acme OIDC",
		Config: map[string]any{
			"issuer":       "https://us-east-1.auth.bcbpsolutions.com/realms/acme",
			"redirect_uri": "https://auth.bcbpsolutions.com/callback",
			"audience":     "acme-clients",
			"pkce_method":  "S256",
		},
	}
	resp, er := decodeErrorBody(t, http.MethodPut, "/api/v1/internal/tenants/"+tenantID.String()+"/idp",
		withHeader(sysHeaders(tenantID), "Idempotency-Key", newIdempotencyKey()), body)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "IDP_REGION_PORTABILITY_FAILED", er.ErrorCode)

	// Never reaches Keycloak: no identity provider was created.
	client, tok, err := rawKeycloakClient(ctx)
	require.NoError(t, err)
	_, kcErr := client.GetIdentityProvider(ctx, tok, dedicatedRealm, "acme-oidc")
	require.Error(t, kcErr, "a CRITICAL-check failure must never reach Keycloak's CreateOrUpdateIdp")
}

// TestConfigureIdp_RegionNeutralURL_Succeeds exercises RP-6's happy path: a
// region-neutral OIDC config with a strong PKCE method passes every §11.6
// check (no WARNs to acknowledge either), writes the IdP to the live
// Keycloak realm, and upserts the local keycloak_idp_configs row.
func TestConfigureIdp_RegionNeutralURL_Succeeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tenantID, dedicatedRealm := provisionDedicatedTenant(t)

	body := configureIdpBody{
		IdpAlias: "acme-oidc", Protocol: "oidc", DisplayName: "Acme OIDC",
		Config: map[string]any{
			"issuer":       "https://auth.bcbpsolutions.com/realms/acme",
			"redirect_uri": "https://auth.bcbpsolutions.com/callback",
			"audience":     "acme-clients",
			"pkce_method":  "S256",
			"client_id":    "acme-client",
		},
	}
	var result service.ConfigureIdpResult
	resp := doJSON(t, http.MethodPut, "/api/v1/internal/tenants/"+tenantID.String()+"/idp",
		withHeader(sysHeaders(tenantID), "Idempotency-Key", newIdempotencyKey()), body, &result)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "acme-oidc", result.IdpAlias)
	require.Equal(t, "active", result.Status)

	// Live Keycloak: the identity provider now exists on the dedicated realm.
	client, tok, err := rawKeycloakClient(ctx)
	require.NoError(t, err)
	idp, err := client.GetIdentityProvider(ctx, tok, dedicatedRealm, "acme-oidc")
	require.NoError(t, err, "the identity provider must exist in Keycloak after a passing ConfigureIdp")
	require.NotNil(t, idp.Enabled)
	require.True(t, *idp.Enabled)

	// Local row: keycloak_idp_configs upserted (RLS-scoped — read via the
	// superuser pool, independent of the caller's GUC).
	var status string
	err = rawPG.QueryRow(ctx, `
		SELECT status FROM keycloak_idp_configs WHERE tenant_id = $1 AND idp_alias = $2`,
		tenantID, "acme-oidc").Scan(&status)
	require.NoError(t, err)
	require.Equal(t, "active", status)
}
