package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

func idpPath(id uuid.UUID) string { return "/api/v1/internal/tenants/" + id.String() + "/idp" }
func idpAliasPath(id uuid.UUID, alias string) string {
	return "/api/v1/internal/tenants/" + id.String() + "/idp/" + alias
}
func realmConfigPath(id uuid.UUID) string {
	return "/api/v1/internal/tenants/" + id.String() + "/realm-config"
}

const auditPath = "/api/v1/internal/audit/idp-region-portability"

// passingOIDCConfig is region-free with a strong PKCE method — the §11.6
// checklist's pass case for OIDC.
func passingOIDCConfig() map[string]any {
	return map[string]any{
		"issuer":       "https://auth.bcbpsolutions.com/realms/acme",
		"redirect_uri": "https://auth.bcbpsolutions.com/callback",
		"audience":     "acme-clients",
		"pkce_method":  "S256",
		"client_id":    "acme-client",
	}
}

func configureIdpBody(protocol string, cfg map[string]any, acks []string) map[string]any {
	return map[string]any{
		"idp_alias": "corp-sso", "protocol": protocol, "display_name": "Corp SSO",
		"config": cfg, "secret": "", "ack": acks,
	}
}

// ── RP-6: configure idp ───────────────────────────────────────────────────

func TestConfigureIdp_HappyPass(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error { return nil }

	headers := withHeader(sysHeaders(), "Idempotency-Key", "idp-1")
	rec := doRequestJSON(t, d.engine(), http.MethodPut, idpPath(tenantID), headers, configureIdpBody("oidc", passingOIDCConfig(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		IdpAlias string `json:"idp_alias"`
		Status   string `json:"status"`
	}
	decodeInto(t, rec, &body)
	if body.IdpAlias != "corp-sso" || body.Status != "active" {
		t.Fatalf("unexpected result: %+v", body)
	}
}

func TestConfigureIdp_WithSecretWritesOpenBao(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error { return nil }

	body := configureIdpBody("oidc", passingOIDCConfig(), nil)
	body["secret"] = "client-secret-value"
	headers := withHeader(sysHeaders(), "Idempotency-Key", "idp-1b")
	rec := doRequestJSON(t, d.engine(), http.MethodPut, idpPath(tenantID), headers, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := d.openbao.secrets[idpConfigKey{tenantID, "corp-sso"}]; !ok {
		t.Fatal("expected secret to have been written to OpenBao")
	}
}

func TestConfigureIdp_OpenBaoWriteFails(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error {
		return domain.NewError(domain.ErrObjectStoreUnavailable, "openbao down")
	}
	body := configureIdpBody("oidc", passingOIDCConfig(), nil)
	body["secret"] = "client-secret-value"
	headers := withHeader(sysHeaders(), "Idempotency-Key", "idp-1c")
	rec := doRequestJSON(t, d.engine(), http.MethodPut, idpPath(tenantID), headers, body)
	assertStatusAndCode(t, rec, http.StatusServiceUnavailable, "object_store_unavailable")
}

func TestConfigureIdp_InvalidProtocol(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	headers := withHeader(sysHeaders(), "Idempotency-Key", "idp-2")
	rec := doRequestJSON(t, d.engine(), http.MethodPut, idpPath(tenantID), headers, configureIdpBody("ldap", passingOIDCConfig(), nil))
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_idp_protocol")
}

func TestConfigureIdp_RequiresDedicatedRealm(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))
	headers := withHeader(sysHeaders(), "Idempotency-Key", "idp-3")
	rec := doRequestJSON(t, d.engine(), http.MethodPut, idpPath(tenantID), headers, configureIdpBody("oidc", passingOIDCConfig(), nil))
	assertStatusAndCode(t, rec, http.StatusConflict, "idp_requires_dedicated_realm")
}

func TestConfigureIdp_RegionPortabilityFailed(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	cfg := map[string]any{
		"issuer": "https://us-east-1.auth.bcbpsolutions.com/realms/acme", "redirect_uri": "https://auth.bcbpsolutions.com/callback",
		"audience": "acme", "pkce_method": "S256",
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "idp-4")
	rec := doRequestJSON(t, d.engine(), http.MethodPut, idpPath(tenantID), headers, configureIdpBody("oidc", cfg, nil))
	assertStatusAndCode(t, rec, http.StatusBadRequest, "idp_region_portability_failed")
}

func TestConfigureIdp_WarningsUnacknowledged(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	cfg := map[string]any{
		"issuer": "https://auth.bcbpsolutions.com/realms/acme", "redirect_uri": "https://auth.bcbpsolutions.com/callback",
		"audience": "acme", "client_id": "acme-client", // pkce_method omitted -> WARN, unacknowledged
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "idp-5")
	rec := doRequestJSON(t, d.engine(), http.MethodPut, idpPath(tenantID), headers, configureIdpBody("oidc", cfg, nil))
	assertStatusAndCode(t, rec, http.StatusUnprocessableEntity, "idp_warnings_unacknowledged")
}

func TestConfigureIdp_WarningsAcknowledgedPasses(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error { return nil }
	cfg := map[string]any{
		"issuer": "https://auth.bcbpsolutions.com/realms/acme", "redirect_uri": "https://auth.bcbpsolutions.com/callback",
		"audience": "acme", "client_id": "acme-client",
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "idp-6")
	rec := doRequestJSON(t, d.engine(), http.MethodPut, idpPath(tenantID), headers, configureIdpBody("oidc", cfg, []string{domain.CheckOIDCPKCE}))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 once the WARN check is acknowledged, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestConfigureIdp_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "idp-7")
	rec := doRequestJSON(t, d.engine(), http.MethodPut, idpPath(uuid.New()), headers, configureIdpBody("oidc", passingOIDCConfig(), nil))
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
}

func TestConfigureIdp_InvalidRequest(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	headers := withHeader(sysHeaders(), "Idempotency-Key", "idp-8")
	rec := doRequest(d.engine(), http.MethodPut, idpPath(tenantID), headers, `{"protocol":"oidc"}`) // missing idp_alias/display_name
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

// CRITICAL (security): a path-traversal idp_alias must be rejected end to
// end through the real router before any OpenBao/Keycloak call — idp_alias
// flows into openbao.Client.idpPath's plain string interpolation with no
// sanitization at that layer.
func TestConfigureIdp_PathTraversalAlias_Rejected(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error {
		t.Fatal("must not write to OpenBao for a path-traversal idp_alias")
		return nil
	}
	d.keycloak.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
		t.Fatal("must not call Keycloak for a path-traversal idp_alias")
		return nil
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "idp-traversal")
	body := `{"idp_alias":"../other-tenant/evil","protocol":"saml","display_name":"Evil"}`
	rec := doRequest(d.engine(), http.MethodPut, idpPath(tenantID), headers, body)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestRemoveIdp_PathTraversalAlias_Rejected(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.openbao.DeleteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string) error {
		t.Fatal("must not call OpenBao for a path-traversal idp_alias")
		return nil
	}
	d.keycloak.DeleteIdpFn = func(ctx context.Context, realmName, alias string) error {
		t.Fatal("must not call Keycloak for a path-traversal idp_alias")
		return nil
	}
	rec := doRequest(d.engine(), http.MethodDelete, idpAliasPath(tenantID, "..evil"), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestConfigureIdp_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "idp-9")
	rec := doRequestJSON(t, d.engine(), http.MethodPut, idpPath(tenantID), headers, configureIdpBody("oidc", passingOIDCConfig(), nil))
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
}

func TestConfigureIdp_MissingIdempotencyKey(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	rec := doRequestJSON(t, d.engine(), http.MethodPut, idpPath(tenantID), sysHeaders(), configureIdpBody("oidc", passingOIDCConfig(), nil))
	assertStatusAndCode(t, rec, http.StatusBadRequest, "missing_idempotency_key")
}

func TestConfigureIdp_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "idp-10")
	rec := doRequestJSON(t, d.engine(), http.MethodPut, "/api/v1/internal/tenants/not-a-uuid/idp", headers, configureIdpBody("oidc", passingOIDCConfig(), nil))
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

// ── RP-7: remove idp ──────────────────────────────────────────────────────

func TestRemoveIdp_Happy(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.idpConfigs.rows[idpConfigKey{tenantID, "corp-sso"}] = &domain.IdpConfig{TenantID: tenantID, IdpAlias: "corp-sso", Protocol: domain.IdpProtocolOIDC, Config: passingOIDCConfig()}
	d.keycloak.DeleteIdpFn = func(ctx context.Context, realmName, alias string) error { return nil }
	rec := doRequest(d.engine(), http.MethodDelete, idpAliasPath(tenantID, "corp-sso"), sysHeaders(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRemoveIdp_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodDelete, idpAliasPath(uuid.New(), "corp-sso"), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
}

func TestRemoveIdp_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.idpConfigs.rows[idpConfigKey{tenantID, "corp-sso"}] = &domain.IdpConfig{TenantID: tenantID, IdpAlias: "corp-sso", Protocol: domain.IdpProtocolOIDC, Config: passingOIDCConfig()}
	d.keycloak.DeleteIdpFn = func(ctx context.Context, realmName, alias string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	rec := doRequest(d.engine(), http.MethodDelete, idpAliasPath(tenantID, "corp-sso"), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
}

// An OpenBao secret-delete failure is best-effort (logged, not returned) —
// the Keycloak IdP and the local row are already gone by that point, so
// blocking the whole removal on OpenBao's availability would leave the
// caller unable to complete an otherwise-successful delete.
func TestRemoveIdp_OpenBaoDeleteFailureIsBestEffort(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.idpConfigs.rows[idpConfigKey{tenantID, "corp-sso"}] = &domain.IdpConfig{TenantID: tenantID, IdpAlias: "corp-sso", Protocol: domain.IdpProtocolOIDC, Config: passingOIDCConfig()}
	d.keycloak.DeleteIdpFn = func(ctx context.Context, realmName, alias string) error { return nil }
	d.openbao.DeleteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string) error {
		return domain.NewError(domain.ErrObjectStoreUnavailable, "down")
	}
	rec := doRequest(d.engine(), http.MethodDelete, idpAliasPath(tenantID, "corp-sso"), sysHeaders(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 (OpenBao failure must not block removal), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRemoveIdp_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodDelete, "/api/v1/internal/tenants/not-a-uuid/idp/corp-sso", sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestPortabilityAudit_RepositoryFailure(t *testing.T) {
	d := newServiceDeps()
	d.idpConfigs.ListAllFn = func(ctx context.Context) ([]*domain.IdpConfig, error) {
		return nil, domain.NewError(domain.ErrKeycloakUnavailable, "listing store down")
	}
	rec := doRequest(d.engine(), http.MethodPost, auditPath, operatorHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
}

// ── RP-13: portability audit (operator-only, whole-estate) ──────────────

func TestPortabilityAudit_Happy(t *testing.T) {
	d := newServiceDeps()
	tenantPass, tenantWarn, tenantFail := uuid.New(), uuid.New(), uuid.New()
	d.idpConfigs.rows[idpConfigKey{tenantPass, "a"}] = &domain.IdpConfig{TenantID: tenantPass, IdpAlias: "a", Protocol: domain.IdpProtocolOIDC, Config: passingOIDCConfig()}
	d.idpConfigs.rows[idpConfigKey{tenantWarn, "b"}] = &domain.IdpConfig{TenantID: tenantWarn, IdpAlias: "b", Protocol: domain.IdpProtocolOIDC, Config: map[string]any{
		"issuer": "https://auth.bcbpsolutions.com/realms/x", "redirect_uri": "https://auth.bcbpsolutions.com/callback", "audience": "x", "client_id": "x-client",
	}}
	d.idpConfigs.rows[idpConfigKey{tenantFail, "c"}] = &domain.IdpConfig{TenantID: tenantFail, IdpAlias: "c", Protocol: domain.IdpProtocolOIDC, Config: map[string]any{
		"issuer": "https://us-east-1.auth.bcbpsolutions.com/realms/x",
	}}

	rec := doRequest(d.engine(), http.MethodPost, auditPath, operatorHeaders(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		TotalTenants    int      `json:"total_tenants"`
		TotalIdpConfigs int      `json:"total_idp_configs"`
		Passing         int      `json:"passing"`
		WarningsOnly    int      `json:"warnings_only"`
		Failing         int      `json:"failing"`
		FailingTenants  []string `json:"failing_tenants"`
	}
	decodeInto(t, rec, &body)
	if body.TotalTenants != 3 || body.TotalIdpConfigs != 3 || body.Passing != 1 || body.WarningsOnly != 1 || body.Failing != 1 {
		t.Fatalf("unexpected audit result: %+v", body)
	}
	if len(body.FailingTenants) != 1 || body.FailingTenants[0] != tenantFail.String() {
		t.Fatalf("expected failing tenant %s, got %+v", tenantFail, body.FailingTenants)
	}
}

func TestPortabilityAudit_RequiresOperatorRole(t *testing.T) {
	d := newServiceDeps()
	// ListAllFn left set to panic — a bypass reaching the service layer
	// would fail the test loudly rather than silently returning 200.
	d.idpConfigs.ListAllFn = func(ctx context.Context) ([]*domain.IdpConfig, error) {
		t.Fatal("PortabilityAudit must not be invoked for a non-operator caller")
		return nil, nil
	}
	rec := doRequest(d.engine(), http.MethodPost, auditPath, sysHeaders(), "") // iam-system only, no platform_operator
	assertStatusAndCode(t, rec, http.StatusForbidden, "insufficient_role")
}

// ── RP-8: apply realm config (local-intent-first, RP-INV-6) ─────────────

func TestApplyRealmConfig_AppliedTrue(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.ApplyRealmConfigFn = func(ctx context.Context, realmName, settingKey string, desiredValue any) error { return nil }
	rec := doRequest(d.engine(), http.MethodPatch, realmConfigPath(tenantID), sysHeaders(), `{"setting_key":"local_accounts_enabled","desired_value":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Applied     bool `json:"applied"`
		Reconciling bool `json:"reconciling"`
	}
	decodeInto(t, rec, &body)
	if !body.Applied || body.Reconciling {
		t.Fatalf("unexpected result: %+v", body)
	}
}

func TestApplyRealmConfig_TransientFailureReconciles202(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.ApplyRealmConfigFn = func(ctx context.Context, realmName, settingKey string, desiredValue any) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "transient")
	}
	rec := doRequest(d.engine(), http.MethodPatch, realmConfigPath(tenantID), sysHeaders(), `{"setting_key":"local_accounts_enabled","desired_value":false}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Applied     bool `json:"applied"`
		Reconciling bool `json:"reconciling"`
	}
	decodeInto(t, rec, &body)
	if body.Applied || !body.Reconciling {
		t.Fatalf("unexpected result: %+v", body)
	}
	if len(d.reconciliations.rows) != 1 {
		t.Fatalf("expected a pending reconciliation row to be recorded, got %d", len(d.reconciliations.rows))
	}
}

func TestApplyRealmConfig_IdpFederationKeyAllowed(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.ApplyRealmConfigFn = func(ctx context.Context, realmName, settingKey string, desiredValue any) error { return nil }
	rec := doRequest(d.engine(), http.MethodPatch, realmConfigPath(tenantID), sysHeaders(), `{"setting_key":"idp:corp-sso","desired_value":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestApplyRealmConfig_UnsupportedSetting(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	rec := doRequest(d.engine(), http.MethodPatch, realmConfigPath(tenantID), sysHeaders(), `{"setting_key":"bogus_setting","desired_value":true}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "unsupported_realm_setting")
}

func TestApplyRealmConfig_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPatch, realmConfigPath(uuid.New()), sysHeaders(), `{"setting_key":"local_accounts_enabled","desired_value":true}`)
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
}

func TestApplyRealmConfig_InvalidRequest(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	rec := doRequest(d.engine(), http.MethodPatch, realmConfigPath(tenantID), sysHeaders(), `{"desired_value":true}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestApplyRealmConfig_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPatch, "/api/v1/internal/tenants/not-a-uuid/realm-config", sysHeaders(), `{"setting_key":"local_accounts_enabled","desired_value":true}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}
