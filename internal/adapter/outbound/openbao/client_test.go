package openbao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

type noopLogger struct{}

func (noopLogger) Debug(string, map[string]any) {}
func (noopLogger) Info(string, map[string]any)  {}
func (noopLogger) Warn(string, map[string]any)  {}
func (noopLogger) Error(string, map[string]any) {}

// writeFakeSAToken writes a fake Kubernetes ServiceAccount JWT to a temp
// file and returns its path, so Client.token() can read it without a real
// cluster.
func writeFakeSAToken(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte("fake.jwt.token\n"), 0o600))
	return path
}

// fakeBao is a minimal in-memory OpenBao server: Kubernetes auth login +
// KV v2 data/metadata read-write-delete, with hooks to force specific
// failure modes.
type fakeBao struct {
	mu           sync.Mutex
	loginCalls   int
	loginFail    bool
	loginBadJSON bool
	kv           map[string]map[string]any // logical path -> secret data
	lastLogin    struct{ Role, JWT string }
	seenPaths    []string // every requested URL path, in order
	writeStatus  int      // non-zero: force this status on the KV write endpoint
	readStatus   int      // non-zero: force this status on the KV read endpoint
	readBadJSON  bool     // force a malformed JSON body on a successful read
	deleteStatus int      // non-zero: force this status on the KV metadata-delete endpoint
}

func newFakeBao() *fakeBao { return &fakeBao{kv: map[string]map[string]any{}} }

func (f *fakeBao) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seenPaths = append(f.seenPaths, r.URL.Path)
		f.mu.Unlock()

		switch {
		case r.URL.Path == "/v1/auth/kubernetes/login" && r.Method == http.MethodPost:
			f.mu.Lock()
			f.loginCalls++
			fail := f.loginFail
			f.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
				return
			}
			var body struct{ Role, JWT string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.lastLogin = body
			badJSON := f.loginBadJSON
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			if badJSON {
				_, _ = w.Write([]byte(`not-json`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"auth": map[string]any{"client_token": "fake-client-token", "lease_duration": 3600},
			})

		case r.Method == http.MethodPost && matchKVData(r.URL.Path):
			f.mu.Lock()
			forced := f.writeStatus
			f.mu.Unlock()
			if forced != 0 {
				w.WriteHeader(forced)
				_, _ = w.Write([]byte(`{"errors":["simulated write failure"]}`))
				return
			}
			logicalPath := stripKVPrefix(r.URL.Path, "/v1/secret/data/")
			var body struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.kv[logicalPath] = body.Data
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"version":1}}`))

		case r.Method == http.MethodGet && matchKVData(r.URL.Path):
			f.mu.Lock()
			forced := f.readStatus
			badJSON := f.readBadJSON
			f.mu.Unlock()
			if forced != 0 {
				w.WriteHeader(forced)
				_, _ = w.Write([]byte(`{"errors":["simulated read failure"]}`))
				return
			}
			logicalPath := stripKVPrefix(r.URL.Path, "/v1/secret/data/")
			f.mu.Lock()
			data, ok := f.kv[logicalPath]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"errors":[]}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			if badJSON {
				_, _ = w.Write([]byte(`not-json`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": data}})

		case r.Method == http.MethodDelete && matchKVMetadata(r.URL.Path):
			f.mu.Lock()
			forced := f.deleteStatus
			f.mu.Unlock()
			if forced != 0 {
				w.WriteHeader(forced)
				_, _ = w.Write([]byte(`{"errors":["simulated delete failure"]}`))
				return
			}
			logicalPath := stripKVPrefix(r.URL.Path, "/v1/secret/metadata/")
			f.mu.Lock()
			delete(f.kv, logicalPath)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func matchKVData(path string) bool     { return hasPrefix(path, "/v1/secret/data/") }
func matchKVMetadata(path string) bool { return hasPrefix(path, "/v1/secret/metadata/") }
func hasPrefix(s, p string) bool       { return len(s) >= len(p) && s[:len(p)] == p }
func stripKVPrefix(s, p string) string { return s[len(p):] }

func newTestClient(t *testing.T, bao *fakeBao) (*Client, *httptest.Server) {
	t.Helper()
	srv := bao.server(t)
	t.Cleanup(srv.Close)
	c := New(Config{
		Addr:                srv.URL,
		AuthRole:            "iam-realm-provisioner",
		KVMount:             "secret",
		IdpPathPrefix:       "iam/realm-provisioner/idp",
		KubernetesTokenPath: writeFakeSAToken(t),
	}, noopLogger{})
	return c, srv
}

// TestIdpPath_ExactDeterministicShape locks down the security-sensitive KV
// path construction documented in SECURITY.md:
// secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias> (the mount
// "secret" + "/data/" prefix are added by writeKV/readKV, idpPath itself
// builds the logical path under the mount).
func TestIdpPath_ExactDeterministicShape(t *testing.T) {
	c := &Client{cfg: Config{IdpPathPrefix: "iam/realm-provisioner/idp"}}
	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	got := c.idpPath(tenantID, "okta-saml")
	want := "iam/realm-provisioner/idp/11111111-1111-1111-1111-111111111111/okta-saml"
	assert.Equal(t, want, got)
}

// TestWriteIdpSecret_UsesExactKVDataPath confirms the full HTTP request path
// hit for a write is exactly /v1/secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>.
func TestWriteIdpSecret_UsesExactKVDataPath(t *testing.T) {
	bao := newFakeBao()
	c, _ := newTestClient(t, bao)
	tenantID := uuid.New()

	err := c.WriteIdpSecret(context.Background(), tenantID, "azure-oidc", map[string]any{"client_secret": "s3cr3t"})
	require.NoError(t, err)

	wantPath := fmt.Sprintf("/v1/secret/data/iam/realm-provisioner/idp/%s/azure-oidc", tenantID)
	bao.mu.Lock()
	defer bao.mu.Unlock()
	assert.Contains(t, bao.seenPaths, wantPath)
	stored, ok := bao.kv[fmt.Sprintf("iam/realm-provisioner/idp/%s/azure-oidc", tenantID)]
	require.True(t, ok)
	assert.Equal(t, "s3cr3t", stored["client_secret"])
}

func TestReadIdpSecret_RoundTrip(t *testing.T) {
	bao := newFakeBao()
	c, _ := newTestClient(t, bao)
	tenantID := uuid.New()

	require.NoError(t, c.WriteIdpSecret(context.Background(), tenantID, "okta-saml", map[string]any{"cert": "abc"}))

	got, err := c.ReadIdpSecret(context.Background(), tenantID, "okta-saml")
	require.NoError(t, err)
	assert.Equal(t, "abc", got["cert"])
}

func TestReadIdpSecret_NotFoundReturnsDomainError(t *testing.T) {
	bao := newFakeBao()
	c, _ := newTestClient(t, bao)

	_, err := c.ReadIdpSecret(context.Background(), uuid.New(), "missing-alias")
	require.Error(t, err)
	var domErr *domain.Error
	require.ErrorAs(t, err, &domErr)
	assert.Equal(t, domain.ErrIdpConfigNotFound, domErr.Code)
	assert.Equal(t, 404, domErr.Status())
}

func TestDeleteIdpSecret_UsesMetadataPathAndSucceeds(t *testing.T) {
	bao := newFakeBao()
	c, _ := newTestClient(t, bao)
	tenantID := uuid.New()
	require.NoError(t, c.WriteIdpSecret(context.Background(), tenantID, "okta-saml", map[string]any{"x": "y"}))

	require.NoError(t, c.DeleteIdpSecret(context.Background(), tenantID, "okta-saml"))

	wantPath := fmt.Sprintf("/v1/secret/metadata/iam/realm-provisioner/idp/%s/okta-saml", tenantID)
	bao.mu.Lock()
	defer bao.mu.Unlock()
	assert.Contains(t, bao.seenPaths, wantPath)
	_, stillThere := bao.kv[fmt.Sprintf("iam/realm-provisioner/idp/%s/okta-saml", tenantID)]
	assert.False(t, stillThere)
}

// TestDeleteIdpSecret_AlreadyGoneIsNotAnError confirms a 404 on delete
// (already-deleted secret) is treated as success, matching RP-7's
// idempotent-remove contract.
func TestDeleteIdpSecret_AlreadyGoneIsNotAnError(t *testing.T) {
	bao := newFakeBao()
	c, _ := newTestClient(t, bao)
	require.NoError(t, c.DeleteIdpSecret(context.Background(), uuid.New(), "never-existed"))
}

func TestToken_CachedAcrossCalls(t *testing.T) {
	bao := newFakeBao()
	c, _ := newTestClient(t, bao)
	tenantID := uuid.New()

	require.NoError(t, c.WriteIdpSecret(context.Background(), tenantID, "a", map[string]any{"x": 1}))
	require.NoError(t, c.WriteIdpSecret(context.Background(), tenantID, "b", map[string]any{"x": 2}))

	bao.mu.Lock()
	defer bao.mu.Unlock()
	assert.Equal(t, 1, bao.loginCalls, "second write must reuse the cached token, not log in again")
	assert.Equal(t, "iam-realm-provisioner", bao.lastLogin.Role)
	assert.Equal(t, "fake.jwt.token", bao.lastLogin.JWT)
}

func TestToken_ExpiredTokenTriggersRelogin(t *testing.T) {
	bao := newFakeBao()
	c, _ := newTestClient(t, bao)
	// Force the cached token to look expired.
	c.cachedToken = "stale-token"
	c.expiresAt = time.Now().Add(-time.Minute)

	require.NoError(t, c.WriteIdpSecret(context.Background(), uuid.New(), "a", map[string]any{"x": 1}))

	bao.mu.Lock()
	defer bao.mu.Unlock()
	assert.Equal(t, 1, bao.loginCalls)
	assert.NotEqual(t, "stale-token", c.cachedToken)
}

func TestToken_AuthFailurePropagates(t *testing.T) {
	bao := newFakeBao()
	bao.loginFail = true
	c, _ := newTestClient(t, bao)

	_, err := c.ReadIdpSecret(context.Background(), uuid.New(), "a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kubernetes_login")
}

func TestToken_MissingServiceAccountFileErrors(t *testing.T) {
	c := New(Config{
		Addr:                "http://unused.invalid",
		AuthRole:            "iam-realm-provisioner",
		KubernetesTokenPath: filepath.Join(t.TempDir(), "does-not-exist"),
	}, noopLogger{})

	_, err := c.token(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read kubernetes service account token")
}

func TestNew_DefaultsAppliedWhenUnset(t *testing.T) {
	c := New(Config{Addr: "http://x"}, noopLogger{})
	assert.Equal(t, "/var/run/secrets/kubernetes.io/serviceaccount/token", c.cfg.KubernetesTokenPath)
	require.NotNil(t, c.http)
	assert.Equal(t, 8*time.Second, c.http.Timeout)
}

func TestWriteIdpSecret_TokenErrorPropagates(t *testing.T) {
	bao := newFakeBao()
	bao.loginFail = true
	c, _ := newTestClient(t, bao)

	err := c.WriteIdpSecret(context.Background(), uuid.New(), "a", map[string]any{"x": 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kubernetes_login")
}

func TestDeleteIdpSecret_TokenErrorPropagates(t *testing.T) {
	bao := newFakeBao()
	bao.loginFail = true
	c, _ := newTestClient(t, bao)

	err := c.DeleteIdpSecret(context.Background(), uuid.New(), "a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kubernetes_login")
}

// TestWriteIdpSecret_UnmarshalableSecretFailsBeforeAnyHTTPCall exercises
// writeKV's json.Marshal error branch — a channel value can never be
// JSON-encoded.
func TestWriteIdpSecret_UnmarshalableSecretFailsBeforeAnyHTTPCall(t *testing.T) {
	bao := newFakeBao()
	c, _ := newTestClient(t, bao)

	err := c.WriteIdpSecret(context.Background(), uuid.New(), "a", map[string]any{"bad": make(chan int)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "marshal write_secret body")
}

// withCachedToken pre-seeds c with a live cached token so token() returns
// immediately without an HTTP round trip — isolates the KV-request-building
// and KV-transport error branches (writeKV/readKV/deleteKV) from token()'s
// own equivalent branches, which share the same Config.Addr.
func withCachedToken(c *Client) {
	c.cachedToken = "cached-token"
	c.expiresAt = time.Now().Add(time.Hour)
}

func TestKVCalls_MalformedAddrFailsRequestConstruction(t *testing.T) {
	c := New(Config{
		Addr:                "http://\x7f-bad",
		KVMount:             "secret",
		IdpPathPrefix:       "iam/realm-provisioner/idp",
		KubernetesTokenPath: writeFakeSAToken(t),
	}, noopLogger{})
	withCachedToken(c)

	tenantID, alias := uuid.New(), "a"
	assert.Error(t, c.WriteIdpSecret(context.Background(), tenantID, alias, map[string]any{"x": 1}))
	_, err := c.ReadIdpSecret(context.Background(), tenantID, alias)
	assert.Error(t, err)
	assert.Error(t, c.DeleteIdpSecret(context.Background(), tenantID, alias))
}

func TestKVCalls_UnreachableAddrWrapsTransportError(t *testing.T) {
	deadSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadAddr := deadSrv.URL
	deadSrv.Close() // connection refused on any subsequent call

	c := New(Config{
		Addr:                deadAddr,
		KVMount:             "secret",
		IdpPathPrefix:       "iam/realm-provisioner/idp",
		KubernetesTokenPath: writeFakeSAToken(t),
	}, noopLogger{})
	withCachedToken(c)

	tenantID, alias := uuid.New(), "a"
	err := c.WriteIdpSecret(context.Background(), tenantID, alias, map[string]any{"x": 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write_secret")

	_, err = c.ReadIdpSecret(context.Background(), tenantID, alias)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read_secret")

	err = c.DeleteIdpSecret(context.Background(), tenantID, alias)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete_secret")
}

func TestToken_UnreachableAddrWrapsTransportError(t *testing.T) {
	deadSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadAddr := deadSrv.URL
	deadSrv.Close()

	c := New(Config{
		Addr:                deadAddr,
		AuthRole:            "iam-realm-provisioner",
		KubernetesTokenPath: writeFakeSAToken(t),
	}, noopLogger{})

	_, err := c.token(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kubernetes_login")
}

func TestKVCalls_GenericErrorStatusPropagates(t *testing.T) {
	bao := newFakeBao()
	bao.writeStatus = http.StatusInternalServerError
	bao.readStatus = http.StatusInternalServerError
	bao.deleteStatus = http.StatusInternalServerError
	c, _ := newTestClient(t, bao)
	tenantID, alias := uuid.New(), "a"

	err := c.WriteIdpSecret(context.Background(), tenantID, alias, map[string]any{"x": 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write_secret")

	_, err = c.ReadIdpSecret(context.Background(), tenantID, alias)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read_secret")

	err = c.DeleteIdpSecret(context.Background(), tenantID, alias)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete_secret")
}

func TestReadIdpSecret_MalformedResponseBodyReturnsDecodeError(t *testing.T) {
	bao := newFakeBao()
	c, _ := newTestClient(t, bao)
	tenantID := uuid.New()
	require.NoError(t, c.WriteIdpSecret(context.Background(), tenantID, "a", map[string]any{"x": 1}))

	bao.readBadJSON = true
	_, err := c.ReadIdpSecret(context.Background(), tenantID, "a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode read_secret response")
}

func TestToken_MalformedLoginResponseBodyReturnsDecodeError(t *testing.T) {
	bao := newFakeBao()
	bao.loginBadJSON = true
	c, _ := newTestClient(t, bao)

	_, err := c.token(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode kubernetes_login response")
}

// erroringBody wraps a real body's Read but fails Close, so closeBody's
// log-only error path can be exercised.
type erroringBody struct{ io.Reader }

func (erroringBody) Close() error { return errors.New("simulated close failure") }

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type capturingLogger struct{ warned map[string]any }

func (*capturingLogger) Debug(string, map[string]any)           {}
func (*capturingLogger) Info(string, map[string]any)            {}
func (l *capturingLogger) Warn(_ string, fields map[string]any) { l.warned = fields }
func (*capturingLogger) Error(string, map[string]any)           {}

func TestWriteIdpSecret_BodyCloseFailureIsLoggedNotPropagated(t *testing.T) {
	logger := &capturingLogger{}
	c := New(Config{
		Addr:                "http://unused.invalid",
		KVMount:             "secret",
		IdpPathPrefix:       "iam/realm-provisioner/idp",
		KubernetesTokenPath: writeFakeSAToken(t),
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       erroringBody{strings.NewReader(`{"data":{"version":1}}`)},
			}, nil
		})},
	}, logger)
	withCachedToken(c)

	err := c.WriteIdpSecret(context.Background(), uuid.New(), "a", map[string]any{"x": 1})
	require.NoError(t, err)
	require.NotNil(t, logger.warned, "a response-body Close failure must be logged via Warn")
	assert.Contains(t, logger.warned["error"], "simulated close failure")
}
