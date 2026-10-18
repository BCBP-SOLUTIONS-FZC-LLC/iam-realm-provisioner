package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

func mfaResetPath(tenantID, keycloakUserID uuid.UUID) string {
	return "/api/v1/internal/tenants/" + tenantID.String() + "/users/" + keycloakUserID.String() + "/mfa-reset"
}

// ── RP-9: MFA reset (fail-closed, idempotent) ────────────────────────────

func TestResetMFA_Happy_RequiresMFA(t *testing.T) {
	d := newServiceDeps()
	tenantID, kcUserID := uuid.New(), uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	var gotRequiresMFA bool
	d.keycloak.HasRequiresMFAFn = func(ctx context.Context, realmName string, userID uuid.UUID) (bool, error) { return true, nil }
	d.keycloak.ResetMFAFn = func(ctx context.Context, realmName string, userID uuid.UUID, requiresMFA bool) error {
		gotRequiresMFA = requiresMFA
		return nil
	}
	rec := doRequest(d.engine(), http.MethodPost, mfaResetPath(tenantID, kcUserID), sysHeaders(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if !gotRequiresMFA {
		t.Fatal("expected ResetMFA to be called with requiresMFA=true")
	}
}

func TestResetMFA_Happy_NoLongerRequiresMFA(t *testing.T) {
	d := newServiceDeps()
	tenantID, kcUserID := uuid.New(), uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	var gotRequiresMFA bool
	called := false
	d.keycloak.HasRequiresMFAFn = func(ctx context.Context, realmName string, userID uuid.UUID) (bool, error) { return false, nil }
	d.keycloak.ResetMFAFn = func(ctx context.Context, realmName string, userID uuid.UUID, requiresMFA bool) error {
		called = true
		gotRequiresMFA = requiresMFA
		return nil
	}
	rec := doRequest(d.engine(), http.MethodPost, mfaResetPath(tenantID, kcUserID), sysHeaders(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if !called || gotRequiresMFA {
		t.Fatal("expected ResetMFA to be called with requiresMFA=false")
	}
}

func TestResetMFA_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, mfaResetPath(uuid.New(), uuid.New()), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusNotFound, "TENANT_REALM_NOT_FOUND")
}

func TestResetMFA_InvalidKeycloakUserID(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/users/not-a-uuid/mfa-reset", sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}

func TestResetMFA_InvalidTenantID(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/users/"+uuid.New().String()+"/mfa-reset", sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}

func TestResetMFA_HasRequiresMFAFails_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	tenantID, kcUserID := uuid.New(), uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.HasRequiresMFAFn = func(ctx context.Context, realmName string, userID uuid.UUID) (bool, error) {
		return false, domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	rec := doRequest(d.engine(), http.MethodPost, mfaResetPath(tenantID, kcUserID), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadGateway, "KEYCLOAK_UNAVAILABLE")
}

func TestResetMFA_ResetFails_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	tenantID, kcUserID := uuid.New(), uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.HasRequiresMFAFn = func(ctx context.Context, realmName string, userID uuid.UUID) (bool, error) { return true, nil }
	d.keycloak.ResetMFAFn = func(ctx context.Context, realmName string, userID uuid.UUID, requiresMFA bool) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	rec := doRequest(d.engine(), http.MethodPost, mfaResetPath(tenantID, kcUserID), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadGateway, "KEYCLOAK_UNAVAILABLE")
}
