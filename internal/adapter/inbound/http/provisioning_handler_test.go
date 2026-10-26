package http

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

const provisionTrialPath = "/api/v1/internal/tenants/provision-trial"
const provisionDirectPath = "/api/v1/internal/tenants/provision-direct"

// termsJSON is a valid terms_acceptance object, accepted a minute ago.
func termsJSON() string {
	return `{"version":"2026-09","accepted_at":"` + time.Now().Add(-time.Minute).UTC().Format(time.RFC3339) +
		`","ip_address":"203.0.113.7","user_agent":"Mozilla/5.0 (Macintosh)"}`
}

func trialBody(tenantID uuid.UUID) string {
	return `{"tenant_id":"` + tenantID.String() + `","tenant_name":"Acme","tenant_slug":"acme","admin_email":"a@example.com","admin_name":"Admin","plan":"starter","terms_acceptance":` + termsJSON() + `}`
}

func directBody(tenantID uuid.UUID) string {
	return `{"tenant_id":"` + tenantID.String() + `","tenant_name":"Acme","tenant_slug":"acme","admin_email":"a@example.com","admin_name":"Admin","plan":"pro","billing_subscription_id":"sub_1","terms_acceptance":` + termsJSON() + `}`
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
	if len(d.tx.pub.events) != 1 || d.tx.pub.events[0].Type != domain.EventTenantIdentityProvisioned {
		t.Fatalf("expected one TenantIdentityProvisioned event, got %+v", d.tx.pub.events)
	}
}

// RP-1 enforces the same plan set as RP-2, since TenantIdentityProvisioned
// carries the plan to O&M's I-1 under a schema enum.
func TestProvisionTrial_UnsupportedPlan_Rejected(t *testing.T) {
	d := newServiceDeps() // keycloak.CreateTrialUserFn left nil — a bypass would panic
	tenantID := uuid.New()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "trial-plan-1")
	body := `{"tenant_id":"` + tenantID.String() + `","tenant_name":"Acme","tenant_slug":"acme","admin_email":"a@example.com","admin_name":"Admin","plan":"trial"}`
	rec := doRequest(d.engine(), http.MethodPost, provisionTrialPath, headers, body)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}

func TestProvisionTrial_MissingIdempotencyKey(t *testing.T) {
	d := newServiceDeps() // keycloak.CreateTrialUserFn left nil — a bypass would panic
	rec := doRequest(d.engine(), http.MethodPost, provisionTrialPath, sysHeaders(), trialBody(uuid.New()))
	assertStatusAndCode(t, rec, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY")
}

func TestProvisionTrial_InvalidRequest(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "trial-2")
	rec := doRequest(d.engine(), http.MethodPost, provisionTrialPath, headers, `{"tenant_id":"`+uuid.New().String()+`"}`)
	// Missing required struct fields → gincommon.ShouldBindJSON returns VALIDATION_FAILED per RFC 9457.
	assertStatusAndCode(t, rec, http.StatusBadRequest, "VALIDATION_FAILED")
}

func TestProvisionTrial_UnsafeTenantSlug_Rejected(t *testing.T) {
	d := newServiceDeps() // keycloak.CreateTrialUserFn left nil — a bypass would panic
	tenantID := uuid.New()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "trial-slug-1")
	body := `{"tenant_id":"` + tenantID.String() + `","tenant_name":"Acme","tenant_slug":"acme/evil","admin_email":"a@example.com","admin_name":"Admin","plan":"starter"}`
	rec := doRequest(d.engine(), http.MethodPost, provisionTrialPath, headers, body)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}

func TestProvisionTrial_KeycloakUnavailable(t *testing.T) {
	d := newServiceDeps()
	d.keycloak.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrKeycloakUnavailable, "kc down")
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "trial-3")
	rec := doRequest(d.engine(), http.MethodPost, provisionTrialPath, headers, trialBody(uuid.New()))
	assertStatusAndCode(t, rec, http.StatusBadGateway, "KEYCLOAK_UNAVAILABLE")
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
	assertStatusAndCode(t, rec, http.StatusConflict, "OPERATION_IN_PROGRESS")
}

// ── RP-2: provision-direct ───────────────────────────────────────────────

func TestProvisionDirect_Happy(t *testing.T) {
	d := newServiceDeps()
	d.keycloak.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, templateVersion string) error { return nil }
	d.keycloak.RegisterOIDCClientsFn = func(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
		return []string{"web", "mobile"}, nil
	}
	kcUserID := uuid.New()
	d.keycloak.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return kcUserID, nil
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "direct-1")
	rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, headers, directBody(uuid.New()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Strategy       string    `json:"strategy"`
		KeycloakUserID uuid.UUID `json:"keycloak_user_id"`
		KeycloakShard  string    `json:"keycloak_shard"`
		OIDCClients    []string  `json:"oidc_clients"`
	}
	decodeInto(t, rec, &body)
	if body.Strategy != "dedicated" || body.KeycloakShard == "" || len(body.OIDCClients) != 2 {
		t.Fatalf("unexpected result: %+v", body)
	}
	// Same JSON name and type as RP-1: the BFF's O&M I-1 owner_user_id.
	if body.KeycloakUserID != kcUserID {
		t.Fatalf("keycloak_user_id = %s, want the created owner %s", body.KeycloakUserID, kcUserID)
	}
	if len(d.tx.pub.events) != 4 {
		t.Fatalf("expected 4 events (TenantIdentityProvisioned+TenantRealmReady+DirectPaidSignup+TenantConverted), got %d", len(d.tx.pub.events))
	}
}

// A retry under the same Idempotency-Key is answered from the ledger and
// still carries keycloak_user_id — a BFF whose first response was lost can
// build I-1's owner_user_id from the replay.
func TestProvisionDirect_Replay_ReturnsKeycloakUserID(t *testing.T) {
	d := newServiceDeps()
	d.keycloak.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, templateVersion string) error { return nil }
	d.keycloak.RegisterOIDCClientsFn = func(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
		return []string{"web", "mobile"}, nil
	}
	kcUserID := uuid.New()
	creates := 0
	d.keycloak.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		creates++
		return kcUserID, nil
	}
	tenantID := uuid.New()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "direct-replay-1")
	var ids [2]uuid.UUID
	for i := range ids {
		rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, headers, directBody(tenantID))
		if rec.Code != http.StatusCreated {
			t.Fatalf("call %d: expected 201, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
		var body struct {
			KeycloakUserID uuid.UUID `json:"keycloak_user_id"`
		}
		decodeInto(t, rec, &body)
		ids[i] = body.KeycloakUserID
	}
	if creates != 1 {
		t.Fatalf("the replay must not create the user again; CreateInvitedUser calls = %d", creates)
	}
	if ids[0] != kcUserID || ids[1] != kcUserID {
		t.Fatalf("keycloak_user_id first=%s replay=%s, want %s both times", ids[0], ids[1], kcUserID)
	}
}

// The user-exists conflict resume (a lost-response retry under a NEW key,
// the owner already in the realm under this tenant) returns the existing
// owner's sub.
func TestProvisionDirect_UserExistsResume_ReturnsExistingKeycloakUserID(t *testing.T) {
	d := newServiceDeps()
	d.keycloak.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, templateVersion string) error { return nil }
	d.keycloak.RegisterOIDCClientsFn = func(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
		return []string{"web"}, nil
	}
	tenantID := uuid.New()
	existingID := uuid.New()
	d.keycloak.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrUserEmailConflict, "email already exists in realm")
	}
	d.keycloak.FindUserByEmailFn = func(ctx context.Context, realmName, email string) (*port.ExistingUser, error) {
		return &port.ExistingUser{ID: existingID, TenantID: tenantID}, nil
	}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "direct-resume-1")
	rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, headers, directBody(tenantID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		KeycloakUserID uuid.UUID `json:"keycloak_user_id"`
	}
	decodeInto(t, rec, &body)
	if body.KeycloakUserID != existingID {
		t.Fatalf("keycloak_user_id = %s, want the existing owner %s", body.KeycloakUserID, existingID)
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
	assertStatusAndCode(t, rec, http.StatusBadGateway, "KEYCLOAK_UNAVAILABLE")
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
	assertStatusAndCode(t, rec, http.StatusBadGateway, "KEYCLOAK_UNAVAILABLE")
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
	assertStatusAndCode(t, rec, http.StatusBadGateway, "KEYCLOAK_UNAVAILABLE")
	if !torndown {
		t.Fatal("expected compensating teardown to have run")
	}
}

func TestProvisionDirect_MissingIdempotencyKey(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, sysHeaders(), directBody(uuid.New()))
	assertStatusAndCode(t, rec, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY")
}

func TestProvisionDirect_InvalidRequest(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "direct-5")
	rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, headers, `{"tenant_id":"not-a-uuid"}`)
	// Invalid UUID in JSON body → binding error → VALIDATION_FAILED per RFC 9457.
	assertStatusAndCode(t, rec, http.StatusBadRequest, "VALIDATION_FAILED")
}

func TestProvisionDirect_UnsafeTenantSlug_Rejected(t *testing.T) {
	d := newServiceDeps() // keycloak.CreateRealmFromTemplateFn left nil — a bypass would panic
	tenantID := uuid.New()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "direct-slug-1")
	body := `{"tenant_id":"` + tenantID.String() + `","tenant_name":"Acme","tenant_slug":"acme evil","admin_email":"a@example.com","admin_name":"Admin","plan":"pro"}`
	rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, headers, body)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}

func TestProvisionDirect_UnsupportedPlan_Rejected(t *testing.T) {
	d := newServiceDeps() // keycloak.CreateRealmFromTemplateFn left nil — a bypass would panic
	tenantID := uuid.New()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "direct-plan-1")
	body := `{"tenant_id":"` + tenantID.String() + `","tenant_name":"Acme","tenant_slug":"acme","admin_email":"a@example.com","admin_name":"Admin","plan":"gold"}`
	rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, headers, body)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
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
	assertStatusAndCode(t, rec, http.StatusConflict, "REALM_ALREADY_DEDICATED")
}

func TestConvert_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-4")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(uuid.New()), headers, `{"plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusNotFound, "TENANT_REALM_NOT_FOUND")
}

func TestConvert_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-5")
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/convert", headers, `{"plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
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
	assertStatusAndCode(t, rec, http.StatusBadGateway, "CONVERSION_FAILED")
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
	assertStatusAndCode(t, rec, http.StatusBadGateway, "CONVERSION_FAILED")
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
	assertStatusAndCode(t, rec, http.StatusBadGateway, "CONVERSION_FAILED")
}

func TestConvert_OperationInProgress(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.idempotency.rows["convert-9"] = &domain.ProvisioningOperation{IdempotencyKey: "convert-9", TenantID: tenantID, Status: domain.OpStatusInProgress}
	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-9")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(tenantID), headers, `{"plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusConflict, "OPERATION_IN_PROGRESS")
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
	assertStatusAndCode(t, rec, http.StatusNotFound, "TENANT_REALM_NOT_FOUND")
}

func TestConvert_MissingIdempotencyKey(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, convertPath(uuid.New()), sysHeaders(), `{"plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY")
}

func TestConvert_MalformedJSONBody(t *testing.T) {
	// convertRequest has no required fields, so a syntactically malformed
	// body (rather than a missing-required-field one) is needed to exercise
	// the ShouldBindJSON error branch. Malformed JSON → VALIDATION_FAILED per RFC 9457.
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "convert-10")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(uuid.New()), headers, `{"plan":`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "VALIDATION_FAILED")
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
	assertStatusAndCode(t, rec, http.StatusConflict, "REVERT_WINDOW_EXPIRED")
}

func TestRevertConversion_NotDedicated_InvalidRequest(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))
	headers := withHeader(sysHeaders(), "Idempotency-Key", "revert-4")
	rec := doRequest(d.engine(), http.MethodPost, revertPath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}

func TestRevertConversion_TenantRealmNotFound(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "revert-5")
	rec := doRequest(d.engine(), http.MethodPost, revertPath(uuid.New()), headers, "")
	assertStatusAndCode(t, rec, http.StatusNotFound, "TENANT_REALM_NOT_FOUND")
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
	assertStatusAndCode(t, rec, http.StatusBadGateway, "KEYCLOAK_UNAVAILABLE")
}

func TestRevertConversion_MissingIdempotencyKey(t *testing.T) {
	d := newServiceDeps()
	rec := doRequest(d.engine(), http.MethodPost, revertPath(uuid.New()), sysHeaders(), "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY")
}

func TestRevertConversion_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "revert-7")
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/revert-conversion", headers, "")
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
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
	assertStatusAndCode(t, rec, http.StatusNotFound, "TENANT_REALM_NOT_FOUND")
}

func TestCreateInvitedUser_InvalidRequest(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(dedicatedRealm(tenantID, "kc-shard-1"))
	headers := withHeader(sysHeaders(), "Idempotency-Key", "invite-3")
	rec := doRequest(d.engine(), http.MethodPost, invitedUserPath(tenantID), headers, `{"email":"not-an-email","full_name":"User One"}`)
	// Invalid email format → validator.ValidationErrors → VALIDATION_FAILED per RFC 9457.
	assertStatusAndCode(t, rec, http.StatusBadRequest, "VALIDATION_FAILED")
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
	assertStatusAndCode(t, rec, http.StatusBadGateway, "KEYCLOAK_UNAVAILABLE")
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
	assertStatusAndCode(t, rec, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY")
}

func TestCreateInvitedUser_InvalidTenantIDInPath(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(sysHeaders(), "Idempotency-Key", "invite-6")
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/not-a-uuid/users", headers, `{"email":"u@example.com","full_name":"User One"}`)
	assertStatusAndCode(t, rec, http.StatusBadRequest, "INVALID_REQUEST")
}

// ── CD-19: commercial-system allow-list for RP-3 and RP-4 ────────────────

// RP-2 must NOT be reachable by commercial-system (not on the CD-19 allow-list).
func TestProvisionDirect_CommercialSystem_403(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(commercialHeaders(), "Idempotency-Key", "cd19-rp2-1")
	rec := doRequest(d.engine(), http.MethodPost, "/api/v1/internal/tenants/provision-direct", headers,
		`{"tenant_name":"Acme","tenant_slug":"acme","admin_email":"a@example.com","admin_name":"Admin","plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusForbidden, "INSUFFICIENT_ROLE")
}

// RP-3 must be reachable by commercial-system (CD-19 allow-list).
func TestConvert_CommercialSystem_Allowed(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))
	d.keycloak.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, templateVersion string) error { return nil }
	d.keycloak.RegisterOIDCClientsFn = func(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
		return []string{"web"}, nil
	}
	d.keycloak.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return []uuid.UUID{uuid.New()}, nil
	}
	d.keycloak.MigrateUserFn = func(ctx context.Context, sourceRealm, targetRealm string, uid uuid.UUID) (bool, error) {
		return false, nil
	}
	d.keycloak.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }
	headers := withHeader(commercialHeaders(), "Idempotency-Key", "cd19-rp3-1")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(tenantID), headers,
		`{"plan":"pro","billing_subscription_id":"sub_123"}`)
	// RP-3 runs detached; 200 is the normal result — must NOT be 403 for commercial-system.
	if rec.Code == http.StatusForbidden {
		t.Fatalf("commercial-system must reach RP-3 (CD-19 allow-list), got 403: %s", rec.Body.String())
	}
}

// RP-4 must be reachable by commercial-system (CD-19 allow-list).
func TestRevertConversion_CommercialSystem_Allowed(t *testing.T) {
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
	headers := withHeader(commercialHeaders(), "Idempotency-Key", "cd19-rp4-1")
	rec := doRequest(d.engine(), http.MethodPost, revertPath(tenantID), headers, `{"reason":"fraud"}`)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("commercial-system must reach RP-4 (CD-19 allow-list), got 403: %s", rec.Body.String())
	}
}

// RP-4: the 7-day revert window is enforced even for commercial-system callers.
func TestRevertConversion_CommercialSystem_WindowExpired_409(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	convertedAt := timeNowMinus(30 * 24) // 30 days ago, well past the 7-day window
	realm := dedicatedRealm(tenantID, "kc-shard-1")
	realm.ConvertedAt = &convertedAt
	d.putRealm(realm)
	headers := withHeader(commercialHeaders(), "Idempotency-Key", "cd19-rp4-window-1")
	rec := doRequest(d.engine(), http.MethodPost, revertPath(tenantID), headers, "")
	assertStatusAndCode(t, rec, http.StatusConflict, "REVERT_WINDOW_EXPIRED")
}

// ── CD-19: actor recording ────────────────────────────────────────────────
//
// When commercial-system calls RP-3 or RP-4, the outbox event's Actor field
// must carry CommercialActorID (not uuid.Nil, not SystemActorID). This verifies
// the full path: commercialHeaders() → GUCBridgeMiddleware → parseBridgedIdentity
// → requestctx.UserID = CommercialActorID → callerActor(ctx) in provisioning
// service → domain.Event.Actor = CommercialActorID.

// TestConvert_CommercialSystem_RecordsActor verifies that TenantConverted
// events emitted via RP-3 carry Actor = CommercialActorID when the caller is
// the commercial-system principal (CD-19).
func TestConvert_CommercialSystem_RecordsActor(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	d.putRealm(sharedRealm(tenantID))
	d.keycloak.CreateRealmFromTemplateFn = func(ctx context.Context, realmName, templateVersion string) error { return nil }
	d.keycloak.RegisterOIDCClientsFn = func(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
		return []string{"web"}, nil
	}
	d.keycloak.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return []uuid.UUID{}, nil // zero users — keeps the test simple
	}
	d.keycloak.ForceLogoutRealmFn = func(ctx context.Context, realmName string) error { return nil }
	headers := withHeader(commercialHeaders(), "Idempotency-Key", "cd19-actor-rp3-1")
	rec := doRequest(d.engine(), http.MethodPost, convertPath(tenantID), headers,
		`{"plan":"pro","billing_subscription_id":"sub_actor"}`)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("commercial-system must reach RP-3, got 403: %s", rec.Body.String())
	}
	// Find the TenantConverted event and verify its Actor.
	var convertedEvent *domain.Event
	for _, ev := range d.tx.pub.events {
		if ev.Type == domain.EventTenantConverted {
			convertedEvent = ev
			break
		}
	}
	if convertedEvent == nil {
		t.Fatal("TenantConverted event not found in outbox")
	}
	if convertedEvent.Actor != domain.CommercialActorID {
		t.Fatalf("TenantConverted.Actor = %q, want CommercialActorID %q", convertedEvent.Actor, domain.CommercialActorID)
	}
}

// TestRevertConversion_CommercialSystem_RecordsActor verifies that
// TrialReactivated events emitted via RP-4 carry Actor = CommercialActorID
// when the caller is the commercial-system principal (CD-19).
func TestRevertConversion_CommercialSystem_RecordsActor(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	convertedAt := timeNowMinus(1) // 1 hour ago — within 7-day window
	realm := dedicatedRealm(tenantID, "kc-shard-1")
	realm.ConvertedAt = &convertedAt
	d.putRealm(realm)
	d.keycloak.ListUsersByTenantFn = func(ctx context.Context, realmName string, tid uuid.UUID) ([]uuid.UUID, error) {
		return []uuid.UUID{}, nil // zero users — keeps the test simple
	}
	d.keycloak.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }
	headers := withHeader(commercialHeaders(), "Idempotency-Key", "cd19-actor-rp4-1")
	rec := doRequest(d.engine(), http.MethodPost, revertPath(tenantID), headers, `{"reason":"fraud"}`)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("commercial-system must reach RP-4, got 403: %s", rec.Body.String())
	}
	// Find the TrialReactivated event and verify its Actor.
	var reactivatedEvent *domain.Event
	for _, ev := range d.tx.pub.events {
		if ev.Type == domain.EventTrialReactivated {
			reactivatedEvent = ev
			break
		}
	}
	if reactivatedEvent == nil {
		t.Fatal("TrialReactivated event not found in outbox")
	}
	if reactivatedEvent.Actor != domain.CommercialActorID {
		t.Fatalf("TrialReactivated.Actor = %q, want CommercialActorID %q", reactivatedEvent.Actor, domain.CommercialActorID)
	}
}

// ── terms_acceptance (Signup BFF open gap A-4) ───────────────────────────

// Every missing or invalid terms_acceptance value is a 400 INVALID_REQUEST
// on both RP-1 and RP-2, before Keycloak is touched (its fakes are left nil
// — a bypass would panic).
func TestProvision_TermsAcceptance_Invalid400(t *testing.T) {
	now := time.Now().UTC()
	terms := func(version, acceptedAt, ip, ua string) string {
		return `{"version":"` + version + `","accepted_at":"` + acceptedAt + `","ip_address":"` + ip + `","user_agent":"` + ua + `"}`
	}
	fresh := now.Add(-time.Minute).Format(time.RFC3339)
	cases := map[string]string{
		"missing object":       "",
		"null object":          `null`,
		"empty version":        terms("", fresh, "203.0.113.7", "Mozilla/5.0"),
		"33-char version":      terms(strings.Repeat("v", 33), fresh, "203.0.113.7", "Mozilla/5.0"),
		"version with slash":   terms("2026/09", fresh, "203.0.113.7", "Mozilla/5.0"),
		"accepted_at not 3339": terms("2026-09", "22/10/2026", "203.0.113.7", "Mozilla/5.0"),
		"accepted_at missing":  terms("2026-09", "", "203.0.113.7", "Mozilla/5.0"),
		"accepted_at 25h old":  terms("2026-09", now.Add(-25*time.Hour).Format(time.RFC3339), "203.0.113.7", "Mozilla/5.0"),
		"accepted_at +1h":      terms("2026-09", now.Add(time.Hour).Format(time.RFC3339), "203.0.113.7", "Mozilla/5.0"),
		"bad ip":               terms("2026-09", fresh, "not-an-ip", "Mozilla/5.0"),
		"missing ip":           terms("2026-09", fresh, "", "Mozilla/5.0"),
		"missing user agent":   terms("2026-09", fresh, "203.0.113.7", ""),
		"513-byte user agent":  terms("2026-09", fresh, "203.0.113.7", strings.Repeat("a", 513)),
		"user agent with tab":  terms("2026-09", fresh, "203.0.113.7", `Mozilla\t5.0`),
	}
	for name, termsJSON := range cases {
		for _, path := range []string{provisionTrialPath, provisionDirectPath} {
			d := newServiceDeps()
			tenantID := uuid.New()
			body := `{"tenant_id":"` + tenantID.String() + `","tenant_name":"Acme","tenant_slug":"acme","admin_email":"a@example.com","admin_name":"Admin","plan":"pro"`
			if termsJSON != "" {
				body += `,"terms_acceptance":` + termsJSON
			}
			body += `}`
			headers := withHeader(sysHeaders(), "Idempotency-Key", "terms-"+tenantID.String())
			rec := doRequest(d.engine(), http.MethodPost, path, headers, body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"errorCode":"INVALID_REQUEST"`) {
				t.Errorf("%s %s: got %d %s, want 400 INVALID_REQUEST", path, name, rec.Code, rec.Body.String())
			}
		}
	}
}
