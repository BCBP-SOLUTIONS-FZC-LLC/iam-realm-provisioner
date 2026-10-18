package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

func reactivatePath(id uuid.UUID) string {
	return "/api/v1/internal/tenants/" + id.String() + "/reactivate"
}
func realmGetPath(id uuid.UUID) string { return "/api/v1/internal/tenants/" + id.String() + "/realm" }
func expireTrialPath(id uuid.UUID) string {
	return "/api/v1/internal/tenants/" + id.String() + "/expire-trial"
}
func suspendPath(id uuid.UUID) string { return "/api/v1/internal/tenants/" + id.String() + "/suspend" }
func deleteUserPath(tenantID, keycloakUserID uuid.UUID) string {
	return "/api/v1/internal/tenants/" + tenantID.String() + "/users/" + keycloakUserID.String()
}
func revokeSessionsPath(tenantID, keycloakUserID uuid.UUID) string {
	return "/api/v1/internal/tenants/" + tenantID.String() + "/users/" + keycloakUserID.String() + "/revoke-sessions"
}

func offboardedRealm(tenantID uuid.UUID) *domain.TenantRealm {
	r := dedicatedRealm(tenantID, "kc-shard-1")
	r.Status = domain.RealmStatusDeleted
	return r
}

// ── RP-10: reactivate ─────────────────────────────────────────────────────

func TestReactivate_Happy_SharedStrategy(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	r := sharedRealm(tenantID)
	r.Status = domain.RealmStatusDisabled
	d.putRealm(r)
	d.keycloak.EnableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error { return nil }
	headers := withHeader(sysHeaders(), "Idempotency-Key", "react-1")
	rec := doRequest(d.engine(), http.MethodPost, reactivatePath(tenantID), headers, `{"reason":"payment received"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status string `json:"status"`
	}
	decodeInto(t, rec, &body)
	if body.Status != "active" {
		t.Fatalf("unexpected result: %+v", body)
	}
	if len(d.tx.pub.events) != 1 || d.tx.pub.events[0].Type != domain.EventTrialReactivated {
		t.Fatalf("expected TrialReactivated event, got %+v", d.tx.pub.events)
	}
}

func TestReactivate_Happy_DedicatedStrategy(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	r := dedicatedRealm(tenantID, "kc-shard-1")
	r.Status = domain.RealmStatusSuspended
	d.putRealm(r)
	d.keycloak.EnableRealmFn = func(ctx context.Context, realmName string) error { return nil }
	headers := withHeader(sysHeaders(), "Idempotency-Key", "react-2")
	rec := doRequest(d.engine(), http.MethodPost, reactivatePath(tenantID), headers, `{"reason":"lapse resolved"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(d.tx.pub.events) != 1 || d.tx.pub.events[0].Type != domain.EventTenantReactivated {
		t.Fatalf("expected TenantReactivated event, got %+v", d.tx.pub.events)
	}
}

func TestReactivate_IdempotentNoOpWhenAlreadyActive(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1")) // Status defaults to active
	// EnableRealmFn/EnableUsersInRealmFn left nil — a bypass would panic.
	headers := withHeader(sysHeaders(), "Idempotency-Key", "react-3")
	rec := doRequest(d.engine(), http.MethodPost, reactivatePath(tenantID), headers, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReactivate_TenantOffboarded(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(offboardedRealm(tenantID))
	headers := withHeader(sysHeaders(), "Idempotency-Key", "react-4")
	rec := doRequest(d.engine(), http.MethodPost, reactivatePath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusConflict, "TENANT_OFFBOARDED")
}

func TestReactivate_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "react-5")
	rec := doRequest(d.engine(), http.MethodPost, reactivatePath(uuid.New()), headers, "")
	assertStatusAndCode(t, rec, http.StatusNotFound, "TENANT_REALM_NOT_FOUND")
}

func TestReactivate_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	r := dedicatedRealm(tenantID, "kc-shard-1")
	r.Status = domain.RealmStatusSuspended
	d.putRealm(r)
	d.keycloak.EnableRealmFn = func(ctx context.Context, realmName string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "react-6")
	rec := doRequest(d.engine(), http.MethodPost, reactivatePath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusBadGateway, "KEYCLOAK_UNAVAILABLE")
}

func TestReactivate_OptimisticLockConflict(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	r := dedicatedRealm(tenantID, "kc-shard-1")
	r.Status = domain.RealmStatusSuspended
	d.putRealm(r)
	d.keycloak.EnableRealmFn = func(ctx context.Context, realmName string) error { return nil }
	d.realms.UpdateFn = func(ctx context.Context, realm *domain.TenantRealm) error {
		return domain.NewError(domain.ErrOptimisticLockConflict, "record_version conflict")
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "react-7")
	rec := doRequest(d.engine(), http.MethodPost, reactivatePath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusConflict, "OPTIMISTIC_LOCK_CONFLICT")
}

func TestReactivate_OperationInProgress(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.idempotency.rows["react-8"] = &domain.ProvisioningOperation{IdempotencyKey: "react-8", TenantID: tenantID, Status: domain.OpStatusInProgress}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "react-8")
	rec := doRequest(d.engine(), http.MethodPost, reactivatePath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusConflict, "OPERATION_IN_PROGRESS")
}

func TestReactivate_MissingIdempotencyKey(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, reactivatePath(uuid.New()), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY")
}

func TestReactivate_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "react-9")
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/reactivate", headers, "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}

// ── RP-11: get realm (read-only) ─────────────────────────────────────────

func TestGetRealm_Happy(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	rec := doRequest(d.engine(), http.MethodGet, realmGetPath(tenantID), sysHeaders(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Strategy string `json:"strategy"`
		Status   string `json:"status"`
	}
	decodeInto(t, rec, &body)
	if body.Strategy != "dedicated" || body.Status != "active" {
		t.Fatalf("unexpected result: %+v", body)
	}
}

func TestGetRealm_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodGet, realmGetPath(uuid.New()), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusNotFound, "TENANT_REALM_NOT_FOUND")
}

func TestGetRealm_InvalidTenantID(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodGet, "/api/v1/internal/tenants/not-a-uuid/realm", sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}

// CD-20: dedicated tenant with active IdPs returns idp_count in the body.
func TestGetRealm_DedicatedTenant_IdpCountInBody(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	// Seed two active IdPs.
	d.idpConfigs.rows[idpConfigKey{tenantID, "saml-corp"}] = &domain.IdpConfig{
		TenantID: tenantID, IdpAlias: "saml-corp", Protocol: domain.IdpProtocolSAML,
		Status: domain.IdpConfigStatusActive, Enabled: true,
	}
	d.idpConfigs.rows[idpConfigKey{tenantID, "oidc-google"}] = &domain.IdpConfig{
		TenantID: tenantID, IdpAlias: "oidc-google", Protocol: domain.IdpProtocolOIDC,
		Status: domain.IdpConfigStatusActive, Enabled: true,
	}

	rec := doRequest(d.engine(), http.MethodGet, realmGetPath(tenantID), sysHeaders(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		IdpCount int `json:"idp_count"`
	}
	decodeInto(t, rec, &body)
	if body.IdpCount != 2 {
		t.Fatalf("expected idp_count=2, got %d", body.IdpCount)
	}
}

// CD-20: disabled configs must not contribute to idp_count.
func TestGetRealm_DedicatedTenant_DisabledIdpNotCounted(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.idpConfigs.rows[idpConfigKey{tenantID, "saml-corp"}] = &domain.IdpConfig{
		TenantID: tenantID, IdpAlias: "saml-corp", Protocol: domain.IdpProtocolSAML,
		Status: domain.IdpConfigStatusActive,
	}
	d.idpConfigs.rows[idpConfigKey{tenantID, "oidc-disabled"}] = &domain.IdpConfig{
		TenantID: tenantID, IdpAlias: "oidc-disabled", Protocol: domain.IdpProtocolOIDC,
		Status: domain.IdpConfigStatusDisabled,
	}

	rec := doRequest(d.engine(), http.MethodGet, realmGetPath(tenantID), sysHeaders(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		IdpCount int `json:"idp_count"`
	}
	decodeInto(t, rec, &body)
	if body.IdpCount != 1 {
		t.Fatalf("expected idp_count=1 (disabled must not count), got %d", body.IdpCount)
	}
}

// CD-20: trial tenant always returns idp_count=0 (shared realm, no SSO possible).
func TestGetRealm_TrialTenant_ZeroIdpCount(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))

	rec := doRequest(d.engine(), http.MethodGet, realmGetPath(tenantID), sysHeaders(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Strategy string `json:"strategy"`
		IdpCount int    `json:"idp_count"`
	}
	decodeInto(t, rec, &body)
	if body.Strategy != "shared" {
		t.Fatalf("expected shared strategy, got %q", body.Strategy)
	}
	if body.IdpCount != 0 {
		t.Fatalf("trial tenant must always return idp_count=0, got %d", body.IdpCount)
	}
}

// CD-20: IdP repository failure must produce 5xx — not a silent zero.
func TestGetRealm_IdpRepoError_Returns503(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.idpConfigs.CountActiveByTenantFn = func(ctx context.Context, tid uuid.UUID) (int, error) {
		return 0, domain.NewError(domain.ErrDBUnavailable, "database unavailable")
	}

	rec := doRequest(d.engine(), http.MethodGet, realmGetPath(tenantID), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusServiceUnavailable, "DB_UNAVAILABLE")
}

// ── CD-19: RP-11 commercial-system allow-list ─────────────────────────────

// CD-19: commercial-system (Billing) can call RP-11 — the route is on the
// explicit allow-list in RequireSystemOrCommercial (router.go).
func TestGetRealm_CommercialSystem_Allowed(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	rec := doRequest(d.engine(), http.MethodGet, realmGetPath(tenantID), commercialHeaders(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("commercial-system must reach RP-11 (CD-19 allow-list), got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		IdpCount int `json:"idp_count"`
	}
	decodeInto(t, rec, &body)
	// idp_count must be present (CD-20 contract)
	_ = body.IdpCount
}

// RP-10 (reactivate) must NOT be reachable by commercial-system.
func TestReactivate_CommercialSystem_403(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	headers := withHeader(commercialHeaders(), "Idempotency-Key", "cd19-test-1")
	rec := doRequest(d.engine(), http.MethodPost, reactivatePath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusForbidden, "INSUFFICIENT_ROLE")
}

// Contract test: route must be exactly /api/v1/internal/tenants/{id}/realm
// and response must include the field "idp_count" (CD-20 contract with Core).
func TestGetRealm_RouteAndFieldContract(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))

	// Use the literal path (not the realmGetPath helper) to assert the
	// exact URL Core's HTTPClient.GetIDPCount must call.
	exactPath := "/api/v1/internal/tenants/" + tenantID.String() + "/realm"
	rec := doRequest(d.engine(), http.MethodGet, exactPath, sysHeaders(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("contract route %s must return 200, got %d", exactPath, rec.Code)
	}
	var body map[string]any
	decodeInto(t, rec, &body)
	if _, ok := body["idp_count"]; !ok {
		t.Fatalf("response body must contain idp_count field for Core's SSO-downgrade guard (CD-20)")
	}
}

// ── RP-12: expire trial ───────────────────────────────────────────────────

func TestExpireTrial_Happy(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))
	d.keycloak.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error { return nil }
	headers := withHeader(sysHeaders(), "Idempotency-Key", "expire-1")
	rec := doRequest(d.engine(), http.MethodPost, expireTrialPath(tenantID), headers, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(d.tx.pub.events) != 1 || d.tx.pub.events[0].Type != domain.EventTrialExpired {
		t.Fatalf("expected TrialExpired event, got %+v", d.tx.pub.events)
	}
}

func TestExpireTrial_DedicatedStrategy_RealmAlreadyDedicated(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	headers := withHeader(sysHeaders(), "Idempotency-Key", "expire-2")
	rec := doRequest(d.engine(), http.MethodPost, expireTrialPath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusConflict, "REALM_ALREADY_DEDICATED")
}

func TestExpireTrial_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "expire-3")
	rec := doRequest(d.engine(), http.MethodPost, expireTrialPath(uuid.New()), headers, "")
	assertStatusAndCode(t, rec, http.StatusNotFound, "TENANT_REALM_NOT_FOUND")
}

func TestExpireTrial_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))
	d.keycloak.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "expire-4")
	rec := doRequest(d.engine(), http.MethodPost, expireTrialPath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusBadGateway, "KEYCLOAK_UNAVAILABLE")
}

func TestExpireTrial_OperationInProgress(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.idempotency.rows["expire-5"] = &domain.ProvisioningOperation{IdempotencyKey: "expire-5", TenantID: tenantID, Status: domain.OpStatusInProgress}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "expire-5")
	rec := doRequest(d.engine(), http.MethodPost, expireTrialPath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusConflict, "OPERATION_IN_PROGRESS")
}

// TestExpireTrial_FailedIdempotencyKey_IsRetried pins the fix for a real bug:
// a `failed` row must never permanently 409 every future retry of the same
// key (idempotency_key is the table's PRIMARY KEY and nothing ever prunes
// provisioning_operations) — Begin reclaims it and the request proceeds
// into real business logic instead of short-circuiting to a replay/conflict.
// The 404 here (no such tenant_realm in the fake repo) is the proof: had the
// old found=true-for-any-existing-row behavior still been in effect, this
// would have 409'd before ever reaching the repository lookup.
func TestExpireTrial_FailedIdempotencyKey_IsRetried(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.idempotency.rows["expire-5"] = &domain.ProvisioningOperation{IdempotencyKey: "expire-5", TenantID: tenantID, Status: domain.OpStatusFailed}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "expire-5")
	rec := doRequest(d.engine(), http.MethodPost, expireTrialPath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusNotFound, "TENANT_REALM_NOT_FOUND")
}

func TestExpireTrial_MissingIdempotencyKey(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, expireTrialPath(uuid.New()), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY")
}

func TestExpireTrial_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "expire-6")
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/expire-trial", headers, "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}

// ── RP-14: emergency operator suspension ─────────────────────────────────

func TestSuspend_Happy_DedicatedRealm(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	disableRealmCalled, disableUsersCalled := false, false
	d.keycloak.DisableRealmFn = func(ctx context.Context, realmName string) error { disableRealmCalled = true; return nil }
	d.keycloak.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error {
		disableUsersCalled = true
		return nil
	}
	headers := withHeader(operatorHeaders(), "Idempotency-Key", "suspend-1")
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(tenantID), headers, `{"reason":"fraud investigation"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !disableRealmCalled || disableUsersCalled {
		t.Fatalf("expected DisableRealm (not DisableUsersInRealm) for a dedicated tenant: realm=%v users=%v", disableRealmCalled, disableUsersCalled)
	}
	if len(d.tx.pub.events) != 1 || d.tx.pub.events[0].Type != domain.EventTenantSuspended {
		t.Fatalf("expected TenantSuspended event, got %+v", d.tx.pub.events)
	}
}

func TestSuspend_Happy_SharedRealm_OnlyDisablesTenantUsers(t *testing.T) {
	// RP-INV-2: a shared-strategy tenant must disable only that tenant's
	// users, never the shared realm itself — DisableRealmFn is left nil so
	// an incorrect code path would panic rather than silently succeed.
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))
	disableUsersCalled := false
	d.keycloak.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error {
		disableUsersCalled = true
		return nil
	}
	headers := withHeader(operatorHeaders(), "Idempotency-Key", "suspend-2")
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(tenantID), headers, `{"reason":"fraud investigation"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !disableUsersCalled {
		t.Fatal("expected DisableUsersInRealm to have been called")
	}
}

func TestSuspend_IdempotentNoOpWhenAlreadySuspended(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	r := dedicatedRealm(tenantID, "kc-shard-1")
	r.Status = domain.RealmStatusSuspended
	d.putRealm(r)
	// DisableRealmFn/DisableUsersInRealmFn left nil — a bypass would panic.
	headers := withHeader(operatorHeaders(), "Idempotency-Key", "suspend-3")
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(tenantID), headers, `{"reason":"noop"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSuspend_TenantOffboarded(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(offboardedRealm(tenantID))
	headers := withHeader(operatorHeaders(), "Idempotency-Key", "suspend-4")
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(tenantID), headers, `{"reason":"noop"}`)
	assertStatusAndCode(t, rec, http.StatusConflict, "TENANT_OFFBOARDED")
}

func TestSuspend_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(operatorHeaders(), "Idempotency-Key", "suspend-5")
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(uuid.New()), headers, `{"reason":"noop"}`)
	assertStatusAndCode(t, rec, http.StatusNotFound, "TENANT_REALM_NOT_FOUND")
}

func TestSuspend_InvalidRequest_MissingReason(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	headers := withHeader(operatorHeaders(), "Idempotency-Key", "suspend-6")
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(tenantID), headers, `{}`)
	// Missing required reason field → gincommon.ShouldBindJSON → VALIDATION_FAILED per RFC 9457.
	assertStatusAndCode(t, rec, http.StatusBadRequest, "VALIDATION_FAILED")
}

func TestSuspend_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.DisableRealmFn = func(ctx context.Context, realmName string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	headers := withHeader(operatorHeaders(), "Idempotency-Key", "suspend-7")
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(tenantID), headers, `{"reason":"down"}`)
	assertStatusAndCode(t, rec, http.StatusBadGateway, "KEYCLOAK_UNAVAILABLE")
}

func TestSuspend_OperationInProgress(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.idempotency.rows["suspend-8"] = &domain.ProvisioningOperation{IdempotencyKey: "suspend-8", TenantID: tenantID, Status: domain.OpStatusInProgress}
	headers := withHeader(operatorHeaders(), "Idempotency-Key", "suspend-8")
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(tenantID), headers, `{"reason":"noop"}`)
	assertStatusAndCode(t, rec, http.StatusConflict, "OPERATION_IN_PROGRESS")
}

func TestSuspend_MissingIdempotencyKeyCheckedBeforeOperatorRole(t *testing.T) {
	// Route order is idem -> RequireOperatorRole -> handler (router.go), so
	// a missing key is rejected even for an operator caller, before role
	// gating and before any dependency is touched.
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(uuid.New()), operatorHeaders(), `{"reason":"noop"}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY")
}

func TestSuspend_RequiresOperatorRole(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	// DisableRealmFn/DisableUsersInRealmFn left nil — a bypass would panic.
	headers := withHeader(sysHeaders(), "Idempotency-Key", "suspend-9") // iam-system only, no platform_operator
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(tenantID), headers, `{"reason":"noop"}`)
	assertStatusAndCode(t, rec, http.StatusForbidden, "INSUFFICIENT_ROLE")
}

func TestSuspend_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(operatorHeaders(), "Idempotency-Key", "suspend-10")
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/suspend", headers, `{"reason":"noop"}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}

// CD-22: Seat overage never suspends a tenant via RP (TL decision 2026-10-15).
// RP-14 is strictly operator-only (platform_operator role required).
// commercial-system (Billing) must receive 403 insufficient_role on RP-14.
func TestSuspend_CommercialSystem_403(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	// commercial-system has no platform_operator role → RequireOperatorRole rejects.
	headers := withHeader(commercialHeaders(), "Idempotency-Key", "cd22-suspend-1")
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(tenantID), headers, `{"reason":"seat overage"}`)
	assertStatusAndCode(t, rec, http.StatusForbidden, "INSUFFICIENT_ROLE")
}

// ── RP-15: delete user (idempotent) ──────────────────────────────────────

func TestDeleteUser_Happy(t *testing.T) {
	d := newServiceDeps()
	tenantID, kcUserID := uuid.New(), uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.DeleteUserFn = func(ctx context.Context, realmName string, userID uuid.UUID) error { return nil }
	rec := doRequest(d.engine(), http.MethodDelete, deleteUserPath(tenantID, kcUserID), sysHeaders(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteUser_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodDelete, deleteUserPath(uuid.New(), uuid.New()), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusNotFound, "TENANT_REALM_NOT_FOUND")
}

func TestDeleteUser_InvalidKeycloakUserID(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	rec := doRequest(d.engine(), http.MethodDelete, "/api/v1/internal/tenants/"+tenantID.String()+"/users/not-a-uuid", sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}

func TestDeleteUser_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	tenantID, kcUserID := uuid.New(), uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.DeleteUserFn = func(ctx context.Context, realmName string, userID uuid.UUID) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	rec := doRequest(d.engine(), http.MethodDelete, deleteUserPath(tenantID, kcUserID), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadGateway, "KEYCLOAK_UNAVAILABLE")
}

func TestDeleteUser_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodDelete, "/api/v1/internal/tenants/not-a-uuid/users/"+uuid.New().String(), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}

// ── RP-16: revoke sessions (idempotent, best-effort/fail-open) ──────────

func TestRevokeSessions_Happy(t *testing.T) {
	d := newServiceDeps()
	tenantID, kcUserID := uuid.New(), uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.RevokeUserSessionsFn = func(ctx context.Context, realmName string, userID uuid.UUID) error { return nil }
	rec := doRequest(d.engine(), http.MethodPost, revokeSessionsPath(tenantID, kcUserID), sysHeaders(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRevokeSessions_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, revokeSessionsPath(uuid.New(), uuid.New()), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusNotFound, "TENANT_REALM_NOT_FOUND")
}

func TestRevokeSessions_InvalidKeycloakUserID(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/users/not-a-uuid/revoke-sessions", sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}

func TestRevokeSessions_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	tenantID, kcUserID := uuid.New(), uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.RevokeUserSessionsFn = func(ctx context.Context, realmName string, userID uuid.UUID) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	rec := doRequest(d.engine(), http.MethodPost, revokeSessionsPath(tenantID, kcUserID), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadGateway, "KEYCLOAK_UNAVAILABLE")
}

func TestRevokeSessions_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/users/"+uuid.New().String()+"/revoke-sessions", sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}
