package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

const provisionTrialPath = "/api/v1/internal/tenants/provision-trial"
const provisionDirectPath = "/api/v1/internal/tenants/provision-direct"

func trialBody(tenantID uuid.UUID) string {
	return `{"tenant_id":"` + tenantID.String() + `","tenant_name":"Acme","tenant_slug":"acme","admin_email":"a@example.com","admin_name":"Admin","plan":"starter"}`
}

func directBody(tenantID uuid.UUID) string {
	return `{"tenant_id":"` + tenantID.String() + `","tenant_name":"Acme","tenant_slug":"acme","admin_email":"a@example.com","admin_name":"Admin","plan":"pro","billing_subscription_id":"sub_1"}`
}

// ── RP-1: provision-trial ────────────────────────────────────────────────

func TestProvisionTrial_Happy(t *testing.T) {
	d := newServiceDeps()
	kcUserID := uuid.New()
	d.keycloak.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		return kcUserID, nil
	}
	tenantID := uuid.New()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "trial-1")
	rec := doRequest(d.engine(), http.MethodPost, provisionTrialPath, headers, trialBody(tenantID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		TenantID              uuid.UUID `json:"tenant_id"`
		Realm                 string    `json:"realm"`
		Strategy              string    `json:"strategy"`
		KeycloakUserID        uuid.UUID `json:"keycloak_user_id"`
		EmailVerificationSent bool      `json:"email_verification_sent"`
	}
	decodeInto(t, rec, &body)
	if body.Realm != "trial" || body.Strategy != "shared" || body.KeycloakUserID != kcUserID || !body.EmailVerificationSent {
		t.Fatalf("unexpected result: %+v", body)
	}
	if len(d.tx.pub.events) != 1 || d.tx.pub.events[0].Type != domain.EventTrialTenantProvisioned {
		t.Fatalf("expected TrialTenantProvisioned event, got %+v", d.tx.pub.events)
	}
}

func TestProvisionTrial_MissingIdempotencyKey(t *testing.T) {
	d := newServiceDeps() // keycloak.CreateTrialUserFn left nil — a bypass would panic
	rec := doRequest(d.engine(), http.MethodPost, provisionTrialPath, sysHeaders(), trialBody(uuid.New()))
	assertStatusAndCode(t, rec, http.StatusBadRequest, "missing_idempotency_key")
}

func TestProvisionTrial_InvalidRequest(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "trial-2")
	rec := doRequest(d.engine(), http.MethodPost, provisionTrialPath, headers, `{"tenant_id":"`+uuid.New().String()+`"}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestProvisionTrial_UnsafeTenantSlug_Rejected(t *testing.T) {
	d := newServiceDeps() // keycloak.CreateTrialUserFn left nil — a bypass would panic
	tenantID := uuid.New()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "trial-slug-1")
	body := `{"tenant_id":"` + tenantID.String() + `","tenant_name":"Acme","tenant_slug":"acme/evil","admin_email":"a@example.com","admin_name":"Admin","plan":"starter"}`
	rec := doRequest(d.engine(), http.MethodPost, provisionTrialPath, headers, body)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestProvisionTrial_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	d.keycloak.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrKeycloakUnavailable, "kc down")
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "trial-3")
	rec := doRequest(d.engine(), http.MethodPost, provisionTrialPath, headers, trialBody(uuid.New()))
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
}

func TestProvisionTrial_OperationInProgress(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.idempotency.rows["trial-4"] = &domain.ProvisioningOperation{
		IdempotencyKey: "trial-4", TenantID: tenantID, Operation: "provision_trial", Status: domain.OpStatusInProgress,
	}
	// CreateTrialUserFn left nil — the replay path must never reach Keycloak.
	headers := withHeader(sysHeaders(), "Idempotency-Key", "trial-4")
	rec := doRequest(d.engine(), http.MethodPost, provisionTrialPath, headers, trialBody(tenantID))
	assertStatusAndCode(t, rec, http.StatusConflict, "operation_in_progress")
}

// ── RP-2: provision-direct ───────────────────────────────────────────────

func TestProvisionDirect_Happy(t *testing.T) {
	d := newServiceDeps()
	d.keycloak.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, templateVersion string) error { return nil }
	d.keycloak.RegisterOIDCClientsFn = func(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
		return []string{"web", "mobile"}, nil
	}
	d.keycloak.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "direct-1")
	rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, headers, directBody(uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Strategy      string   `json:"strategy"`
		KeycloakShard string   `json:"keycloak_shard"`
		OIDCClients   []string `json:"oidc_clients"`
	}
	decodeInto(t, rec, &body)
	if body.Strategy != "dedicated" || body.KeycloakShard == "" || len(body.OIDCClients) != 2 {
		t.Fatalf("unexpected result: %+v", body)
	}
	if len(d.tx.pub.events) != 3 {
		t.Fatalf("expected 3 events (TenantRealmReady+DirectPaidSignup+TenantConverted), got %d", len(d.tx.pub.events))
	}
}

func TestProvisionDirect_RealmCreateFails_NoTeardownNeeded(t *testing.T) {
	d := newServiceDeps()
	d.keycloak.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, templateVersion string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "kc down")
	}
	// DeleteRealmIfExistsFn intentionally left nil: nothing was created yet,
	// so bestEffortDeleteRealm must not be invoked.
	headers := withHeader(sysHeaders(), "Idempotency-Key", "direct-2")
	rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, headers, directBody(uuid.New()))
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
}

func TestProvisionDirect_RegisterOIDCFails_TearsDownRealm(t *testing.T) {
	d := newServiceDeps()
	d.keycloak.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, templateVersion string) error { return nil }
	d.keycloak.RegisterOIDCClientsFn = func(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
		return nil, domain.NewError(domain.ErrKeycloakUnavailable, "kc down")
	}
	torndown := false
	d.keycloak.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { torndown = true; return nil }
	headers := withHeader(sysHeaders(), "Idempotency-Key", "direct-3")
	rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, headers, directBody(uuid.New()))
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
	if !torndown {
		t.Fatal("expected compensating teardown to have run")
	}
}

func TestProvisionDirect_CreateInvitedUserFails_TearsDownRealm(t *testing.T) {
	d := newServiceDeps()
	d.keycloak.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, templateVersion string) error { return nil }
	d.keycloak.RegisterOIDCClientsFn = func(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
		return []string{"web"}, nil
	}
	d.keycloak.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrKeycloakUnavailable, "kc down")
	}
	torndown := false
	d.keycloak.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { torndown = true; return nil }
	headers := withHeader(sysHeaders(), "Idempotency-Key", "direct-4")
	rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, headers, directBody(uuid.New()))
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
	if !torndown {
		t.Fatal("expected compensating teardown to have run")
	}
}

func TestProvisionDirect_MissingIdempotencyKey(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, sysHeaders(), directBody(uuid.New()))
	assertStatusAndCode(t, rec, http.StatusBadRequest, "missing_idempotency_key")
}

func TestProvisionDirect_InvalidRequest(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "direct-5")
	rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, headers, `{"tenant_id":"not-a-uuid"}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestProvisionDirect_UnsafeTenantSlug_Rejected(t *testing.T) {
	d := newServiceDeps() // keycloak.CreateRealmFromTemplateFn left nil — a bypass would panic
	tenantID := uuid.New()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "direct-slug-1")
	body := `{"tenant_id":"` + tenantID.String() + `","tenant_name":"Acme","tenant_slug":"acme evil","admin_email":"a@example.com","admin_name":"Admin","plan":"pro"}`
	rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, headers, body)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

// ── RP-3: convert ────────────────────────────────────────────────────────

func convertPath(id uuid.UUID) string { return "/api/v1/internal/tenants/" + id.String() + "/convert" }

func sharedRealm(tenantID uuid.UUID) *domain.TenantRealm {
	return &domain.TenantRealm{TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusActive}
}

func dedicatedRealm(tenantID uuid.UUID, shard string) *domain.TenantRealm {
	return &domain.TenantRealm{
		TenantID: tenantID, RealmName: "tenant-" + tenantID.String()[:8], Strategy: domain.RealmStrategyDedicated,
		KeycloakShard: &shard, Status: domain.RealmStatusActive,
	}
}

func TestConvert_Happy(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))
	migratedUser := uuid.New()
	d.keycloak.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, templateVersion string) error { return nil }
	d.keycloak.RegisterOIDCClientsFn = func(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
		return []string{"web"}, nil
	}
	d.keycloak.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return []uuid.UUID{migratedUser}, nil
	}
	d.keycloak.MigrateUserFn = func(ctx context.Context, sourceRealm, targetRealm string, uid uuid.UUID) (bool, error) {
		return true, nil // WebAuthn re-enrollment required
	}
	forceLogoutCalled := false
	d.keycloak.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { forceLogoutCalled = true; return nil }

	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-1")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(tenantID), headers, `{"plan":"pro"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Strategy                 string `json:"strategy"`
		UsersMigrated            int    `json:"users_migrated"`
		WebauthnReenrollRequired int    `json:"webauthn_reenroll_required"`
		SharedSessionsTerminated bool   `json:"shared_sessions_terminated"`
	}
	decodeInto(t, rec, &body)
	if body.Strategy != "dedicated" || body.UsersMigrated != 1 || body.WebauthnReenrollRequired != 1 || !body.SharedSessionsTerminated {
		t.Fatalf("unexpected result: %+v", body)
	}
	if !forceLogoutCalled {
		t.Fatal("expected ForceLogoutRealm to have been called")
	}
}

func TestConvert_ForceLogoutFailureIsNonFatal(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))
	d.keycloak.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, templateVersion string) error { return nil }
	d.keycloak.RegisterOIDCClientsFn = func(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
		return nil, nil
	}
	d.keycloak.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return nil, nil
	}
	d.keycloak.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "force logout down")
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-2")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(tenantID), headers, `{"plan":"pro"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 despite non-fatal ForceLogoutRealm failure, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestConvert_RealmAlreadyDedicated(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-3")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(tenantID), headers, `{"plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusConflict, "realm_already_dedicated")
}

func TestConvert_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-4")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(uuid.New()), headers, `{"plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
}

func TestConvert_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-5")
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/convert", headers, `{"plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestConvert_RegisterOIDCFails_ConversionFailed(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))
	d.keycloak.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, templateVersion string) error { return nil }
	d.keycloak.RegisterOIDCClientsFn = func(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
		return nil, domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	torndown := false
	d.keycloak.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { torndown = true; return nil }
	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-6")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(tenantID), headers, `{"plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusBadGateway, "conversion_failed")
	if !torndown {
		t.Fatal("expected compensating teardown to have run")
	}
}

func TestConvert_ListUsersFails_ConversionFailed(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))
	d.keycloak.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, templateVersion string) error { return nil }
	d.keycloak.RegisterOIDCClientsFn = func(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
		return nil, nil
	}
	d.keycloak.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return nil, domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	d.keycloak.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }
	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-7")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(tenantID), headers, `{"plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusBadGateway, "conversion_failed")
}

func TestConvert_MigrateUserFails_ConversionFailed(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))
	d.keycloak.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, templateVersion string) error { return nil }
	d.keycloak.RegisterOIDCClientsFn = func(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
		return nil, nil
	}
	d.keycloak.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return []uuid.UUID{uuid.New()}, nil
	}
	d.keycloak.MigrateUserFn = func(ctx context.Context, sourceRealm, targetRealm string, uid uuid.UUID) (bool, error) {
		return false, domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	d.keycloak.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }
	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-8")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(tenantID), headers, `{"plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusBadGateway, "conversion_failed")
}

func TestConvert_OperationInProgress(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.idempotency.rows["convert-9"] = &domain.ProvisioningOperation{IdempotencyKey: "convert-9", TenantID: tenantID, Status: domain.OpStatusInProgress}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-9")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(tenantID), headers, `{"plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusConflict, "operation_in_progress")
}

// TestConvert_FailedIdempotencyKey_IsRetried pins the fix for a real bug: a
// `failed` row must never permanently 409 every future retry of the same
// key (idempotency_key is the table's PRIMARY KEY and nothing ever prunes
// provisioning_operations) — Begin reclaims it and the request proceeds
// into real business logic instead of short-circuiting to a replay/conflict.
// The 404 here (no such tenant_realm in the fake repo) is the proof: had the
// old found=true-for-any-existing-row behavior still been in effect, this
// would have 409'd before ever reaching the repository lookup.
func TestConvert_FailedIdempotencyKey_IsRetried(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.idempotency.rows["convert-9"] = &domain.ProvisioningOperation{IdempotencyKey: "convert-9", TenantID: tenantID, Status: domain.OpStatusFailed}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-9")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(tenantID), headers, `{"plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
}

func TestConvert_MissingIdempotencyKey(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, convertPath(uuid.New()), sysHeaders(), `{"plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "missing_idempotency_key")
}

func TestConvert_MalformedJSONBody(t *testing.T) {
	// convertRequest has no required fields, so a syntactically malformed
	// body (rather than a missing-required-field one) is needed to exercise
	// the ShouldBindJSON error branch.
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-10")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(uuid.New()), headers, `{"plan":`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

// ── RP-4: revert-conversion ──────────────────────────────────────────────

func revertPath(id uuid.UUID) string {
	return "/api/v1/internal/tenants/" + id.String() + "/revert-conversion"
}

func TestRevertConversion_Happy(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	convertedAt := timeNowMinus(1)
	realm := dedicatedRealm(tenantID, "kc-shard-1")
	realm.ConvertedAt = &convertedAt
	d.putRealm(realm)
	d.keycloak.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return []uuid.UUID{uuid.New()}, nil
	}
	d.keycloak.MigrateUserFn = func(ctx context.Context, sourceRealm, targetRealm string, uid uuid.UUID) (bool, error) {
		return false, nil
	}
	d.keycloak.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }
	headers := withHeader(sysHeaders(), "Idempotency-Key", "revert-1")
	rec := doRequest(d.engine(), http.MethodPost, revertPath(tenantID), headers, `{"reason":"customer request"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Realm    string `json:"realm"`
		Strategy string `json:"strategy"`
	}
	decodeInto(t, rec, &body)
	if body.Realm != "trial" || body.Strategy != "shared" {
		t.Fatalf("unexpected result: %+v", body)
	}
}

func TestRevertConversion_EmptyBodyIsOptional(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	convertedAt := timeNowMinus(1)
	realm := dedicatedRealm(tenantID, "kc-shard-1")
	realm.ConvertedAt = &convertedAt
	d.putRealm(realm)
	d.keycloak.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	d.keycloak.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }
	headers := withHeader(sysHeaders(), "Idempotency-Key", "revert-2")
	rec := doRequest(d.engine(), http.MethodPost, revertPath(tenantID), headers, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with empty body, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRevertConversion_WindowExpired(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	convertedAt := timeNowMinus(30 * 24)
	realm := dedicatedRealm(tenantID, "kc-shard-1")
	realm.ConvertedAt = &convertedAt
	d.putRealm(realm)
	headers := withHeader(sysHeaders(), "Idempotency-Key", "revert-3")
	rec := doRequest(d.engine(), http.MethodPost, revertPath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusConflict, "revert_window_expired")
}

func TestRevertConversion_NotDedicated_InvalidRequest(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))
	headers := withHeader(sysHeaders(), "Idempotency-Key", "revert-4")
	rec := doRequest(d.engine(), http.MethodPost, revertPath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestRevertConversion_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "revert-5")
	rec := doRequest(d.engine(), http.MethodPost, revertPath(uuid.New()), headers, "")
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
}

func TestRevertConversion_KeycloakUnavailableOnDelete(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	convertedAt := timeNowMinus(1)
	realm := dedicatedRealm(tenantID, "kc-shard-1")
	realm.ConvertedAt = &convertedAt
	d.putRealm(realm)
	d.keycloak.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	d.keycloak.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "revert-6")
	rec := doRequest(d.engine(), http.MethodPost, revertPath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
}

func TestRevertConversion_MissingIdempotencyKey(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, revertPath(uuid.New()), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "missing_idempotency_key")
}

func TestRevertConversion_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "revert-7")
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/revert-conversion", headers, "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

// ── RP-5: create invited user ────────────────────────────────────────────

func invitedUserPath(id uuid.UUID) string {
	return "/api/v1/internal/tenants/" + id.String() + "/users"
}

func TestCreateInvitedUser_Happy(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	newUserID := uuid.New()
	d.keycloak.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return newUserID, nil
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "invite-1")
	rec := doRequest(d.engine(), http.MethodPost, invitedUserPath(tenantID), headers, `{"email":"u@example.com","full_name":"User One","required_actions":["VERIFY_EMAIL"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		KeycloakUserID uuid.UUID `json:"keycloak_user_id"`
	}
	decodeInto(t, rec, &body)
	if body.KeycloakUserID != newUserID {
		t.Fatalf("unexpected result: %+v", body)
	}
}

func TestCreateInvitedUser_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "invite-2")
	rec := doRequest(d.engine(), http.MethodPost, invitedUserPath(uuid.New()), headers, `{"email":"u@example.com","full_name":"User One"}`)
	assertStatusAndCode(t, rec, http.StatusNotFound, "tenant_realm_not_found")
}

func TestCreateInvitedUser_InvalidRequest(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	headers := withHeader(sysHeaders(), "Idempotency-Key", "invite-3")
	rec := doRequest(d.engine(), http.MethodPost, invitedUserPath(tenantID), headers, `{"email":"not-an-email","full_name":"User One"}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestCreateInvitedUser_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.keycloak.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "invite-4")
	rec := doRequest(d.engine(), http.MethodPost, invitedUserPath(tenantID), headers, `{"email":"u@example.com","full_name":"User One"}`)
	assertStatusAndCode(t, rec, http.StatusBadGateway, "keycloak_unavailable")
}

func TestCreateInvitedUser_OperationInProgress(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	d.idempotency.rows["invite-5"] = &domain.ProvisioningOperation{IdempotencyKey: "invite-5", TenantID: tenantID, Status: domain.OpStatusSucceeded, Result: []byte(`{"keycloak_user_id":"` + uuid.New().String() + `"}`)}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "invite-5")
	rec := doRequest(d.engine(), http.MethodPost, invitedUserPath(tenantID), headers, `{"email":"u@example.com","full_name":"User One"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected replayed 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateInvitedUser_MissingIdempotencyKey(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, invitedUserPath(uuid.New()), sysHeaders(), `{"email":"u@example.com","full_name":"User One"}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "missing_idempotency_key")
}

func TestCreateInvitedUser_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "invite-6")
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/users", headers, `{"email":"u@example.com","full_name":"User One"}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "invalid_request")
}
