//go:build e2e

package e2e_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

type provisionTrialBody struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	TenantName string    `json:"tenant_name"`
	TenantSlug string    `json:"tenant_slug"`
	AdminEmail string    `json:"admin_email"`
	AdminName  string    `json:"admin_name"`
	Plan       string    `json:"plan"`
}

type convertBody struct {
	Plan                  string `json:"plan"`
	BillingSubscriptionID string `json:"billing_subscription_id"`
}

// TestTrialProvisionThenConvert drives RP-1 (POST provision-trial) followed
// by RP-3 (POST :id/convert) end to end: real HTTP -> real service ->
// real Keycloak testcontainer -> real Postgres testcontainer, and asserts
// on the HTTP response, the tenant_realms row, live Keycloak realm/user
// state, and the outbox_events row RP-3 must produce.
//
// TOTP is deliberately never seeded on the trial user before conversion:
// test/integration/keycloak_migrate_credentials_test.go pins a real,
// already-diagnosed production bug (MigrateUser's partialImport copies an
// "otp" credential missing its secretData, since Keycloak's GET
// /credentials never returns it) that makes Convert fail outright for any
// user with an enrolled TOTP credential. Exercising that path here would
// make this scenario assert a known bug rather than the RP-3 happy path;
// WebAuthn re-enrollment flagging is exercised at the KeycloakAdmin-adapter
// layer already (same file) and is out of scope for the HTTP-level
// assertion here — this test instead confirms the zero-credential case
// (webauthn_reenroll_required == 0) as the baseline conversion contract.
func TestTrialProvisionThenConvert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tenantID := newTenantID()
	slug := "e2e-" + uuid.NewString()[:8]
	email := slug + "@example.com"

	// ── RP-1: provision-trial ───────────────────────────────────────────
	trialReq := provisionTrialBody{
		TenantID: tenantID, TenantName: "E2E Trial Co", TenantSlug: slug,
		AdminEmail: email, AdminName: "Trial Admin", Plan: "trial",
	}
	var trialResult service.ProvisionTrialResult
	resp := doJSON(t, http.MethodPost, "/api/v1/internal/tenants/provision-trial",
		withHeader(sysHeaders(tenantID), "Idempotency-Key", newIdempotencyKey()), trialReq, &trialResult)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	assert.Equal(t, tenantID, trialResult.TenantID)
	assert.Equal(t, sharedRealm, trialResult.Realm)
	assert.Equal(t, "shared", trialResult.Strategy)
	assert.NotEqual(t, uuid.Nil, trialResult.KeycloakUserID)
	assert.True(t, trialResult.EmailVerificationSent)

	// tenant_realms row: provisioned into the shared trial realm, active.
	assertTenantRealmRow(t, ctx, tenantID, "shared", sharedRealm, "active")

	// Live Keycloak: the trial user resolves in the shared realm under
	// this tenant_id.
	trialUsers, err := kcAdmin.ListUsersByTenant(ctx, sharedRealm, tenantID)
	require.NoError(t, err)
	require.Contains(t, trialUsers, trialResult.KeycloakUserID)

	// ── RP-3: convert ────────────────────────────────────────────────────
	convertReq := convertBody{Plan: "pro", BillingSubscriptionID: "sub_" + uuid.NewString()[:8]}
	var convertResult service.ConvertResult
	resp = doJSON(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/convert",
		withHeader(sysHeaders(tenantID), "Idempotency-Key", newIdempotencyKey()), convertReq, &convertResult)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, tenantID, convertResult.TenantID)
	assert.Equal(t, "dedicated", convertResult.Strategy)
	assert.Equal(t, 1, convertResult.UsersMigrated)
	assert.Equal(t, 0, convertResult.WebauthnReenrollRequired)
	assert.True(t, convertResult.SharedSessionsTerminated)
	dedicatedRealm := convertResult.Realm
	assert.NotEqual(t, sharedRealm, dedicatedRealm)

	// tenant_realms row: now dedicated, pointing at the new realm.
	assertTenantRealmRow(t, ctx, tenantID, "dedicated", dedicatedRealm, "active")

	// Live Keycloak: sub-preserving migration — the SAME keycloak user id
	// now resolves in the dedicated realm...
	dedicatedUsers, err := kcAdmin.ListUsersByTenant(ctx, dedicatedRealm, tenantID)
	require.NoError(t, err)
	require.Contains(t, dedicatedUsers, trialResult.KeycloakUserID, "conversion must preserve the Keycloak user id (sub)")

	// ...and no longer resolves in the shared trial realm (MigrateUser
	// deletes the source-realm user as part of the cross-realm migrate).
	trialUsersAfter, err := kcAdmin.ListUsersByTenant(ctx, sharedRealm, tenantID)
	require.NoError(t, err)
	assert.Empty(t, trialUsersAfter, "the trial-realm user must be gone after migration")

	// outbox_events: TenantConverted landed for this tenant (RP-INV-3 —
	// atomic with the tenant_realms commit inside the same tx).
	assertOutboxEvent(t, ctx, tenantID, domain.EventTenantConverted)
}

func withHeader(h map[string]string, key, value string) map[string]string {
	h[key] = value
	return h
}
