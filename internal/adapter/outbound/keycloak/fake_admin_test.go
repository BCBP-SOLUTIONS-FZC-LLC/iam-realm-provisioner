package keycloak

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// TestMain shrinks retryTransport's backoff to near-zero for this whole
// test binary. Every fake-admin-backed test in this package goes through
// New's real transport chain (newFakeAdmin calls the production
// constructor directly, deliberately, so these tests exercise the exact
// chain production uses) — without this, every test simulating a 5xx or a
// transport error would sit through retryTransport's real (up to
// several-second) backoff schedule 3 times before returning. Tests that
// need to assert the real backoff *shape* (exponential growth, jitter
// bounds, Retry-After honoring) set their own values and restore these via
// t.Cleanup instead of relying on this reduced default.
func TestMain(m *testing.M) {
	origAttempts, origBase, origMax := retryMaxAttempts, retryBaseDelay, retryMaxDelay
	retryMaxAttempts = 2
	retryBaseDelay = time.Millisecond
	retryMaxDelay = time.Millisecond

	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		Domain:       "iam",
		Environment:  "test",
		ServiceName:  "iam-realm-provisioner",
		BuildVersion: "test",
	})
	metrics.Register()

	code := m.Run()
	retryMaxAttempts, retryBaseDelay, retryMaxDelay = origAttempts, origBase, origMax
	os.Exit(code)
}

// nopLogger is a no-op port.Logger for tests that don't assert on log
// output (e.g. CreateTrialUser's "send verify email failed" WARN path).
type nopLogger struct{}

func (nopLogger) Debug(string, map[string]any) {}
func (nopLogger) Info(string, map[string]any)  {}
func (nopLogger) Warn(string, map[string]any)  {}
func (nopLogger) Error(string, map[string]any) {}

var _ port.Logger = nopLogger{}

// tokenResponse describes one canned response the fake token endpoint
// serves.
type tokenResponse struct {
	status int
	body   string
}

func okTokenResponse() tokenResponse {
	return tokenResponse{status: http.StatusOK, body: `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`}
}

func failTokenResponse() tokenResponse {
	return tokenResponse{status: http.StatusUnauthorized, body: `{"error":"invalid_client","error_description":"bad secret"}`}
}

// sequencedTokenHandler serves responses in order; once exhausted the last
// entry repeats for every subsequent call.
func sequencedTokenHandler(responses []tokenResponse) http.HandlerFunc {
	var n int32
	return func(w http.ResponseWriter, _ *http.Request) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		if i >= len(responses) {
			i = len(responses) - 1
		}
		resp := responses[i]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}
}

// newFakeAdmin builds a *Admin wired to an httptest server that serves the
// client-credentials token endpoint per tokenResponses (defaulting to
// always-OK when nil/empty) plus whatever admin-API routes register adds to
// the mux.
func newFakeAdmin(t *testing.T, tokenResponses []tokenResponse, register func(mux *http.ServeMux)) *Admin {
	t.Helper()
	if len(tokenResponses) == 0 {
		tokenResponses = []tokenResponse{okTokenResponse()}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/master/protocol/openid-connect/token", sequencedTokenHandler(tokenResponses))
	if register != nil {
		register(mux)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL, AdminRealm: "master", ClientID: "cid", ClientSecret: "secret"}, nopLogger{})
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// writeLocation sets the Location header gocloak's getID() parses to
// extract a newly-created resource's id (the last path segment).
func writeLocation(w http.ResponseWriter, id string) {
	w.Header().Set("Location", "http://fake/x/"+id)
}
