package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

func applySecretPath(tenantID uuid.UUID) string {
	return "/api/v1/internal/tenants/" + tenantID.String() + "/service-account/credentials/apply"
}

// ── RP-17: standalone rotate/break-glass-apply ────────────────────────────

func TestApplyServiceAccountSecret_Happy(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	clientID := "platform-automation"
	realm := dedicatedRealm(tenantID, "kc-shard-1")
	realm.ServiceAccountKeycloakClientID = &clientID
	d.putRealm(realm)

	var gotSecret string
	d.keycloak.SetClientSecretFn = func(ctx context.Context, realmName, clientID, secret string) error {
		gotSecret = secret
		return nil
	}

	rec := doRequestJSON(t, d.engine(), http.MethodPost, applySecretPath(tenantID), sysHeaders(), map[string]any{"secret": "rotated-secret"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotSecret != "rotated-secret" {
		t.Fatalf("expected SetClientSecret to receive the request's secret, got %q", gotSecret)
	}
}

func TestApplyServiceAccountSecret_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	rec := doRequestJSON(t, d.engine(), http.MethodPost, applySecretPath(uuid.New()), sysHeaders(), map[string]any{"secret": "s"})
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
}

func TestApplyServiceAccountSecret_NeverMinted(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))

	rec := doRequestJSON(t, d.engine(), http.MethodPost, applySecretPath(tenantID), sysHeaders(), map[string]any{"secret": "s"})
	assertStatusAndCode(t, rec, http.StatusUnprocessableEntity, "service_account_not_provisioned")
}

func TestApplyServiceAccountSecret_MissingSecret_InvalidRequest(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))

	rec := doRequestJSON(t, d.engine(), http.MethodPost, applySecretPath(tenantID), sysHeaders(), map[string]any{})
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestApplyServiceAccountSecret_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	clientID := "platform-automation"
	realm := dedicatedRealm(tenantID, "kc-shard-1")
	realm.ServiceAccountKeycloakClientID = &clientID
	d.putRealm(realm)
	d.keycloak.SetClientSecretFn = func(ctx context.Context, realmName, clientID, secret string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "keycloak down")
	}

	rec := doRequestJSON(t, d.engine(), http.MethodPost, applySecretPath(tenantID), sysHeaders(), map[string]any{"secret": "s"})
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
}
