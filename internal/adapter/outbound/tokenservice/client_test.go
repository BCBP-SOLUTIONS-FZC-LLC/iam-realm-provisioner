package tokenservice

import (
	"context"
	"encoding/json"
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

type noopLogger struct{}

func (noopLogger) Debug(string, map[string]any) {}
func (noopLogger) Info(string, map[string]any)  {}
func (noopLogger) Warn(string, map[string]any)  {}
func (noopLogger) Error(string, map[string]any) {}

// ── JWKSURLFor ────────────────────────────────────────────────────────────

func TestJWKSURLFor(t *testing.T) {
	tenantID := uuid.New()
	c := New("http://token-service.iam.svc.cluster.local", noopLogger{})
	got := c.JWKSURLFor(tenantID)
	want := fmt.Sprintf("http://token-service.iam.svc.cluster.local/api/v1/internal/tenants/%s/service-accounts/platform-automation/jwks.json", tenantID)
	assert.Equal(t, want, got)
}

// ── newRequest ────────────────────────────────────────────────────────────

func TestNewRequest_MarshalErrorPropagates(t *testing.T) {
	c := New("http://example.com", noopLogger{})
	// A channel value cannot be JSON-marshaled.
	_, err := c.newRequest(context.Background(), http.MethodPost, "http://example.com", make(chan int))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encode request body")
}

func TestNewRequest_InvalidRequestPropagatesError(t *testing.T) {
	c := New("http://example.com", noopLogger{})
	_, err := c.newRequest(context.Background(), "IN VALID", "http://example.com", nil)
	require.Error(t, err)
}

func TestNewRequest_NilBodySetsNoContentType(t *testing.T) {
	c := New("http://example.com", noopLogger{})
	req, err := c.newRequest(context.Background(), http.MethodGet, "http://example.com", nil)
	require.NoError(t, err)
	assert.Empty(t, req.Header.Get("Content-Type"))
}

// ── RegisterPrincipal (TS-4) ─────────────────────────────────────────────

// A malformed baseURL makes newRequest's own http.NewRequestWithContext
// call fail — the one newRequest-error branch RegisterPrincipal/
// IssueOrRotateCredential/ReadPrincipal each propagate unchanged.
func TestRegisterPrincipal_InvalidURLPropagatesNewRequestError(t *testing.T) {
	c := New("http://exa\x7fmple.com", noopLogger{})
	_, err := c.RegisterPrincipal(context.Background(), uuid.New(), port.RegisterPrincipalRequest{
		PrincipalSub: uuid.New(), KeycloakClientID: "platform-automation",
	})
	require.Error(t, err)
}

func TestRegisterPrincipal_RequestShapeAndHeaders(t *testing.T) {
	tenantID, principalSub := uuid.New(), uuid.New()
	var gotPath, gotMethod string
	var gotHeaders http.Header
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotHeaders = r.Header.Clone()
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"principal_id":"%s","tenant_id":"%s","principal_type":"platform_automation","status":"active","record_version":1}`,
			uuid.New(), tenantID)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	res, err := c.RegisterPrincipal(context.Background(), tenantID, port.RegisterPrincipalRequest{
		PrincipalSub: principalSub, KeycloakClientID: "platform-automation",
	})
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, fmt.Sprintf("/api/v1/internal/tenants/%s/service-accounts", tenantID), gotPath)
	assert.Equal(t, "00000000-0000-0000-0000-0000000000a1", gotHeaders.Get("x-user-id"))
	assert.Equal(t, tenantID.String(), gotHeaders.Get("x-tenant-id"))
	assert.Equal(t, principalSub.String(), gotBody["principal_sub"])
	assert.Equal(t, "platform-automation", gotBody["keycloak_client_id"])
	assert.True(t, res.Created, "201 must report Created=true")
}

func TestRegisterPrincipal_IdempotentRepeatReportsNotCreated(t *testing.T) {
	tenantID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"principal_id":"%s","tenant_id":"%s","principal_type":"platform_automation","status":"active","record_version":2}`,
			uuid.New(), tenantID)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	res, err := c.RegisterPrincipal(context.Background(), tenantID, port.RegisterPrincipalRequest{
		PrincipalSub: uuid.New(), KeycloakClientID: "platform-automation",
	})
	require.NoError(t, err)
	assert.False(t, res.Created, "200 must report Created=false")
}

func TestRegisterPrincipal_ServerErrorMapsToTokenServiceUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.RegisterPrincipal(context.Background(), uuid.New(), port.RegisterPrincipalRequest{
		PrincipalSub: uuid.New(), KeycloakClientID: "platform-automation",
	})
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.Equal(t, domain.ErrTokenServiceUnavailable, derr.Code)
}

func TestRegisterPrincipal_UnexpectedStatusIsPlainError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.RegisterPrincipal(context.Background(), uuid.New(), port.RegisterPrincipalRequest{
		PrincipalSub: uuid.New(), KeycloakClientID: "platform-automation",
	})
	require.Error(t, err)
	var derr *domain.Error
	assert.False(t, errors.As(err, &derr), "a 4xx other than 409 must not be a domain.Error")
}

func TestRegisterPrincipal_TransportErrorMapsToTokenServiceUnavailable(t *testing.T) {
	c := New("http://127.0.0.1:0", noopLogger{})
	_, err := c.RegisterPrincipal(context.Background(), uuid.New(), port.RegisterPrincipalRequest{
		PrincipalSub: uuid.New(), KeycloakClientID: "platform-automation",
	})
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.Equal(t, domain.ErrTokenServiceUnavailable, derr.Code)
}

func TestRegisterPrincipal_DecodeErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.RegisterPrincipal(context.Background(), uuid.New(), port.RegisterPrincipalRequest{
		PrincipalSub: uuid.New(), KeycloakClientID: "platform-automation",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode TS-4 response")
}

// ── IssueOrRotateCredential (TS-1) ────────────────────────────────────────

func TestIssueOrRotateCredential_RequestShapeAndResponse(t *testing.T) {
	tenantID, principalID, rotationID := uuid.New(), uuid.New(), uuid.New()
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"version":1,"secret":"s3cr3t","openbao_path":"iam/serviceaccount/x/v1","record_version":1}`)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	res, err := c.IssueOrRotateCredential(context.Background(), tenantID, principalID, port.IssueCredentialRequest{RotationID: rotationID})
	require.NoError(t, err)

	assert.Equal(t, fmt.Sprintf("/api/v1/internal/tenants/%s/service-accounts/%s/credentials", tenantID, principalID), gotPath)
	assert.Equal(t, rotationID.String(), gotBody["rotation_id"])
	assert.Equal(t, "s3cr3t", res.Secret)
	assert.Equal(t, 1, res.Version)
	assert.False(t, res.Replayed, "201 must not report Replayed=true")
}

func TestIssueOrRotateCredential_ReplayReportsReplayedTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"version":1,"secret":"s3cr3t","openbao_path":"iam/serviceaccount/x/v1","record_version":1}`)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	res, err := c.IssueOrRotateCredential(context.Background(), uuid.New(), uuid.New(), port.IssueCredentialRequest{RotationID: uuid.New()})
	require.NoError(t, err)
	assert.True(t, res.Replayed)
}

func TestIssueOrRotateCredential_ExpiresPriorAtParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"version":2,"secret":"s3cr3t","openbao_path":"iam/serviceaccount/x/v2","expires_prior_at":"2026-09-17T00:00:00Z","record_version":2}`)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	res, err := c.IssueOrRotateCredential(context.Background(), uuid.New(), uuid.New(), port.IssueCredentialRequest{RotationID: uuid.New()})
	require.NoError(t, err)
	require.NotNil(t, res.ExpiresPriorAt)
	assert.Equal(t, 2026, res.ExpiresPriorAt.Year())
}

func TestIssueOrRotateCredential_ExpiresPriorAtMalformedErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"version":2,"secret":"s3cr3t","openbao_path":"x","expires_prior_at":"not-a-date","record_version":2}`)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.IssueOrRotateCredential(context.Background(), uuid.New(), uuid.New(), port.IssueCredentialRequest{RotationID: uuid.New()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expires_prior_at")
}

// An empty/unparseable 409 body falls back to the safe default —
// rotation-in-flight, not a hard failure.
func TestIssueOrRotateCredential_ConflictWithNoBody_DefaultsToRotationInFlight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.IssueOrRotateCredential(context.Background(), uuid.New(), uuid.New(), port.IssueCredentialRequest{RotationID: uuid.New()})
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.Equal(t, domain.ErrServiceAccountRotationInFlight, derr.Code)
}

func TestIssueOrRotateCredential_ConflictWithRotationInFlightBody_MapsToRotationInFlight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"rotation_in_flight","status":409}`))
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.IssueOrRotateCredential(context.Background(), uuid.New(), uuid.New(), port.IssueCredentialRequest{RotationID: uuid.New()})
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.Equal(t, domain.ErrServiceAccountRotationInFlight, derr.Code)
}

// TS-1's own second 409 cause (§17 of the Token Service LLD) — a
// record_version mismatch on the principal/credential row, unrelated to a
// concurrent rotation — must surface as the distinct
// domain.ErrOptimisticLockConflict, not be flattened into
// ErrServiceAccountRotationInFlight.
func TestIssueOrRotateCredential_ConflictWithOptimisticLockBody_MapsToOptimisticLockConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"optimistic_lock_conflict","status":409}`))
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.IssueOrRotateCredential(context.Background(), uuid.New(), uuid.New(), port.IssueCredentialRequest{RotationID: uuid.New()})
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.Equal(t, domain.ErrOptimisticLockConflict, derr.Code)
}

func TestIssueOrRotateCredential_ServerErrorMapsToTokenServiceUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.IssueOrRotateCredential(context.Background(), uuid.New(), uuid.New(), port.IssueCredentialRequest{RotationID: uuid.New()})
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.Equal(t, domain.ErrTokenServiceUnavailable, derr.Code)
}

func TestIssueOrRotateCredential_InvalidURLPropagatesNewRequestError(t *testing.T) {
	c := New("http://exa\x7fmple.com", noopLogger{})
	_, err := c.IssueOrRotateCredential(context.Background(), uuid.New(), uuid.New(), port.IssueCredentialRequest{RotationID: uuid.New()})
	require.Error(t, err)
}

func TestIssueOrRotateCredential_TransportErrorMapsToTokenServiceUnavailable(t *testing.T) {
	c := New("http://127.0.0.1:0", noopLogger{})
	_, err := c.IssueOrRotateCredential(context.Background(), uuid.New(), uuid.New(), port.IssueCredentialRequest{RotationID: uuid.New()})
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.Equal(t, domain.ErrTokenServiceUnavailable, derr.Code)
}

func TestIssueOrRotateCredential_DecodeErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.IssueOrRotateCredential(context.Background(), uuid.New(), uuid.New(), port.IssueCredentialRequest{RotationID: uuid.New()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode TS-1 response")
}

func TestIssueOrRotateCredential_OverlapSecondsOmittedWhenNil(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"version":1,"secret":"s","openbao_path":"x","record_version":1}`)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.IssueOrRotateCredential(context.Background(), uuid.New(), uuid.New(), port.IssueCredentialRequest{RotationID: uuid.New()})
	require.NoError(t, err)
	_, present := gotBody["overlap_seconds"]
	assert.False(t, present)
}

// ── ReadPrincipal (TS-3, RP-18) ───────────────────────────────────────────

func TestReadPrincipal_Success(t *testing.T) {
	tenantID, principalID := uuid.New(), uuid.New()
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"principal_id":"%s","tenant_id":"%s","keycloak_client_id":"platform-automation","principal_type":"platform_automation","status":"active","record_version":3,
			"credentials":[{"version":1,"status":"rotating","openbao_path":"x/v1","issued_at":"2026-09-01T00:00:00Z","expires_at":"2026-09-10T00:00:00Z"},
			              {"version":2,"status":"active","openbao_path":"x/v2","issued_at":"2026-09-10T00:00:00Z"}]}`,
			principalID, tenantID)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	res, err := c.ReadPrincipal(context.Background(), tenantID, principalID)
	require.NoError(t, err)

	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, fmt.Sprintf("/api/v1/internal/tenants/%s/service-accounts/%s", tenantID, principalID), gotPath)
	require.Len(t, res.Credentials, 2)
	assert.Equal(t, "rotating", res.Credentials[0].Status)
	require.NotNil(t, res.Credentials[0].ExpiresAt)
	assert.Nil(t, res.Credentials[1].ExpiresAt)
}

func TestReadPrincipal_IssuedAtMalformedErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"principal_id":"`+uuid.New().String()+`","tenant_id":"`+uuid.New().String()+`","keycloak_client_id":"platform-automation",
			"principal_type":"platform_automation","status":"active","record_version":1,
			"credentials":[{"version":1,"status":"active","openbao_path":"x","issued_at":"not-a-date"}]}`)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.ReadPrincipal(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "issued_at")
}

func TestReadPrincipal_InvalidURLPropagatesNewRequestError(t *testing.T) {
	c := New("http://exa\x7fmple.com", noopLogger{})
	_, err := c.ReadPrincipal(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
}

func TestReadPrincipal_TransportErrorMapsToTokenServiceUnavailable(t *testing.T) {
	c := New("http://127.0.0.1:0", noopLogger{})
	_, err := c.ReadPrincipal(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.Equal(t, domain.ErrTokenServiceUnavailable, derr.Code)
}

func TestReadPrincipal_ExpiresAtMalformedErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"principal_id":"`+uuid.New().String()+`","tenant_id":"`+uuid.New().String()+`","keycloak_client_id":"platform-automation",
			"principal_type":"platform_automation","status":"active","record_version":1,
			"credentials":[{"version":1,"status":"active","openbao_path":"x","issued_at":"2026-09-01T00:00:00Z","expires_at":"not-a-date"}]}`)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.ReadPrincipal(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expires_at")
}

func TestReadPrincipal_ServerErrorMapsToTokenServiceUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.ReadPrincipal(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.Equal(t, domain.ErrTokenServiceUnavailable, derr.Code)
}

func TestReadPrincipal_DecodeErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.ReadPrincipal(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode TS-3 response")
}
