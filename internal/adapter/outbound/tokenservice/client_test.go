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

// writeProblem writes an RFC 9457 problem body shaped exactly like Token
// Service's platform-gincommon renderer emits (application/problem+json,
// UPPER_SNAKE errorCode).
func writeProblem(w http.ResponseWriter, status int, typeName, title, errorCode string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"type":"https://errors.company.com/iam/%s","title":%q,"status":%d,"detail":"conflict","instance":"/api/v1/internal/tenants/x/service-accounts/y/credentials","traceId":"4bf92f3577b34da6a3ce929d0e0e4736","errorCode":%q,"timestamp":"2026-10-21T10:00:00Z"}`,
		typeName, title, status, errorCode)
}

// Every TS-1 409 errorCode Token Service emits (its errors.go problem types)
// must map to its distinct domain code — not be flattened into
// ErrServiceAccountRotationInFlight. optimistic-lock-conflict is a
// record_version mismatch unrelated to a rotation race; the replay codes
// (TS-D13 revoked, TS-D22 expired) mean the caller's stored rotation_id is
// stale and must not be retried as transient.
func TestIssueOrRotateCredential_ConflictProblem_MapsErrorCodeToDomainCode(t *testing.T) {
	cases := []struct {
		typeName, title, errorCode string
		want                       domain.ErrorCode
	}{
		{"rotation-in-flight", "Rotation In Flight", "ROTATION_IN_FLIGHT", domain.ErrServiceAccountRotationInFlight},
		{"optimistic-lock-conflict", "Optimistic Lock Conflict", "OPTIMISTIC_LOCK_CONFLICT", domain.ErrOptimisticLockConflict},
		{"credential-replay-revoked", "Credential Replay Revoked", "CREDENTIAL_REPLAY_REVOKED", domain.ErrCredentialReplayRevoked},
		{"credential-replay-expired", "Credential Replay Expired", "CREDENTIAL_REPLAY_EXPIRED", domain.ErrCredentialReplayExpired},
		// Unknown code → the safe retryable default.
		{"conflict", "Conflict", "CONFLICT", domain.ErrServiceAccountRotationInFlight},
	}
	for _, tc := range cases {
		t.Run(tc.errorCode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeProblem(w, http.StatusConflict, tc.typeName, tc.title, tc.errorCode)
			}))
			defer srv.Close()

			c := New(srv.URL, noopLogger{})
			_, err := c.IssueOrRotateCredential(context.Background(), uuid.New(), uuid.New(), port.IssueCredentialRequest{RotationID: uuid.New()})
			require.Error(t, err)
			var derr *domain.Error
			require.ErrorAs(t, err, &derr)
			assert.Equal(t, tc.want, derr.Code)
			assert.Equal(t, tc.errorCode, derr.Details["upstream_error"])
		})
	}
}

// A 409 whose body is not application/problem+json is not decoded at all —
// it falls back to the safe default.
func TestIssueOrRotateCredential_ConflictNonProblemBody_DefaultsToRotationInFlight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"errorCode":"OPTIMISTIC_LOCK_CONFLICT"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.IssueOrRotateCredential(context.Background(), uuid.New(), uuid.New(), port.IssueCredentialRequest{RotationID: uuid.New()})
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.Equal(t, domain.ErrServiceAccountRotationInFlight, derr.Code)
}

// A 400/422 problem surfaces Token Service's errorCode in the error text.
func TestRegisterPrincipal_BadRequestProblemSurfacesErrorCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, http.StatusBadRequest, "invalid-request", "Invalid Request", "INVALID_REQUEST")
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.RegisterPrincipal(context.Background(), uuid.New(), port.RegisterPrincipalRequest{
		PrincipalSub: uuid.New(), KeycloakClientID: "platform-automation",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TS-4 rejected request — INVALID_REQUEST")
	var derr *domain.Error
	assert.False(t, errors.As(err, &derr), "a 400 must not be a domain.Error")
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
			"credentials":[{"version":1,"status":"rotating","openbao_path":"x/v1","issued_at":"2026-09-01T00:00:00Z","expires_at":"2026-09-10T00:00:00Z","rotation_cadence_days":30,"next_rotation_at":"2026-10-10T00:00:00Z"},
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
	require.NotNil(t, res.Credentials[0].RotationCadenceDays)
	assert.Equal(t, 30, *res.Credentials[0].RotationCadenceDays)
	require.NotNil(t, res.Credentials[0].NextRotationAt)
	assert.Equal(t, 2026, res.Credentials[0].NextRotationAt.Year())
	assert.Nil(t, res.Credentials[1].ExpiresAt)
	assert.Nil(t, res.Credentials[1].RotationCadenceDays, "omitted rotation_cadence_days decodes to nil")
	assert.Nil(t, res.Credentials[1].NextRotationAt, "omitted next_rotation_at decodes to nil")
}

func TestReadPrincipal_NextRotationAtMalformedErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"principal_id":"`+uuid.New().String()+`","tenant_id":"`+uuid.New().String()+`","keycloak_client_id":"platform-automation",
			"principal_type":"platform_automation","status":"active","record_version":1,
			"credentials":[{"version":1,"status":"active","openbao_path":"x","issued_at":"2026-09-01T00:00:00Z","next_rotation_at":"not-a-date"}]}`)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	_, err := c.ReadPrincipal(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "next_rotation_at")
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

// TestReadPrincipal_ResponseShapePinned pins the TS-3 response JSON field
// names and types that RP-18 will rely on. If Token Service renames, drops,
// or changes the type of any field, this test fails before RP-18 ships —
// catching contract drift at the unit-test layer rather than in staging.
func TestReadPrincipal_ResponseShapePinned(t *testing.T) {
	tenantID, principalID := uuid.New(), uuid.New()
	cadence := 30
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, fmt.Sprintf("/api/v1/internal/tenants/%s/service-accounts/%s", tenantID, principalID), r.URL.Path)
		assert.Equal(t, domain.SystemActorID, r.Header.Get("x-user-id"))
		assert.Equal(t, tenantID.String(), r.Header.Get("x-tenant-id"))
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{
			"principal_id": "%s",
			"tenant_id": "%s",
			"keycloak_client_id": "platform-automation",
			"principal_type": "platform_automation",
			"status": "active",
			"record_version": 3,
			"credentials": [{
				"version": 2,
				"status": "active",
				"openbao_path": "secret/data/iam/ts/tenants/%s/credentials/v2",
				"issued_at": "2026-09-01T00:00:00Z",
				"expires_at": "2027-09-01T00:00:00Z",
				"rotation_cadence_days": %d,
				"next_rotation_at": "2026-12-01T00:00:00Z"
			}]
		}`, principalID, tenantID, tenantID, cadence)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	res, err := c.ReadPrincipal(context.Background(), tenantID, principalID)
	require.NoError(t, err)

	assert.Equal(t, principalID, res.PrincipalID)
	assert.Equal(t, tenantID, res.TenantID)
	assert.Equal(t, "platform-automation", res.KeycloakClientID)
	assert.Equal(t, "platform_automation", res.PrincipalType)
	assert.Equal(t, "active", res.Status)
	assert.Equal(t, 3, res.RecordVersion)
	require.Len(t, res.Credentials, 1)
	cred := res.Credentials[0]
	assert.Equal(t, 2, cred.Version)
	assert.Equal(t, "active", cred.Status)
	assert.NotNil(t, cred.ExpiresAt)
	assert.NotNil(t, cred.RotationCadenceDays)
	assert.Equal(t, cadence, *cred.RotationCadenceDays)
	assert.NotNil(t, cred.NextRotationAt)
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

// ── RevokeCredential (TS-2 via TS-3 lookup) ──────────────────────────────────

// Happy path: one rotating credential → TS-3 read, then POST .../credentials/:version/revoke.
func TestRevokeCredential_RevokesRotatingCredentialViaVersionedEndpoint(t *testing.T) {
	tenantID, principalID := uuid.New(), uuid.New()
	var revokeMethod, revokePath string
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/v1/internal/tenants/%s/service-accounts/%s", tenantID, principalID),
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"principal_id":"%s","tenant_id":"%s","keycloak_client_id":"platform-automation",
				"principal_type":"platform_automation","status":"active","record_version":2,
				"credentials":[
					{"version":1,"status":"rotating","openbao_path":"x/v1","issued_at":"2026-09-01T00:00:00Z"},
					{"version":2,"status":"active","openbao_path":"x/v2","issued_at":"2026-10-01T00:00:00Z"}
				]}`, principalID, tenantID)
		})
	mux.HandleFunc(fmt.Sprintf("/api/v1/internal/tenants/%s/service-accounts/%s/credentials/1/revoke", tenantID, principalID),
		func(w http.ResponseWriter, r *http.Request) {
			revokeMethod, revokePath = r.Method, r.URL.Path
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"version":1,"status":"revoked","revoked_at":"2026-10-12T00:00:00Z","keycloak_invalidation":"caller_responsibility"}`)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	err := c.RevokeCredential(context.Background(), tenantID, principalID)
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, revokeMethod, "TS-2 must use POST, not DELETE")
	assert.Equal(t, fmt.Sprintf("/api/v1/internal/tenants/%s/service-accounts/%s/credentials/1/revoke", tenantID, principalID), revokePath)
}

// No rotating credential → only TS-3 is called, returns nil (idempotent).
func TestRevokeCredential_NoRotatingCredential_ReturnsNil(t *testing.T) {
	tenantID, principalID := uuid.New(), uuid.New()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"principal_id":"%s","tenant_id":"%s","keycloak_client_id":"platform-automation",
			"principal_type":"platform_automation","status":"active","record_version":1,
			"credentials":[{"version":1,"status":"active","openbao_path":"x/v1","issued_at":"2026-10-01T00:00:00Z"}]}`,
			principalID, tenantID)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	err := c.RevokeCredential(context.Background(), tenantID, principalID)
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "only TS-3 should be called when no rotating credential exists")
}

// TS-3 failure (e.g. 503) wraps the error with "TS-3 lookup" context.
func TestRevokeCredential_TS3FailurePropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	err := c.RevokeCredential(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TS-3 lookup")
}

// TS-2 server error propagates as ErrTokenServiceUnavailable.
func TestRevokeCredential_TS2ServerErrorPropagates(t *testing.T) {
	tenantID, principalID := uuid.New(), uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/v1/internal/tenants/%s/service-accounts/%s", tenantID, principalID),
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"principal_id":"%s","tenant_id":"%s","keycloak_client_id":"platform-automation",
				"principal_type":"platform_automation","status":"active","record_version":2,
				"credentials":[{"version":1,"status":"rotating","openbao_path":"x/v1","issued_at":"2026-09-01T00:00:00Z"}]}`,
				principalID, tenantID)
		})
	mux.HandleFunc(fmt.Sprintf("/api/v1/internal/tenants/%s/service-accounts/%s/credentials/1/revoke", tenantID, principalID),
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, noopLogger{})
	err := c.RevokeCredential(context.Background(), tenantID, principalID)
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.Equal(t, domain.ErrTokenServiceUnavailable, derr.Code)
}
