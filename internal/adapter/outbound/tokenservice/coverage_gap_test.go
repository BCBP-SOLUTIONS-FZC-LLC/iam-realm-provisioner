package tokenservice

// coverage_gap_test.go — tests for classifyStatus, revokeVersion, and the
// RegisterPrincipal 201-with-non-JSON branch that were not covered by
// client_test.go.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// ── classifyStatus ────────────────────────────────────────────────────────

// buildResp is a helper that starts a minimal httptest.Server, performs one
// round-trip (without following redirects), and returns the resulting
// *http.Response.  The caller is responsible for closing the body.
func buildResp(status int, contentType, body string) *http.Response {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = fmt.Fprint(w, body)
		}
	}))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	// Disable redirect-following so that 3xx responses are returned as-is.
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, _ := cl.Do(req)
	srv.Close()
	return resp
}

// TestClassifyStatus_400WithProblemBodySurfacesErrorCode verifies that a
// 400 response whose body is a valid RFC 9457 problem with a non-empty
// errorCode returns a plain (non-domain) error that mentions the code.
func TestClassifyStatus_400WithProblemBodySurfacesErrorCode(t *testing.T) {
	body := `{"type":"https://errors.example.com/iam/invalid-request","title":"Invalid Request","status":400,"detail":"bad","instance":"/x","traceId":"abc","errorCode":"INVALID_REQUEST","timestamp":"2026-11-15T00:00:00Z"}`
	resp := buildResp(http.StatusBadRequest, "application/problem+json", body)
	defer resp.Body.Close()

	err := classifyStatus(resp, "TS-X")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "INVALID_REQUEST", "the RFC 9457 errorCode must appear in the error text")
	var derr *domain.Error
	assert.False(t, errors.As(err, &derr), "a 400 must not produce a domain.Error")
}

// TestClassifyStatus_400WithNonJSONBodyReturnsPlainStatusError verifies that
// a 400 response with a non-JSON (plain-text) body still returns a plain
// error — problem.FromResponse decode fails, so we fall back to the
// "rejected request (status 400)" form.
func TestClassifyStatus_400WithNonJSONBodyReturnsPlainStatusError(t *testing.T) {
	resp := buildResp(http.StatusBadRequest, "text/plain", "bad input")
	defer resp.Body.Close()

	err := classifyStatus(resp, "TS-X")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400", "the status code must appear in the fallback error text")
	var derr *domain.Error
	assert.False(t, errors.As(err, &derr), "a 400 must not produce a domain.Error")
}

// TestClassifyStatus_3xxRedirectReturnsUnexpectedStatusError verifies that
// a 3xx response is treated as an unexpected status when redirects are
// disabled on the calling http.Client.
func TestClassifyStatus_3xxRedirectReturnsUnexpectedStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Set Location manually — we must not pass nil to http.Redirect.
		w.Header().Set("Location", "http://other.invalid/")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := cl.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusFound, resp.StatusCode)
	csErr := classifyStatus(resp, "TS-X")
	require.Error(t, csErr)
	assert.Contains(t, csErr.Error(), "unexpected status 302")
	var derr *domain.Error
	assert.False(t, errors.As(csErr, &derr), "a 3xx must not produce a domain.Error")
}

// TestClassifyStatus_404ReturnServiceAccountNotProvisioned verifies that
// classifyStatus maps a 404 to ErrServiceAccountNotProvisioned.
func TestClassifyStatus_404ReturnsServiceAccountNotProvisioned(t *testing.T) {
	resp := buildResp(http.StatusNotFound, "", "")
	defer resp.Body.Close()

	err := classifyStatus(resp, "TS-X")
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.Equal(t, domain.ErrServiceAccountNotProvisioned, derr.Code)
}

// TestClassifyStatus_2xxReturnsNil verifies that a 2xx status is not an error.
func TestClassifyStatus_2xxReturnsNil(t *testing.T) {
	resp := buildResp(http.StatusOK, "", "")
	defer resp.Body.Close()
	assert.NoError(t, classifyStatus(resp, "TS-X"))
}

// ── revokeVersion (TS-2 inner step) ──────────────────────────────────────

// TestRevokeVersion_204NoContentReturnsNil verifies the happy path: a 204
// (no content, success) from TS-2 is treated as success.
func TestRevokeVersion_204NoContentReturnsNil(t *testing.T) {
	tenantID, principalID := uuid.New(), uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, 0, noopLogger{})
	err := c.revokeVersion(context.Background(), tenantID, principalID, 1)
	require.NoError(t, err)
}

// TestRevokeVersion_200OKReturnsNil verifies that a 200 OK (the shape Token
// Service currently returns) is also success.
func TestRevokeVersion_200OKReturnsNil(t *testing.T) {
	tenantID, principalID := uuid.New(), uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"status":"revoked"}`)
	}))
	defer srv.Close()

	c := New(srv.URL, 0, noopLogger{})
	err := c.revokeVersion(context.Background(), tenantID, principalID, 2)
	require.NoError(t, err)
}

// TestRevokeVersion_404MapsToServiceAccountNotProvisioned verifies that a
// 404 from TS-2 is surfaced as ErrServiceAccountNotProvisioned (via
// classifyStatus).
func TestRevokeVersion_404MapsToServiceAccountNotProvisioned(t *testing.T) {
	tenantID, principalID := uuid.New(), uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, 0, noopLogger{})
	err := c.revokeVersion(context.Background(), tenantID, principalID, 1)
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.Equal(t, domain.ErrServiceAccountNotProvisioned, derr.Code)
}

// TestRevokeVersion_500MapsToTokenServiceUnavailable verifies that a 500
// from TS-2 is surfaced as ErrTokenServiceUnavailable (via classifyStatus).
func TestRevokeVersion_500MapsToTokenServiceUnavailable(t *testing.T) {
	tenantID, principalID := uuid.New(), uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, 0, noopLogger{})
	err := c.revokeVersion(context.Background(), tenantID, principalID, 1)
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.Equal(t, domain.ErrTokenServiceUnavailable, derr.Code)
}

// ── RegisterPrincipal — 201 Created with non-JSON body ───────────────────

// TestRegisterPrincipal_201CreatedWithNonJSONBodyReturnsDecodeError
// exercises the decode-error branch when TS-4 responds 201 (Created) but
// with a body that is not valid JSON — classifyStatus sees a 2xx and
// returns nil, so we reach json.Decoder.Decode and get an error.
func TestRegisterPrincipal_201CreatedWithNonJSONBodyReturnsDecodeError(t *testing.T) {
	tenantID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, "not valid json at all")
	}))
	defer srv.Close()

	c := New(srv.URL, 0, noopLogger{})
	_, err := c.RegisterPrincipal(context.Background(), tenantID, port.RegisterPrincipalRequest{
		PrincipalSub: uuid.New(), KeycloakClientID: "platform-automation",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode TS-4 response",
		"a 201 with a non-JSON body must return a decode error referencing the TS-4 endpoint")
}

// TestRegisterPrincipal_409ConflictReturnsDomainError verifies that a 409
// from TS-4 is mapped to ErrServiceAccountRotationInFlight — a concurrent
// registration write in progress.
func TestRegisterPrincipal_409ConflictReturnsDomainError(t *testing.T) {
	tenantID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	c := New(srv.URL, 0, noopLogger{})
	_, err := c.RegisterPrincipal(context.Background(), tenantID, port.RegisterPrincipalRequest{
		PrincipalSub: uuid.New(), KeycloakClientID: "platform-automation",
	})
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr, "a 409 must produce a *domain.Error")
	assert.Equal(t, domain.ErrServiceAccountRotationInFlight, derr.Code,
		"a 409 from TS-4 must map to ErrServiceAccountRotationInFlight")
}

// TestRevokeVersion_TransportFailureReturnsError verifies that a network-level
// transport error from revokeVersion (the HTTP call in c.do fails) is surfaced
// as a non-nil error. A closed server is used so the connection is refused
// immediately.
func TestRevokeVersion_TransportFailureReturnsError(t *testing.T) {
	tenantID, principalID := uuid.New(), uuid.New()

	// Start a server and immediately close it so the port is unreachable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.Close()

	c := New(srv.URL, 0, noopLogger{})
	err := c.revokeVersion(context.Background(), tenantID, principalID, 1)
	require.Error(t, err, "a transport-level failure must surface as a non-nil error")
	var derr *domain.Error
	require.ErrorAs(t, err, &derr, "c.do wraps transport errors in a domain.Error")
	assert.Equal(t, domain.ErrTokenServiceUnavailable, derr.Code,
		"a transport failure must map to ErrTokenServiceUnavailable")
}
