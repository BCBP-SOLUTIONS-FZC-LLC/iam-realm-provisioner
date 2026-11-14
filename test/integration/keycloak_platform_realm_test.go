//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Nerzal/gocloak/v13"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// The platform realm, from nothing, against a real Keycloak 26 (CD-52 item 2):
// platform_operator role, realm-wide OTP enforcement via the browser flow,
// and short session lifetimes. A second run is a no-op (idempotent).
func TestKeycloakAdmin_EnsurePlatformRealm_CreatesAndConverges(t *testing.T) {
	t.Parallel()
	admin, baseURL := setupKeycloakWithURL(t)
	ctx := context.Background()
	realm := "platform-it-" + uuid.NewString()[:8]
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, realm) })

	require.NoError(t, admin.EnsurePlatformRealm(ctx, realm))
	require.NoError(t, admin.EnsurePlatformRealm(ctx, realm), "second run converges an already-correct realm")

	client, tok := rawAdminClient(t, baseURL)

	// platform_operator realm role must exist.
	roles, err := client.GetRealmRoles(ctx, tok, realm, gocloak.GetRoleParams{})
	require.NoError(t, err)
	var hasOperator bool
	for _, r := range roles {
		if r != nil && r.Name != nil && *r.Name == domain.PlatformOperatorRoleName {
			hasOperator = true
		}
	}
	assert.True(t, hasOperator, "platform_operator realm role must exist")

	// Browser flow must have its "Conditional OTP" subflow set to REQUIRED
	// so every login requires a second factor (not just users with requires-mfa).
	executions, err := client.GetAuthenticationExecutions(ctx, tok, realm, "browser")
	require.NoError(t, err)
	var foundOTP bool
	for _, exec := range executions {
		if exec == nil || exec.DisplayName == nil {
			continue
		}
		if strings.Contains(*exec.DisplayName, "Conditional OTP") {
			foundOTP = true
			require.NotNil(t, exec.Requirement)
			assert.Equal(t, "REQUIRED", *exec.Requirement, "Conditional OTP subflow must be REQUIRED")
		}
	}
	assert.True(t, foundOTP, "browser flow must contain a 'Conditional OTP' execution")

	// Short-session settings.
	settings := rawJSON(t, client, tok, baseURL+"/admin/realms/"+realm)
	assert.EqualValues(t, 300, settings["accessTokenLifespan"], "access token 5 min")
	assert.EqualValues(t, 1800, settings["ssoSessionIdleTimeout"], "SSO idle 30 min")
	assert.EqualValues(t, 28800, settings["ssoSessionMaxLifespan"], "SSO max 8 h")
	assert.EqualValues(t, 1800, settings["clientSessionIdleTimeout"], "client session idle 30 min")
	assert.EqualValues(t, 28800, settings["clientSessionMaxLifespan"], "client session max 8 h")
	assert.Equal(t, true, settings["revokeRefreshToken"])
	assert.EqualValues(t, 0, settings["refreshTokenMaxReuse"])
}
