package orgmembership

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestHasResidualMFARequirement_RequestShapeAndHeaders(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	var gotPath, gotQuery string
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"roles":[],"departments":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second, noopLogger{})
	_, err := c.HasResidualMFARequirement(context.Background(), tenantID, userID)
	require.NoError(t, err)

	assert.Equal(t, fmt.Sprintf("/api/v1/internal/users/%s/memberships", userID), gotPath)
	assert.Equal(t, fmt.Sprintf("tenant_id=%s", tenantID), gotQuery)
	assert.Equal(t, "00000000-0000-0000-0000-0000000000a1", gotHeaders.Get("x-user-id"))
	assert.Equal(t, tenantID.String(), gotHeaders.Get("x-tenant-id"))
	assert.Equal(t, "iam-system", gotHeaders.Get("x-tenant-roles"))
}

func TestHasResidualMFARequirement_TrueOnMFARequiringRole(t *testing.T) {
	for _, role := range []string{"tenant_owner", "tenant_admin"} {
		t.Run(role, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintf(w, `{"roles":["%s"],"departments":[]}`, role)
			}))
			defer srv.Close()

			c := New(srv.URL, 5*time.Second, noopLogger{})
			got, err := c.HasResidualMFARequirement(context.Background(), uuid.New(), uuid.New())
			require.NoError(t, err)
			assert.True(t, got)
		})
	}
}

func TestHasResidualMFARequirement_TrueOnMFARequiringDepartmentLevel(t *testing.T) {
	for _, level := range []string{"reviewer", "approver"} {
		t.Run(level, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintf(w, `{"roles":[],"departments":[{"role_level":"%s"}]}`, level)
			}))
			defer srv.Close()

			c := New(srv.URL, 5*time.Second, noopLogger{})
			got, err := c.HasResidualMFARequirement(context.Background(), uuid.New(), uuid.New())
			require.NoError(t, err)
			assert.True(t, got)
		})
	}
}

func TestHasResidualMFARequirement_FalseWhenNoMFARequiringGrant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"roles":["member"],"departments":[{"role_level":"contributor"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second, noopLogger{})
	got, err := c.HasResidualMFARequirement(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.False(t, got)
}

// TestHasResidualMFARequirement_WrongDepartmentFieldNameNeverMatches guards
// against regressing to the pre-fix field-name mismatch: O&M's I-8 response
// serializes DeptMembershipView.RoleLevel as `role_level`
// (iam-org-membership's domain/membership.go), not `level`. A payload using
// the old, wrong key must decode to an empty Level and therefore never
// report a residual MFA requirement — proving the struct tag really reads
// `role_level`, not silently tolerating either name.
func TestHasResidualMFARequirement_WrongDepartmentFieldNameNeverMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"roles":[],"departments":[{"level":"approver"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second, noopLogger{})
	got, err := c.HasResidualMFARequirement(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.False(t, got, "a department entry keyed \"level\" (not O&M's actual \"role_level\") must not be read as a residual MFA requirement")
}

func TestHasResidualMFARequirement_404MeansNoActiveMembership(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second, noopLogger{})
	got, err := c.HasResidualMFARequirement(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.False(t, got)
}

func TestHasResidualMFARequirement_5xxMapsToOrgMembershipUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second, noopLogger{})
	_, err := c.HasResidualMFARequirement(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)

	var domErr *domain.Error
	require.ErrorAs(t, err, &domErr)
	assert.Equal(t, domain.ErrOrgMembershipUnavailable, domErr.Code)
	assert.Equal(t, 503, domErr.Status())
	assert.Equal(t, http.StatusServiceUnavailable, domErr.Details["status"])
}

func TestHasResidualMFARequirement_TransportErrorMapsToOrgMembershipUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed before use: connection refused

	c := New(srv.URL, 5*time.Second, noopLogger{})
	_, err := c.HasResidualMFARequirement(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)

	var domErr *domain.Error
	require.ErrorAs(t, err, &domErr)
	assert.Equal(t, domain.ErrOrgMembershipUnavailable, domErr.Code)
}

func TestHasResidualMFARequirement_Unexpected3xxStatusReturnsPlainError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second, noopLogger{})
	_, err := c.HasResidualMFARequirement(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	var domErr *domain.Error
	assert.NotErrorAs(t, err, &domErr, "a plain unexpected-status error must not be mistaken for the org_membership_unavailable domain error")
	assert.Contains(t, err.Error(), "unexpected status 302")
}

func TestHasResidualMFARequirement_MalformedJSONReturnsDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second, noopLogger{})
	_, err := c.HasResidualMFARequirement(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode I-8 response")
}

func TestHasResidualMFARequirement_MalformedBaseURLFailsRequestConstruction(t *testing.T) {
	c := New("http://\x7f-invalid-host", 5*time.Second, noopLogger{})
	_, err := c.HasResidualMFARequirement(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
}

// erroringBody wraps a real body's Read but fails Close, so closeBody's
// log-only error path can be exercised.
type erroringBody struct {
	io.Reader
}

func (erroringBody) Close() error { return errors.New("simulated close failure") }

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHasResidualMFARequirement_BodyCloseFailureIsLoggedNotPropagated(t *testing.T) {
	var warned map[string]any
	logger := &capturingLogger{warn: func(m map[string]any) { warned = m }}

	c := &Client{
		baseURL: "http://unused.invalid",
		log:     logger,
		http: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       erroringBody{strings.NewReader(`{"roles":[],"departments":[]}`)},
			}, nil
		})},
	}

	got, err := c.HasResidualMFARequirement(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.False(t, got)
	require.NotNil(t, warned, "a Close failure must be logged via Warn")
	assert.Contains(t, warned["error"], "simulated close failure")
}

type capturingLogger struct {
	warn func(map[string]any)
}

func (capturingLogger) Debug(string, map[string]any) {}
func (capturingLogger) Info(string, map[string]any)  {}
func (c *capturingLogger) Warn(_ string, fields map[string]any) {
	if c.warn != nil {
		c.warn(fields)
	}
}
func (capturingLogger) Error(string, map[string]any) {}

func TestNew_SetsBaseURLAndLogger(t *testing.T) {
	c := New("http://om.internal", 5*time.Second, noopLogger{})
	assert.Equal(t, "http://om.internal", c.baseURL)
	require.NotNil(t, c.http)
}

// ─────────────────────────────────────────────────────────────────────────
// ListLapsedSubscriptions (I-16, RP-C3's suspend-phase trigger) — same
// shape/branch complexity as HasResidualMFARequirement above, mirrored
// test-for-test, adapted for the two real differences: this call is
// deliberately cross-tenant (no :id path segment, and O&M's handler never
// reads x-tenant-id — it queries its own BYPASSRLS sysPool unconditionally)
// and has no 404-means-something-benign branch — any 3xx/4xx here is
// simply an unexpected status.
//
// A sentinel uuid.Nil x-tenant-id IS still sent (corrected — a prior version
// of this client sent no header at all, which O&M's shared gincommon
// RequireAuth middleware hard-rejects with 401 on every /api/v1 route,
// unconditionally, with no per-route opt-out; every real poll 401'd until
// this was caught and fixed). The header carries no real tenant scope —
// it exists purely to satisfy that platform-wide auth-header contract.
// ─────────────────────────────────────────────────────────────────────────

func TestListLapsedSubscriptions_RequestShapeAndHeaders(t *testing.T) {
	var gotPath string
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tenants":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second, noopLogger{})
	_, err := c.ListLapsedSubscriptions(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "/api/v1/internal/subscription-lapses", gotPath)
	assert.Equal(t, "00000000-0000-0000-0000-0000000000a1", gotHeaders.Get("x-user-id"))
	assert.Equal(t, "iam-system", gotHeaders.Get("x-tenant-roles"))
	assert.Equal(t, "00000000-0000-0000-0000-000000000000", gotHeaders.Get("x-tenant-id"),
		"a sentinel nil UUID satisfies O&M's RequireAuth header-presence check — this call carries no real tenant scope")
}

func TestListLapsedSubscriptions_DecodesMultipleTenants(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()
	cancelledAt := "2026-08-01T00:00:00Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"tenants":[
			{"tenant_id":%q,"realm_id":"kc-a","realm_type":"dedicated","cancelled_at":%q},
			{"tenant_id":%q,"realm_id":"kc-b","realm_type":"shared","cancelled_at":%q}
		]}`, tenantA, cancelledAt, tenantB, cancelledAt)
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second, noopLogger{})
	got, err := c.ListLapsedSubscriptions(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, tenantA, got[0].TenantID)
	assert.Equal(t, "kc-a", got[0].RealmID)
	assert.Equal(t, "dedicated", got[0].RealmType)
	assert.Equal(t, tenantB, got[1].TenantID)
	assert.Equal(t, "shared", got[1].RealmType)
}

func TestListLapsedSubscriptions_EmptyListWhenNoTenantsLapsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tenants":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second, noopLogger{})
	got, err := c.ListLapsedSubscriptions(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListLapsedSubscriptions_5xxMapsToOrgMembershipUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second, noopLogger{})
	_, err := c.ListLapsedSubscriptions(context.Background())
	require.Error(t, err)

	var domErr *domain.Error
	require.ErrorAs(t, err, &domErr)
	assert.Equal(t, domain.ErrOrgMembershipUnavailable, domErr.Code)
	assert.Equal(t, 503, domErr.Status())
	assert.Equal(t, http.StatusServiceUnavailable, domErr.Details["status"])
}

func TestListLapsedSubscriptions_TransportErrorMapsToOrgMembershipUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed before use: connection refused

	c := New(srv.URL, 5*time.Second, noopLogger{})
	_, err := c.ListLapsedSubscriptions(context.Background())
	require.Error(t, err)

	var domErr *domain.Error
	require.ErrorAs(t, err, &domErr)
	assert.Equal(t, domain.ErrOrgMembershipUnavailable, domErr.Code)
}

func TestListLapsedSubscriptions_UnexpectedStatusReturnsPlainError(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusFound} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			c := New(srv.URL, 5*time.Second, noopLogger{})
			_, err := c.ListLapsedSubscriptions(context.Background())
			require.Error(t, err)
			var domErr *domain.Error
			assert.NotErrorAs(t, err, &domErr, "an unexpected-status error must not be mistaken for the org_membership_unavailable domain error")
			assert.Contains(t, err.Error(), fmt.Sprintf("unexpected status %d", status))
		})
	}
}

func TestListLapsedSubscriptions_MalformedJSONReturnsDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second, noopLogger{})
	_, err := c.ListLapsedSubscriptions(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode I-16 response")
}

func TestListLapsedSubscriptions_MalformedBaseURLFailsRequestConstruction(t *testing.T) {
	c := New("http://\x7f-invalid-host", 5*time.Second, noopLogger{})
	_, err := c.ListLapsedSubscriptions(context.Background())
	require.Error(t, err)
}

func TestListLapsedSubscriptions_BodyCloseFailureIsLoggedNotPropagated(t *testing.T) {
	var warned map[string]any
	logger := &capturingLogger{warn: func(m map[string]any) { warned = m }}

	c := &Client{
		baseURL: "http://unused.invalid",
		log:     logger,
		http: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       erroringBody{strings.NewReader(`{"tenants":[]}`)},
			}, nil
		})},
	}

	got, err := c.ListLapsedSubscriptions(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got)
	require.NotNil(t, warned, "a Close failure must be logged via Warn")
	assert.Contains(t, warned["error"], "simulated close failure")
}
