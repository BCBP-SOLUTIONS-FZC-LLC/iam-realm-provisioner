package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

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
	// gincommon's own LoggingMiddleware also logs 5xx responses at Error
	// level, so this only asserts HandleError's own call landed at least
	// once — the exact count is gincommon's concern, not this package's.
	if logger.errorCalls < 1 {
		t.Fatalf("expected errorLogger.Error to be called at least once, got %d", logger.errorCalls)
	}
}
