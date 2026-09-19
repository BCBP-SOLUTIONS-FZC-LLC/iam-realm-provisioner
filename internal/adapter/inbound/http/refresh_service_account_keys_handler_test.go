package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

func refreshKeysPath(tenantID uuid.UUID) string {
	return "/api/v1/internal/tenants/" + tenantID.String() + "/service-account/keys/refresh"
}

// ── RP-17: standalone rotate/revoke/break-glass key-cache refresh ────────

func TestRefreshServiceAccountKeys_Happy(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	clientID := "platform-automation"
	realm := dedicatedRealm(tenantID, "kc-shard-1")
	realm.ServiceAccountKeycloakClientID = &clientID
	d.putRealm(realm)

	var gotRealm string
	d.keycloak.ClearServiceAccountKeysCacheFn = func(ctx context.Context, realmName string) error {
		gotRealm = realmName
		return nil
	}

	rec := doRequestJSON(t, d.engine(), http.MethodPost, refreshKeysPath(tenantID), sysHeaders(), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotRealm != realm.RealmName {
		t.Fatalf("expected ClearServiceAccountKeysCache to receive the tenant's realm, got %q", gotRealm)
	}
}

func TestRefreshServiceAccountKeys_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	rec := doRequestJSON(t, d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/service-account/keys/refresh", sysHeaders(), nil)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestRefreshServiceAccountKeys_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	rec := doRequestJSON(t, d.engine(), http.MethodPost, refreshKeysPath(uuid.New()), sysHeaders(), nil)
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
}

func TestRefreshServiceAccountKeys_NeverMinted(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))

	rec := doRequestJSON(t, d.engine(), http.MethodPost, refreshKeysPath(tenantID), sysHeaders(), nil)
	assertStatusAndCode(t, rec, http.StatusUnprocessableEntity, "service_account_not_provisioned")
}

func TestRefreshServiceAccountKeys_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	clientID := "platform-automation"
	realm := dedicatedRealm(tenantID, "kc-shard-1")
	realm.ServiceAccountKeycloakClientID = &clientID
	d.putRealm(realm)
	d.keycloak.ClearServiceAccountKeysCacheFn = func(ctx context.Context, realmName string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "keycloak down")
	}

	rec := doRequestJSON(t, d.engine(), http.MethodPost, refreshKeysPath(tenantID), sysHeaders(), nil)
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
}
