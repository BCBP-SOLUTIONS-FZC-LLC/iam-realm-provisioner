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
	assertStatusAndCode(t, rec, http.StatusConflict, "tenant_offboarded")
}

func TestReactivate_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "react-5")
	rec := doRequest(d.engine(), http.MethodPost, reactivatePath(uuid.New()), headers, "")
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
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
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
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
	assertStatusAndCode(t, rec, http.StatusConflict, "optimistic_lock_conflict")
}

func TestReactivate_OperationInProgress(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.idempotency.rows["react-8"] = &domain.ProvisioningOperation{IdempotencyKey: "react-8", TenantID: tenantID, Status: domain.OpStatusInProgress}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "react-8")
	rec := doRequest(d.engine(), http.MethodPost, reactivatePath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusConflict, "operation_in_progress")
}

func TestReactivate_MissingIdempotencyKey(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, reactivatePath(uuid.New()), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "missing_idempotency_key")
}

func TestReactivate_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "react-9")
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/reactivate", headers, "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
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
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
}

func TestGetRealm_InvalidTenantID(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodGet, "/api/v1/internal/tenants/not-a-uuid/realm", sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
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

func TestExpireTrial_DedicatedStrategy_InvalidRequest(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	headers := withHeader(sysHeaders(), "Idempotency-Key", "expire-2")
	rec := doRequest(d.engine(), http.MethodPost, expireTrialPath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestExpireTrial_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "expire-3")
	rec := doRequest(d.engine(), http.MethodPost, expireTrialPath(uuid.New()), headers, "")
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
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
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
}

func TestExpireTrial_OperationInProgress(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.idempotency.rows["expire-5"] = &domain.ProvisioningOperation{IdempotencyKey: "expire-5", TenantID: tenantID, Status: domain.OpStatusFailed}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "expire-5")
	rec := doRequest(d.engine(), http.MethodPost, expireTrialPath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusConflict, "operation_in_progress")
}

func TestExpireTrial_MissingIdempotencyKey(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, expireTrialPath(uuid.New()), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "missing_idempotency_key")
}

func TestExpireTrial_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "expire-6")
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/expire-trial", headers, "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
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
	assertStatusAndCode(t, rec, http.StatusConflict, "tenant_offboarded")
}

func TestSuspend_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(operatorHeaders(), "Idempotency-Key", "suspend-5")
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(uuid.New()), headers, `{"reason":"noop"}`)
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
}

func TestSuspend_InvalidRequest_MissingReason(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	headers := withHeader(operatorHeaders(), "Idempotency-Key", "suspend-6")
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(tenantID), headers, `{}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
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
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
}

func TestSuspend_OperationInProgress(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.idempotency.rows["suspend-8"] = &domain.ProvisioningOperation{IdempotencyKey: "suspend-8", TenantID: tenantID, Status: domain.OpStatusInProgress}
	headers := withHeader(operatorHeaders(), "Idempotency-Key", "suspend-8")
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(tenantID), headers, `{"reason":"noop"}`)
	assertStatusAndCode(t, rec, http.StatusConflict, "operation_in_progress")
}

func TestSuspend_MissingIdempotencyKeyCheckedBeforeOperatorRole(t *testing.T) {
	// Route order is idem -> RequireOperatorRole -> handler (router.go), so
	// a missing key is rejected even for an operator caller, before role
	// gating and before any dependency is touched.
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(uuid.New()), operatorHeaders(), `{"reason":"noop"}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "missing_idempotency_key")
}

func TestSuspend_RequiresOperatorRole(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	// DisableRealmFn/DisableUsersInRealmFn left nil — a bypass would panic.
	headers := withHeader(sysHeaders(), "Idempotency-Key", "suspend-9") // iam-system only, no platform_operator
	rec := doRequest(d.engine(), http.MethodPost, suspendPath(tenantID), headers, `{"reason":"noop"}`)
	assertStatusAndCode(t, rec, http.StatusForbidden, "insufficient_role")
}

func TestSuspend_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(operatorHeaders(), "Idempotency-Key", "suspend-10")
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/suspend", headers, `{"reason":"noop"}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
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
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
}

func TestDeleteUser_InvalidKeycloakUserID(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	rec := doRequest(d.engine(), http.MethodDelete, "/api/v1/internal/tenants/"+tenantID.String()+"/users/not-a-uuid", sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestDeleteUser_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	tenantID, kcUserID := uuid.New(), uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.DeleteUserFn = func(ctx context.Context, realmName string, userID uuid.UUID) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	rec := doRequest(d.engine(), http.MethodDelete, deleteUserPath(tenantID, kcUserID), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
}

func TestDeleteUser_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodDelete, "/api/v1/internal/tenants/not-a-uuid/users/"+uuid.New().String(), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
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
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
}

func TestRevokeSessions_InvalidKeycloakUserID(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/users/not-a-uuid/revoke-sessions", sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestRevokeSessions_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	tenantID, kcUserID := uuid.New(), uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.RevokeUserSessionsFn = func(ctx context.Context, realmName string, userID uuid.UUID) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	rec := doRequest(d.engine(), http.MethodPost, revokeSessionsPath(tenantID, kcUserID), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
}

func TestRevokeSessions_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/users/"+uuid.New().String()+"/revoke-sessions", sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}
