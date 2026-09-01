package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
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
	tx              *fakeTxRunner
	idpConfigs      *fakeIdpConfigRepository
	idpValidations  *fakeIdpValidationRepository
	openbao         *fakeOpenBaoClient
	reconciliations *fakeReconciliationRepository
	orgMembership   *fakeOrgMembershipClient
}

func newServiceDeps() *serviceDeps {
	return &serviceDeps{
		realms:          &fakeTenantRealmRepository{rows: map[uuid.UUID]*domain.TenantRealm{}},
		idempotency:     &fakeIdempotencyStore{rows: map[string]*domain.ProvisioningOperation{}},
		keycloak:        &fakeKeycloakAdmin{},
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
		service.NewProvisioningService(d.realms, d.idempotency, d.keycloak, d.tx, "trial", []string{"kc-shard-1"}),
		service.NewLifecycleService(d.realms, d.idempotency, d.keycloak, d.tx, "trial"),
		service.NewMfaService(d.realms, d.keycloak, d.orgMembership),
		service.NewIdpService(d.realms, d.idpConfigs, d.idpValidations, d.openbao, d.keycloak),
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

// sysHeaders is a valid iam-system-principal caller — the identity every
// internal route (RequireSystemRole) requires.
func sysHeaders() map[string]string {
	return map[string]string{
		"x-user-id":      systemPrincipalUserID,
		"x-tenant-id":    uuid.New().String(),
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

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var er ErrorResponse
	decodeInto(t, rec, &er)
	return er
}

func assertStatusAndCode(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("expected status %d, got %d: %s", wantStatus, rec.Code, rec.Body.String())
	}
	if wantCode == "" {
		return
	}
	er := decodeError(t, rec)
	if er.Error != wantCode {
		t.Fatalf("expected error code %q, got %q (body=%s)", wantCode, er.Error, rec.Body.String())
	}
}
