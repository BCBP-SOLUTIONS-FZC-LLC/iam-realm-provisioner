package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

// serviceDeps bundles every fake port dependency the five core services
// InternalHandler delegates to can be wired from. Each handler test builds
// one of these, overrides only the fields its scenario cares about, then
// calls engine() to get a real *gin.Engine built through the production
// NewRouter/registerInternalRoutes wiring.
type serviceDeps struct {
	realms          *fakeTenantRealmRepository
	idempotency     *fakeIdempotencyStore
	keycloak        *fakeKeycloakAdmin
	tokenService    *fakeTokenServiceClient
	tx              *fakeTxRunner
	idpConfigs      *fakeIdpConfigRepository
	idpValidations  *fakeIdpValidationRepository
	openbao         *fakeOpenBaoClient
	reconciliations *fakeReconciliationRepository
	orgMembership   *fakeOrgMembershipClient
}

func newServiceDeps() *serviceDeps {
	keycloak := &fakeKeycloakAdmin{
		CreateServiceAccountClientFn: func(ctx context.Context, realmName, clientID, jwksURL string) (uuid.UUID, bool, error) {
			return uuid.New(), true, nil
		},
		ClearServiceAccountKeysCacheFn:       func(ctx context.Context, realmName string) error { return nil },
		DeleteServiceAccountClientIfExistsFn: func(ctx context.Context, realmName, clientID string) error { return nil },
	}
	tokenService := &fakeTokenServiceClient{
		RegisterPrincipalFn: func(ctx context.Context, tenantID uuid.UUID, req port.RegisterPrincipalRequest) (*port.RegisterPrincipalResult, error) {
			return &port.RegisterPrincipalResult{PrincipalID: uuid.New(), Created: true, RecordVersion: 1}, nil
		},
		IssueOrRotateCredentialFn: func(ctx context.Context, tenantID, principalID uuid.UUID, req port.IssueCredentialRequest) (*port.IssueCredentialResult, error) {
			return &port.IssueCredentialResult{Version: 1, Secret: "test-secret", OpenBaoPath: "iam/serviceaccount/test/v1", RecordVersion: 1}, nil
		},
		JWKSURLForFn: func(tenantID uuid.UUID) string {
			return "http://token-service.test/api/v1/internal/tenants/" + tenantID.String() + "/service-accounts/platform-automation/jwks.json"
		},
	}
	return &serviceDeps{
		realms:          &fakeTenantRealmRepository{rows: map[uuid.UUID]*domain.TenantRealm{}},
		idempotency:     &fakeIdempotencyStore{rows: map[string]*domain.ProvisioningOperation{}},
		keycloak:        keycloak,
		tokenService:    tokenService,
		tx:              &fakeTxRunner{pub: &fakeEventPublisher{}},
		idpConfigs:      &fakeIdpConfigRepository{rows: map[idpConfigKey]*domain.IdpConfig{}},
		idpValidations:  &fakeIdpValidationRepository{},
		openbao:         &fakeOpenBaoClient{secrets: map[idpConfigKey]map[string]any{}},
		reconciliations: &fakeReconciliationRepository{},
		orgMembership:   &fakeOrgMembershipClient{},
	}
}

// handler wires every fake into the real core services and the real
// InternalHandler — the same construction cmd/server/wiring.go performs,
// just with fakes standing in for adapters.
func (d *serviceDeps) handler() Handlers {
	return NewInternalHandler(
		service.NewProvisioningService(d.realms, d.idempotency, d.keycloak, d.tokenService, d.tx, "trial", []string{"kc-shard-1"}),
		service.NewLifecycleService(d.realms, d.idempotency, d.keycloak, d.tx, "trial").WithIdpConfigs(d.idpConfigs),
		service.NewMfaService(d.realms, d.keycloak, d.orgMembership),
		service.NewIdpService(d.realms, d.idpConfigs, d.idpValidations, d.idempotency, d.openbao, d.keycloak, d.tx, &fakePendingAuditEntryRepository{}),
		service.NewRealmConfigService(d.realms, d.reconciliations, d.keycloak),
	)
}

// engine builds the real production router (router.go's NewRouter) around
// d's fakes.
func (d *serviceDeps) engine() http.Handler {
	return NewRouter(RouterConfig{GinConfig: testGinConfig, Handlers: d.handler()}).Handler()
}

func (d *serviceDeps) putRealm(r *domain.TenantRealm) uuid.UUID {
	d.realms.rows[r.TenantID] = r
	return r.TenantID
}

// targetTenant is the x-tenant-id placeholder sysHeaders sends: doRequest
// replaces it with the request's own target tenant (the /tenants/:id path
// segment, else the body's tenant_id), exactly as real callers do (RLS-5,
// RequireTenantMatch). Set an explicit UUID to test a mismatch.
const targetTenant = "<target-tenant>"

// sysHeaders is a valid iam-system-principal caller — the identity every
// internal route (RequireSystemRole) requires.
func sysHeaders() map[string]string {
	return map[string]string{
		"x-user-id":      domain.SystemActorID,
		"x-tenant-id":    targetTenant,
		"x-tenant-roles": "iam-system",
		"Content-Type":   "application/json",
	}
}

// operatorHeaders additionally carries platform_operator, required by
// RP-13/RP-14 on top of RequireSystemRole.
func operatorHeaders() map[string]string {
	h := sysHeaders()
	h["x-tenant-roles"] = "iam-system,platform_operator"
	return h
}

// commercialHeaders simulates a commercial-system caller (Billing service,
// CD-19 allow-list). Admitted ONLY on RP-3, RP-4, and RP-11; all other
// /internal/* routes return 403 insufficient_role.
func commercialHeaders() map[string]string {
	return map[string]string{
		"x-user-id":      domain.CommercialActorID,
		"x-tenant-id":    targetTenant,
		"x-tenant-roles": "commercial-system",
		"Content-Type":   "application/json",
	}
}

func withHeader(h map[string]string, key, value string) map[string]string {
	h[key] = value
	return h
}

// timeNowMinus returns a UTC time hoursAgo hours in the past — used to
// fabricate TenantRealm.ConvertedAt fixtures relative to the revert window.
func timeNowMinus(hoursAgo float64) time.Time {
	return time.Now().UTC().Add(-time.Duration(hoursAgo * float64(time.Hour)))
}

// doRequestJSON marshals body and issues the request via doRequest
// (middleware_test.go) — a convenience for handlers whose request shape is
// easiest to build as a map[string]any (e.g. IdP config's nested map).
func doRequestJSON(t *testing.T, r http.Handler, method, path string, headers map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	return doRequest(r, method, path, headers, string(raw))
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response body: %v, body=%s", err, rec.Body.String())
	}
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) ProblemResponse {
	t.Helper()
	var p ProblemResponse
	decodeInto(t, rec, &p)
	return p
}

func assertStatusAndCode(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("expected status %d, got %d: %s", wantStatus, rec.Code, rec.Body.String())
	}
	if wantCode == "" {
		return
	}
	p := decodeError(t, rec)
	if p.ErrorCode != wantCode {
		t.Fatalf("expected errorCode %q, got %q (body=%s)", wantCode, p.ErrorCode, rec.Body.String())
	}
}
