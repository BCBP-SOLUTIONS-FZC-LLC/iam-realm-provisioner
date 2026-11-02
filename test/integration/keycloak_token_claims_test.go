//go:build integration

package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real tokens from a real tender-web sign-in to the shared trial realm (the
// evaluate-scopes examples carry no auth_time, so they cannot show it):
//   - the access token has what the gateway and AuthZ Enrichment read (aud,
//     sub, tenant_id, auth_time) and no name, email or other profile claim
//     (HLD §4.1, RP-D22);
//   - the ID token still has name and email, which the frontend SDK reads.
func TestKeycloak_TenderWeb_RealTokenClaims(t *testing.T) {
	t.Parallel()
	admin, baseURL := setupKeycloakWithWeb(t)
	ctx := context.Background()
	realm := "trial-claims-" + uuid.NewString()[:8]
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, realm) })
	require.NoError(t, admin.EnsureSharedRealm(ctx, realm))

	client, tok := rawAdminClient(t, baseURL)
	const password = "Claims-test-pass-1!"
	tenantID := uuid.NewString()
	userID, err := client.CreateUser(ctx, tok, realm, gocloak.User{
		Username: gocloak.StringP("claims@example.com"), Email: gocloak.StringP("claims@example.com"),
		FirstName: gocloak.StringP("Token"), LastName: gocloak.StringP("Claims"),
		Enabled: gocloak.BoolP(true), EmailVerified: gocloak.BoolP(true),
		Attributes: &map[string][]string{"tenant_id": {tenantID}},
	})
	require.NoError(t, err)
	require.NoError(t, client.SetPassword(ctx, tok, userID, realm, password, false))

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	tokens := authCodePKCE(t, browser, baseURL, realm, "https://app.example.test/t/acme/", "claims@example.com", password)

	access := jwtClaims(t, tokens["access_token"].(string))
	assert.Contains(t, audiences(access["aud"]), "tender-web")
	assert.Equal(t, userID, access["sub"])
	assert.Equal(t, tenantID, access["tenant_id"])
	assert.IsType(t, float64(0), access["auth_time"], "AuthZ Enrichment's MFA-freshness gate reads auth_time")
	for _, claim := range []string{"name", "email", "preferred_username", "given_name", "family_name", "email_verified"} {
		assert.NotContains(t, access, claim, "the access token carries no %s", claim)
	}
	assert.NotContains(t, strings.Fields(access["scope"].(string)), "profile")
	assert.NotContains(t, strings.Fields(access["scope"].(string)), "email")

	id := jwtClaims(t, tokens["id_token"].(string))
	assert.Equal(t, "Token Claims", id["name"])
	assert.Equal(t, "claims@example.com", id["email"])
	assert.Equal(t, tenantID, id["tenant_id"])
	assert.IsType(t, float64(0), id["auth_time"])
}

// jwtClaims decodes a JWT's payload without verifying it.
func jwtClaims(t *testing.T, jwt string) map[string]any {
	t.Helper()
	parts := strings.Split(jwt, ".")
	require.Len(t, parts, 3)
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(raw, &claims))
	return claims
}
