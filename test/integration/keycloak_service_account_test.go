//go:build integration

// This file validates RP-17's core unverified assumption against a real
// Keycloak: that gocloak's UpdateClient with an explicit Secret field
// actually sets an arbitrary caller-supplied client secret (as opposed to
// only Keycloak's own RegenerateClientSecret, which mints a random one).
// No gocloak wrapper exists for this — CreateServiceAccountClient/
// SetClientSecret rely on the general-purpose UpdateClient endpoint, so
// this is the one place that assumption is exercised against a live
// server rather than a mocked HTTP handler.
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// clientCredentialsToken attempts a client-credentials grant for
// (clientID, secret) against realmName and reports whether Keycloak
// accepted it — the only way to prove a secret actually took effect,
// rather than merely that the UpdateClient call returned 2xx.
func clientCredentialsToken(t *testing.T, baseURL, realmName, clientID, secret string) (ok bool, status int) {
	t.Helper()
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
	}
	resp, err := http.Post(baseURL+"/realms/"+realmName+"/protocol/openid-connect/token",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return false, resp.StatusCode
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.Unmarshal(body, &tok), "expected a token response, got: %s", body)
	return tok.AccessToken != "", resp.StatusCode
}

func TestKeycloakAdmin_CreateServiceAccountClient_And_SetClientSecret(t *testing.T) {
	t.Parallel()
	admin, baseURL := setupKeycloakWithURL(t)
	ctx := context.Background()
	realmName := "tenant-" + uuid.NewString()[:8]
	require.NoError(t, admin.CreateRealmFromTemplate(ctx, realmName, "v1"))
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, realmName) })

	clientID := fmt.Sprintf("platform-automation-%s", uuid.NewString())

	sub, err := admin.CreateServiceAccountClient(ctx, realmName, clientID)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, sub)

	// Idempotent resume: a repeat call returns the same sub, does not
	// create a second client.
	sub2, err := admin.CreateServiceAccountClient(ctx, realmName, clientID)
	require.NoError(t, err)
	require.Equal(t, sub, sub2)

	// A freshly-minted service-account client has no usable secret until
	// SetClientSecret applies one.
	ok, status := clientCredentialsToken(t, baseURL, realmName, clientID, "not-the-real-secret")
	require.False(t, ok, "an unset/placeholder secret must not authenticate")
	require.NotEqual(t, http.StatusOK, status)

	require.NoError(t, admin.SetClientSecret(ctx, realmName, clientID, "first-secret-value"))
	ok, _ = clientCredentialsToken(t, baseURL, realmName, clientID, "first-secret-value")
	require.True(t, ok, "the caller-supplied secret must actually authenticate the client-credentials grant")

	// Rotation: applying a second value invalidates the first (Keycloak
	// client secrets are single-valued, unlike TS-1's dual-secret overlap
	// window — RP-17's rotation-apply endpoint relies on this).
	require.NoError(t, admin.SetClientSecret(ctx, realmName, clientID, "second-secret-value"))
	ok, _ = clientCredentialsToken(t, baseURL, realmName, clientID, "second-secret-value")
	require.True(t, ok, "the rotated secret must authenticate")
	ok, _ = clientCredentialsToken(t, baseURL, realmName, clientID, "first-secret-value")
	require.False(t, ok, "the superseded secret must no longer authenticate")
}
