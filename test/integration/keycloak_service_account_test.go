//go:build integration

// This file validates EXT-6's acceptance criterion against a real Keycloak:
// that CreateServiceAccountClient's client-jwt/jwks.url mechanism gives the
// platform-automation principal genuine multi-key rotation overlap — unlike
// plain client-secret auth (single value, no grace window, proven false in
// an earlier revision of this file; see the EXT-6 revision-history row in
// docs/lld/iam-lld-realm-provisioner-service.md). It also proves the two
// operational corrections EXT-6 found: Keycloak does not self-refresh a
// jwks.url client's cached keys — neither a newly-added key nor a removed
// one takes effect until ClearServiceAccountKeysCache is called — except
// that a client's FIRST key needs no such warm-up (fetched lazily on first
// real use).
package integration_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// testJWKSKey is one RSA keypair this harness can serve and sign with.
type testJWKSKey struct {
	kid  string
	priv *rsa.PrivateKey
}

func mustGenerateTestJWKSKey(t *testing.T, kid string) testJWKSKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return testJWKSKey{kid: kid, priv: priv}
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jwkFor(k testJWKSKey) map[string]any {
	pub := k.priv.PublicKey
	return map[string]any{
		"kty": "RSA",
		"kid": k.kid,
		"use": "sig",
		"alg": "RS256",
		"n":   b64url(pub.N.Bytes()),
		"e":   b64url(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// signClientAssertion builds an RFC7523 client_assertion JWT (RS256) for a
// client-jwt (private_key_jwt) client-credentials grant.
func signClientAssertion(t *testing.T, k testJWKSKey, clientID, tokenEndpoint string) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": k.kid}
	now := time.Now().Unix()
	claims := map[string]any{
		"iss": clientID, "sub": clientID, "aud": tokenEndpoint,
		"exp": now + 60, "iat": now, "jti": uuid.NewString(),
	}
	hB, err := json.Marshal(header)
	require.NoError(t, err)
	cB, err := json.Marshal(claims)
	require.NoError(t, err)
	signingInput := b64url(hB) + "." + b64url(cB)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.priv, crypto.SHA256, digest[:])
	require.NoError(t, err)
	return signingInput + "." + b64url(sig)
}

// jwksTestServer serves an in-process JWKS at a fixed 0.0.0.0-bound port so
// the Keycloak container (via host.docker.internal, see
// startKeycloakContainer's ExtraHosts) can fetch it. The served key set is
// mutable so the test can simulate add-key/remove-key without restarting
// the server.
type jwksTestServer struct {
	mu     sync.Mutex
	served []testJWKSKey
	url    string
}

func startJWKSTestServer(t *testing.T, initial ...testJWKSKey) *jwksTestServer {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)

	s := &jwksTestServer{served: initial}
	mux := http.NewServeMux()
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		keys := make([]map[string]any, 0, len(s.served))
		for _, k := range s.served {
			keys = append(keys, jwkFor(k))
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})

	srv := httptest.NewUnstartedServer(mux)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	port := ln.Addr().(*net.TCPAddr).Port
	s.url = fmt.Sprintf("http://host.docker.internal:%d/jwks.json", port)
	return s
}

func (s *jwksTestServer) setServed(keys ...testJWKSKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.served = keys
}

// clientCredentialsTokenViaJWT attempts a private_key_jwt client-credentials
// grant for clientID, signed with k, and reports whether Keycloak accepted
// it — the only way to prove a key actually took effect at the auth plane,
// rather than merely that an admin call returned 2xx.
func clientCredentialsTokenViaJWT(t *testing.T, baseURL, realmName, clientID string, k testJWKSKey) (ok bool, status int) {
	t.Helper()
	tokenEndpoint := baseURL + "/realms/" + realmName + "/protocol/openid-connect/token"
	assertion := signClientAssertion(t, k, clientID, tokenEndpoint)
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	resp, err := http.Post(tokenEndpoint, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
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

func TestKeycloakAdmin_ServiceAccountClient_ClientJWTOverlap(t *testing.T) {
	t.Parallel()
	admin, baseURL := setupKeycloakWithURL(t)
	ctx := context.Background()
	realmName := "tenant-" + uuid.NewString()[:8]
	require.NoError(t, admin.CreateRealmFromTemplate(ctx, realmName, "v1"))
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, realmName) })

	clientID := fmt.Sprintf("platform-automation-%s", uuid.NewString())
	key1 := mustGenerateTestJWKSKey(t, "key1")
	key2 := mustGenerateTestJWKSKey(t, "key2")

	jwks := startJWKSTestServer(t, key1)

	sub, created, err := admin.CreateServiceAccountClient(ctx, realmName, clientID, jwks.url)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, sub)
	require.True(t, created, "a brand-new client must report created=true")

	// Idempotent resume: a repeat call returns the same sub, does not
	// create a second client.
	sub2, created2, err := admin.CreateServiceAccountClient(ctx, realmName, clientID, jwks.url)
	require.NoError(t, err)
	require.Equal(t, sub, sub2)
	require.False(t, created2, "a resumed client must report created=false")

	// (a) A brand-new client's first key needs no cache-clear/pre-warming —
	// Keycloak fetches the JWKS lazily on first real use.
	ok, _ := clientCredentialsTokenViaJWT(t, baseURL, realmName, clientID, key1)
	require.True(t, ok, "key1 must authenticate immediately, with no ClearServiceAccountKeysCache call")

	// (b) Adding key2 to the served JWKS is NOT enough on its own — Keycloak
	// has already cached the (empty, at fetch time) key set and does not
	// self-refresh.
	jwks.setServed(key1, key2)
	ok, _ = clientCredentialsTokenViaJWT(t, baseURL, realmName, clientID, key2)
	require.False(t, ok, "a newly-added key must NOT authenticate before ClearServiceAccountKeysCache")

	// (c) After ClearServiceAccountKeysCache, BOTH keys authenticate —
	// genuine overlap, the core EXT-6 finding.
	require.NoError(t, admin.ClearServiceAccountKeysCache(ctx, realmName))
	ok, _ = clientCredentialsTokenViaJWT(t, baseURL, realmName, clientID, key2)
	require.True(t, ok, "key2 must authenticate once the keys cache is cleared")
	ok, _ = clientCredentialsTokenViaJWT(t, baseURL, realmName, clientID, key1)
	require.True(t, ok, "key1 must still authenticate — both keys are live simultaneously")

	// (d) Removing key1 from the served JWKS is, again, not enough on its
	// own — the cache keeps validating it until explicitly cleared. This is
	// the finding that makes RP-17 a real, required step on every revoke.
	jwks.setServed(key2)
	ok, _ = clientCredentialsTokenViaJWT(t, baseURL, realmName, clientID, key1)
	require.True(t, ok, "a removed key must keep authenticating until ClearServiceAccountKeysCache — revocation is not automatic")

	require.NoError(t, admin.ClearServiceAccountKeysCache(ctx, realmName))
	ok, _ = clientCredentialsTokenViaJWT(t, baseURL, realmName, clientID, key1)
	require.False(t, ok, "the removed key must stop authenticating once the keys cache is cleared")
	ok, _ = clientCredentialsTokenViaJWT(t, baseURL, realmName, clientID, key2)
	require.True(t, ok, "the surviving key must be unaffected by revoking the other")
}
