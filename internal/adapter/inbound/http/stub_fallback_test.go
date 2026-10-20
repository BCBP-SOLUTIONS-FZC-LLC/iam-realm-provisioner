package http

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestNilServiceFallsBackToStub501 exercises every one of the 17 RP-*
// routes with every core service left nil — InternalHandler's documented
// incremental-wiring behavior (handlers.go, internal_handler.go's doc
// comment): a nil service falls back to StubHandlers' uniform 501
// not_implemented rather than panicking, so the router can be brought up
// before every phase has landed. This also exercises StubHandlers' 17
// methods directly, since NewStubHandlers() only ever renders 501s.
func TestNilServiceFallsBackToStub501(t *testing.T) {
	stub := NewInternalHandler(nil, nil, nil, nil, nil)
	r := newRouterFor(stub)

	tenantID, userID := uuid.New(), uuid.New()

	tests := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		body    string
	}{
		{"RP-1 provision-trial", http.MethodPost, provisionTrialPath, withHeader(sysHeaders(), "Idempotency-Key", "k1"), ""},
		{"RP-2 provision-direct", http.MethodPost, provisionDirectPath, withHeader(sysHeaders(), "Idempotency-Key", "k2"), ""},
		{"RP-3 convert", http.MethodPost, convertPath(tenantID), withHeader(sysHeaders(), "Idempotency-Key", "k3"), ""},
		{"RP-4 revert-conversion", http.MethodPost, revertPath(tenantID), withHeader(sysHeaders(), "Idempotency-Key", "k4"), ""},
		{"RP-5 create invited user", http.MethodPost, invitedUserPath(tenantID), withHeader(sysHeaders(), "Idempotency-Key", "k5"), ""},
		{"RP-6 configure idp", http.MethodPut, idpPath(tenantID), withHeader(sysHeaders(), "Idempotency-Key", "k6"), ""},
		{"RP-7 remove idp", http.MethodDelete, idpAliasPath(tenantID, "alias"), sysHeaders(), ""},
		{"RP-8 apply realm config", http.MethodPatch, realmConfigPath(tenantID), sysHeaders(), ""},
		{"RP-9 reset mfa", http.MethodPost, mfaResetPath(tenantID, userID), sysHeaders(), ""},
		{"RP-10 reactivate", http.MethodPost, reactivatePath(tenantID), withHeader(sysHeaders(), "Idempotency-Key", "k10"), ""},
		{"RP-11 get realm", http.MethodGet, realmGetPath(tenantID), sysHeaders(), ""},
		{"RP-12 expire trial", http.MethodPost, expireTrialPath(tenantID), withHeader(sysHeaders(), "Idempotency-Key", "k12"), ""},
		{"RP-13 portability audit", http.MethodPost, auditPath, operatorHeaders(), ""},
		{"RP-14 suspend", http.MethodPost, suspendPath(tenantID), withHeader(operatorHeaders(), "Idempotency-Key", "k14"), ""},
		{"RP-15 delete user", http.MethodDelete, deleteUserPath(tenantID, userID), sysHeaders(), ""},
		{"RP-16 revoke sessions", http.MethodPost, revokeSessionsPath(tenantID, userID), sysHeaders(), ""},
		{"RP-17 refresh service-account keys", http.MethodPost, refreshKeysPath(tenantID), sysHeaders(), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(r, tc.method, tc.path, tc.headers, tc.body)
			assertStatusAndCode(t, rec, http.StatusNotImplemented, "NOT_IMPLEMENTED")
		})
	}
}

func newRouterFor(h Handlers) http.Handler {
	return NewRouter(RouterConfig{GinConfig: testGinConfig, Handlers: h}).Handler()
}
