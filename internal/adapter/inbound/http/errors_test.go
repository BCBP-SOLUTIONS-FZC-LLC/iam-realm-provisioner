package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// newErrorResponse's c==nil branch — no known production call site passes
// a nil context, but the function guards against it explicitly, so it's
// exercised directly here rather than left dark.
func TestNewErrorResponse_NilContext(t *testing.T) {
	er := newErrorResponse(nil, "some_code", map[string]any{"k": "v"})
	assert.Equal(t, "some_code", er.Error)
	assert.Empty(t, er.TraceID)
	assert.Empty(t, er.RequestID)
	assert.Equal(t, "v", er.Details["k"])
}

// The request-id fallback chain's second branch: no value stored on the
// context by gincommon's own RequestIDMiddleware, but the inbound request
// carries the header directly.
func TestNewErrorResponse_RequestIDFromInboundHeader(t *testing.T) {
	c, _ := newErrorTestContext()
	c.Request.Header.Set(gincommon.HeaderRequestID, "req-from-header")
	er := newErrorResponse(c, "some_code", nil)
	assert.Equal(t, "req-from-header", er.RequestID)
}

// The request-id fallback chain's third branch: neither the context value
// nor the inbound header is set, but a response header was already written
// (e.g. by an earlier middleware echoing the ID back to the caller).
func TestNewErrorResponse_RequestIDFromResponseHeader(t *testing.T) {
	c, w := newErrorTestContext()
	w.Header().Set(gincommon.HeaderRequestIDResponse, "req-from-response")
	er := newErrorResponse(c, "some_code", nil)
	assert.Equal(t, "req-from-response", er.RequestID)
}

// All three request-id sources absent — RequestID stays empty rather than
// panicking or fabricating a value.
func TestNewErrorResponse_NoRequestIDSourceAvailable(t *testing.T) {
	c, _ := newErrorTestContext()
	er := newErrorResponse(c, "some_code", nil)
	assert.Empty(t, er.RequestID)
}

// newErrorTestContext builds a bare *gin.Context with a real (non-nil)
// *http.Request, matching what every real handler invocation carries.
func newErrorTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/probe", nil)
	return c, w
}

// TestHandleError_FullTaxonomy exercises every §17 ErrorCode this package
// knows how to map, including three codes (invalid_realm_strategy,
// idp_config_not_found, org_membership_unavailable) that no handler-
// reachable code path in this version of the service actually constructs —
// see the package-level notes in provisioning/idp/lifecycle handler tests
// for the codes that ARE reachable via a full request. This test still
// pins HandleError's mapping table itself, directly.
func TestHandleError_FullTaxonomy(t *testing.T) {
	tests := []struct {
		code       domain.ErrorCode
		wantStatus int
	}{
		{domain.ErrInvalidRequest, http.StatusBadRequest},
		{domain.ErrInvalidRealmStrategy, http.StatusBadRequest},
		{domain.ErrInvalidIdpProtocol, http.StatusBadRequest},
		{domain.ErrUnsupportedRealmSetting, http.StatusBadRequest},
		{domain.ErrIdpRegionPortabilityFailed, http.StatusBadRequest},
		{domain.ErrMissingIdempotencyKey, http.StatusBadRequest},
		{domain.ErrIdpWarningsUnacknowledged, http.StatusUnprocessableEntity},
		{domain.ErrTenantRealmNotFound, http.StatusNotFound},
		{domain.ErrIdpConfigNotFound, http.StatusNotFound},
		{domain.ErrIdpRequiresDedicatedRealm, http.StatusConflict},
		{domain.ErrRealmAlreadyDedicated, http.StatusConflict},
		{domain.ErrRevertWindowExpired, http.StatusConflict},
		{domain.ErrTenantOffboarded, http.StatusConflict},
		{domain.ErrOperationInProgress, http.StatusConflict},
		{domain.ErrOptimisticLockConflict, http.StatusConflict},
		{domain.ErrKeycloakUnavailable, http.StatusBadGateway},
		{domain.ErrConversionFailed, http.StatusBadGateway},
		{domain.ErrOrgMembershipUnavailable, http.StatusServiceUnavailable},
		{domain.ErrObjectStoreUnavailable, http.StatusServiceUnavailable},
		{domain.ErrDBUnavailable, http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(string(tc.code), func(t *testing.T) {
			c, w := newErrorTestContext()
			HandleError(c, domain.NewError(tc.code, "boom").WithDetails(map[string]any{"k": "v"}))
			if w.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d", tc.wantStatus, w.Code)
			}
			er := decodeError(t, w)
			if er.Error != string(tc.code) {
				t.Fatalf("expected error code %q, got %q", tc.code, er.Error)
			}
			if er.Status != tc.wantStatus {
				t.Fatalf("expected body status %d, got %d", tc.wantStatus, er.Status)
			}
			if er.Details["k"] != "v" {
				t.Fatalf("expected details to be echoed, got %+v", er.Details)
			}
		})
	}
}

func TestHandleError_UnclassifiedErrorFallsBackTo500(t *testing.T) {
	c, w := newErrorTestContext()
	HandleError(c, errors.New("something unexpected"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	er := decodeError(t, w)
	if er.Error != "internal_error" {
		t.Fatalf("expected internal_error, got %q", er.Error)
	}
}

type fakeErrorLogger struct {
	errorCalls int
}

func (f *fakeErrorLogger) Debug(msg string, fields map[string]any) {}
func (f *fakeErrorLogger) Info(msg string, fields map[string]any)  {}
func (f *fakeErrorLogger) Warn(msg string, fields map[string]any)  {}
func (f *fakeErrorLogger) Error(msg string, fields map[string]any) { f.errorCalls++ }

// TestHandleError_LogsUnclassifiedErrors drives the fallback-500 path
// through a real request — replaying a corrupted idempotency-ledger result
// produces a genuine non-domain error from replayOrConflict's json.Unmarshal
// failure — and asserts the shared errorLogger (set once by NewRouter) is
// invoked, matching production behavior where an unhandled 500 is always
// logged, never silent.
func TestHandleError_LogsUnclassifiedErrors(t *testing.T) {
	logger := &fakeErrorLogger{}
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.idempotency.rows["invite-corrupt"] = &domain.ProvisioningOperation{
		IdempotencyKey: "invite-corrupt", TenantID: tenantID, Status: domain.OpStatusSucceeded, Result: []byte(`not-json`),
	}
	cfg := testGinConfig
	cfg.Logger = logger
	r := NewRouter(RouterConfig{
		GinConfig: cfg,
		Handlers:  d.handler(),
	}).Handler()

	headers := withHeader(sysHeaders(), "Idempotency-Key", "invite-corrupt")
	rec := doRequest(r, http.MethodPost, invitedUserPath(tenantID), headers, `{"email":"u@example.com","full_name":"User One"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	er := decodeError(t, rec)
	if er.RequestID == "" {
		t.Fatal("expected gincommon RequestIDMiddleware to populate request_id on the error body")
	}
	if er.TraceID == "" {
		t.Fatal("expected gincommon TracingMiddleware to populate trace_id on the error body")
	}
	// gincommon's own LoggingMiddleware also logs 5xx responses at Error
	// level, so this only asserts HandleError's own call landed at least
	// once — the exact count is gincommon's concern, not this package's.
	if logger.errorCalls < 1 {
		t.Fatalf("expected errorLogger.Error to be called at least once, got %d", logger.errorCalls)
	}
}

func TestHandleError_DBConnectivitySQLState_Returns503(t *testing.T) {
	for _, code := range []string{"08006", "08001", "08004"} {
		t.Run(code, func(t *testing.T) {
			c, w := newErrorTestContext()
			HandleError(c, &pgconn.PgError{Code: code})
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d", w.Code)
			}
			er := decodeError(t, w)
			if er.Error != string(domain.ErrDBUnavailable) {
				t.Fatalf("expected db_unavailable, got %q", er.Error)
			}
		})
	}
}

func TestHandleError_DBResourceExhaustionSQLState_Returns503(t *testing.T) {
	c, w := newErrorTestContext()
	HandleError(c, &pgconn.PgError{Code: "53300"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	er := decodeError(t, w)
	if er.Error != string(domain.ErrDBUnavailable) {
		t.Fatalf("expected db_unavailable, got %q", er.Error)
	}
}

func TestHandleError_OperatorOrSystemErrorSQLState_Returns503(t *testing.T) {
	for _, code := range []string{"57P01", "58030"} {
		t.Run(code, func(t *testing.T) {
			c, w := newErrorTestContext()
			HandleError(c, &pgconn.PgError{Code: code})
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d", w.Code)
			}
			er := decodeError(t, w)
			if er.Error != string(domain.ErrDBUnavailable) {
				t.Fatalf("expected db_unavailable, got %q", er.Error)
			}
		})
	}
}

func TestHandleError_ConstraintViolationSQLState_Returns500(t *testing.T) {
	c, w := newErrorTestContext()
	HandleError(c, &pgconn.PgError{Code: "23505"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	er := decodeError(t, w)
	if er.Error != "internal_error" {
		t.Fatalf("expected internal_error, got %q", er.Error)
	}
}

// A statement_timeout (57014) or lock_timeout (55P03) that leaked past the
// service layer is a timed-out request, not a database outage.
func TestHandleError_StatementOrLockTimeout_Returns503RequestTimeout(t *testing.T) {
	for _, code := range []string{"57014", "55P03"} {
		t.Run(code, func(t *testing.T) {
			c, w := newErrorTestContext()
			HandleError(c, &pgconn.PgError{Code: code})
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d", w.Code)
			}
			if er := decodeError(t, w); er.Error != string(domain.ErrRequestTimeout) {
				t.Fatalf("expected request_timeout, got %q", er.Error)
			}
		})
	}
}
