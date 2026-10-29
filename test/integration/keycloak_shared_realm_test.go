//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	keycloakadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/keycloak"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// setupKeycloakWithWeb is setupKeycloakWithURL with the tender-web config
// EnsureSharedRealm requires and CreateRealmFromTemplate applies.
func setupKeycloakWithWeb(t *testing.T) (*keycloakadapter.Admin, string) {
	t.Helper()
	baseURL := startKeycloakContainer(t)
	web, err := keycloakadapter.NewWebClientConfig("tender-web", "https://app.example.test", "http://localhost:5173")
	require.NoError(t, err)
	admin := keycloakadapter.New(keycloakadapter.Config{
		BaseURL: baseURL, AdminRealm: "master", ClientID: kcClientID, ClientSecret: kcClientSecret,
		RateLimitRPS: 50, Timeout: 10 * time.Second, Web: web,
	}, noopLogger{})
	require.NoError(t, admin.Ready(context.Background()))
	return admin, baseURL
}

// rawJSON GETs an admin path as a generic map — for fields gocloak's
// structs do not carry (clientSession*, the user profile).
func rawJSON(t *testing.T, client *gocloak.GoCloak, tok, url string) map[string]any {
	t.Helper()
	resp, err := client.GetRequestWithBearerAuth(context.Background(), tok).Get(url)
	require.NoError(t, err)
	require.False(t, resp.IsError(), resp.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(resp.Body(), &out))
	return out
}

// The shared trial realm, from nothing, against a real Keycloak 26: the
// hardened tender-web client, the HLD §11.3 lifetimes, an admin-only
// tenant_id user-profile attribute, and a tenant_id claim in the access and
// ID tokens of a trial user.
func TestKeycloakAdmin_EnsureSharedRealm_CreatesAndConverges(t *testing.T) {
	t.Parallel()
	admin, baseURL := setupKeycloakWithWeb(t)
	ctx := context.Background()
	realm := "trial-it-" + uuid.NewString()[:8]
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, realm) })

	require.NoError(t, admin.EnsureSharedRealm(ctx, realm))
	require.NoError(t, admin.EnsureSharedRealm(ctx, realm), "a second run converges a realm that is already right")

	client, tok := rawAdminClient(t, baseURL)

	settings := rawJSON(t, client, tok, baseURL+"/admin/realms/"+realm)
	assert.EqualValues(t, 300, settings["accessTokenLifespan"])
	assert.EqualValues(t, 43200, settings["ssoSessionIdleTimeout"])
	assert.EqualValues(t, 86400, settings["ssoSessionMaxLifespan"])
	assert.EqualValues(t, 28800, settings["clientSessionIdleTimeout"])
	assert.EqualValues(t, 43200, settings["clientSessionMaxLifespan"])
	assert.Equal(t, true, settings["revokeRefreshToken"])
	assert.EqualValues(t, 0, settings["refreshTokenMaxReuse"])

	clients, err := client.GetClients(ctx, tok, realm, gocloak.GetClientsParams{ClientID: gocloak.StringP("tender-web")})
	require.NoError(t, err)
	require.Len(t, clients, 1)
	web := clients[0]
	assert.ElementsMatch(t, []string{"https://app.example.test/*", "http://localhost:5173/*"}, *web.RedirectURIs)
	assert.ElementsMatch(t, []string{"https://app.example.test", "http://localhost:5173"}, *web.WebOrigins)
	assert.Equal(t, "S256", (*web.Attributes)["pkce.code.challenge.method"])
	assert.Equal(t, "https://app.example.test/*##http://localhost:5173/*", (*web.Attributes)["post.logout.redirect.uris"])
	assert.True(t, *web.PublicClient)
	assert.False(t, *web.ImplicitFlowEnabled)
	assert.False(t, *web.DirectAccessGrantsEnabled)
	require.NotNil(t, web.ProtocolMappers)
	mappers := 0
	for _, m := range *web.ProtocolMappers {
		if *m.Name == "tenant_id" {
			mappers++
		}
	}
	assert.Equal(t, 1, mappers, "exactly one tenant_id mapper after two runs")

	profile := rawJSON(t, client, tok, baseURL+"/admin/realms/"+realm+"/users/profile")
	var decl map[string]any
	for _, a := range profile["attributes"].([]any) {
		if m := a.(map[string]any); m["name"] == "tenant_id" {
			decl = m
		}
	}
	require.NotNil(t, decl, "tenant_id is a declared (managed) attribute")
	assert.Equal(t, map[string]any{"view": []any{"admin"}, "edit": []any{"admin"}}, decl["permissions"])

	// An admin-created user keeps tenant_id and the token carries it.
	tenantID := uuid.New()
	userID, err := admin.CreateInvitedUser(ctx, realm, port.CreateUserRequest{
		Email: "trial@example.com", FullName: "Trial User", TenantID: tenantID, EmailVerified: true,
	})
	require.NoError(t, err)
	found, err := admin.ListUsersByTenant(ctx, realm, tenantID)
	require.NoError(t, err)
	require.Contains(t, found, userID)

	for _, kind := range []string{"access-token", "id-token"} {
		example := rawJSON(t, client, tok, baseURL+"/admin/realms/"+realm+"/clients/"+*web.ID+
			"/evaluate-scopes/generate-example-"+kind+"?scope=openid&userId="+userID.String())
		assert.Equal(t, tenantID.String(), example["tenant_id"], "%s carries the tenant_id claim", kind)
	}
}

// A new dedicated realm gets the hardened tender-web client at creation:
// exact redirect URIs/origins from the web config, no wildcard, no "+",
// PKCE S256, and no tenant_id mapper (its tenant is its iss, HLD §5.4).
func TestKeycloakAdmin_CreateRealmFromTemplate_HardenedWebClient(t *testing.T) {
	t.Parallel()
	admin, baseURL := setupKeycloakWithWeb(t)
	ctx := context.Background()
	realm := "tenant-" + uuid.NewString()[:8]
	require.NoError(t, admin.CreateRealmFromTemplate(ctx, realm, "v1"))
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, realm) })

	client, tok := rawAdminClient(t, baseURL)
	clients, err := client.GetClients(ctx, tok, realm, gocloak.GetClientsParams{ClientID: gocloak.StringP("tender-web")})
	require.NoError(t, err)
	require.Len(t, clients, 1)
	got := clients[0]
	assert.ElementsMatch(t, []string{"https://app.example.test/*", "http://localhost:5173/*"}, *got.RedirectURIs)
	assert.ElementsMatch(t, []string{"https://app.example.test", "http://localhost:5173"}, *got.WebOrigins)
	assert.Equal(t, "S256", (*got.Attributes)["pkce.code.challenge.method"])
	assert.False(t, *got.DirectAccessGrantsEnabled)
	if got.ProtocolMappers != nil {
		for _, m := range *got.ProtocolMappers {
			assert.NotEqual(t, "tenant_id", *m.Name, "a dedicated realm gets no tenant_id mapper")
		}
	}

	settings := rawJSON(t, client, tok, baseURL+"/admin/realms/"+realm)
	assert.EqualValues(t, 300, settings["accessTokenLifespan"])
	assert.EqualValues(t, 28800, settings["clientSessionIdleTimeout"])
	assert.Equal(t, true, settings["revokeRefreshToken"])
}
