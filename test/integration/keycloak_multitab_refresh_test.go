//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// tab is one browser tab's in-memory token lineage: the SDK keeps tokens
// per tab and refreshes only its own refresh token, never sharing them.
type tab struct {
	name    string
	refresh string
}

var loginFormAction = regexp.MustCompile(`<form[^>]*id="kc-form-login"[^>]*action="([^"]+)"`)

// authCodePKCE runs one Auth Code + PKCE (S256) round for tender-web on
// browser (its cookie jar is the SSO session). It posts the login form only
// when Keycloak shows one; with a live SSO session the authorize request
// redirects straight back with a code. Returns the token response.
func authCodePKCE(t *testing.T, browser *http.Client, baseURL, realm, redirectURI, user, password string) map[string]any {
	t.Helper()
	verifier := make([]byte, 32)
	_, err := rand.Read(verifier)
	require.NoError(t, err)
	codeVerifier := base64.RawURLEncoding.EncodeToString(verifier)
	sum := sha256.Sum256([]byte(codeVerifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	oidc := baseURL + "/realms/" + realm + "/protocol/openid-connect"
	authURL := oidc + "/auth?" + url.Values{
		"client_id":             {"tender-web"},
		"response_type":         {"code"},
		"scope":                 {"openid"},
		"redirect_uri":          {redirectURI},
		"state":                 {uuid.NewString()},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	resp, err := browser.Get(authURL)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		m := loginFormAction.FindSubmatch(body)
		require.NotNil(t, m, "login form not found: %s", body)
		resp, err = browser.PostForm(html.UnescapeString(string(m[1])), url.Values{
			"username": {user}, "password": {password}, "credentialId": {""},
		})
		require.NoError(t, err)
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	require.Equal(t, http.StatusFound, resp.StatusCode, "expected a redirect with a code: %s", body)
	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(loc.String(), redirectURI), "redirected to %s", loc)
	code := loc.Query().Get("code")
	require.NotEmpty(t, code, "no code in %s", loc)

	return tokenCall(t, oidc+"/token", url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {"tender-web"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {codeVerifier},
	}, http.StatusOK)
}

// tokenCall POSTs to the token endpoint and returns the JSON body, failing
// unless the status is want.
func tokenCall(t *testing.T, tokenURL string, form url.Values, want int) map[string]any {
	t.Helper()
	resp, err := http.PostForm(tokenURL, form)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, want, resp.StatusCode, "token endpoint: %v", out)
	return out
}

// Round-2 ask L (frontend SDK): with RP #41's rotation settings
// (revokeRefreshToken=true, refreshTokenMaxReuse=0), two browser tabs in
// one SSO session each hold their own token lineage and refresh it
// independently, and one tab's refresh never invalidates the other's.
//
// Verified Keycloak 26 behavior this pins:
//   - every authorization-code exchange starts its own lineage (its own
//     reuse_id claim), and reuse is tracked per lineage, not per client
//     session — so the two tabs do not collide;
//   - reuse detection ("Stale token") compares the token's iat, which has
//     one-second resolution, with the lineage's last refresh — so every
//     step here waits more than a second, or a same-second replay would go
//     unnoticed and a same-second collision could pass by accident;
//   - a detected replay revokes the whole SSO session: both tabs are
//     signed out (asserted last).
func TestKeycloak_TenderWeb_MultiTabRefreshRotation(t *testing.T) {
	t.Parallel()
	admin, baseURL := setupKeycloakWithWeb(t)
	ctx := context.Background()
	realm := "trial-mt-" + uuid.NewString()[:8]
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, realm) })
	require.NoError(t, admin.EnsureSharedRealm(ctx, realm))

	// A ready-to-sign-in user: verified email, full profile, no required
	// actions, a password.
	client, tok := rawAdminClient(t, baseURL)
	const password = "Tabs-test-pass-1!"
	userID, err := client.CreateUser(ctx, tok, realm, gocloak.User{
		Username: gocloak.StringP("tabs@example.com"), Email: gocloak.StringP("tabs@example.com"),
		FirstName: gocloak.StringP("Two"), LastName: gocloak.StringP("Tabs"),
		Enabled: gocloak.BoolP(true), EmailVerified: gocloak.BoolP(true),
	})
	require.NoError(t, err)
	require.NoError(t, client.SetPassword(ctx, tok, userID, realm, password, false))

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	redirect := "https://app.example.test/t/acme/"
	tokenURL := baseURL + "/realms/" + realm + "/protocol/openid-connect/token"

	// Tab A signs in; tab B opens later in the same browser and gets its
	// code from the SSO session, without the login form.
	a := authCodePKCE(t, browser, baseURL, realm, redirect, "tabs@example.com", password)
	b := authCodePKCE(t, browser, baseURL, realm, redirect, "tabs@example.com", password)
	require.Equal(t, a["session_state"], b["session_state"], "both tabs share one SSO session")
	tabs := []*tab{{"A", a["refresh_token"].(string)}, {"B", b["refresh_token"].(string)}}
	firstA := tabs[0].refresh

	refresh := func(tb *tab, want int) map[string]any {
		return tokenCall(t, tokenURL, url.Values{
			"grant_type": {"refresh_token"}, "client_id": {"tender-web"}, "refresh_token": {tb.refresh},
		}, want)
	}

	// Alternate A, B, A, B … each always using its own latest token, a
	// little over a second apart (iat has one-second resolution).
	const rounds = 5
	for i := 0; i < rounds; i++ {
		for _, tb := range tabs {
			time.Sleep(1100 * time.Millisecond)
			out := refresh(tb, http.StatusOK)
			next, _ := out["refresh_token"].(string)
			require.NotEmpty(t, next, "round %d tab %s: no rotated refresh token", i, tb.name)
			require.NotEqual(t, tb.refresh, next, "round %d tab %s: refresh token not rotated", i, tb.name)
			tb.refresh = next
		}
	}

	// Reuse protection still holds inside a lineage: tab A's first, long
	// since rotated, refresh token is refused. Checked last because a
	// detected replay revokes the whole SSO session — both tabs' current
	// tokens stop working too.
	time.Sleep(1100 * time.Millisecond)
	out := refresh(&tab{"A-replay", firstA}, http.StatusBadRequest)
	require.Equal(t, "invalid_grant", out["error"])
	for _, tb := range tabs {
		out := refresh(tb, http.StatusBadRequest)
		require.Equal(t, "invalid_grant", out["error"], "tab %s after a replay in tab A", tb.name)
	}
}
