//go:build e2e

package e2e_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

// TestProvisionTrial_IdempotencyKeyReplay_NoDuplicateKeycloakUser is the
// core RP-INV correctness invariant behind RequireIdempotencyKey +
// provisioning_operations (§9.2): two calls to RP-1 with the SAME
// Idempotency-Key header must produce the same result WITHOUT a second
// Keycloak user (or a second tenant_realms row) being created — the second
// call is a stored-result replay, not a re-execution.
func TestProvisionTrial_IdempotencyKeyReplay_NoDuplicateKeycloakUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tenantID := newTenantID()
	slug := "e2e-idem-" + uuid.NewString()[:8]
	req := provisionTrialBody{
		TenantID: tenantID, TenantName: "E2E Idempotency Co", TenantSlug: slug,
		AdminEmail: slug + "@example.com", AdminName: "Idem Admin", Plan: "starter", TermsAcceptance: e2eTerms(),
	}
	key := newIdempotencyKey()
	headers := sysHeaders(tenantID)
	headers["Idempotency-Key"] = key

	var first service.ProvisionTrialResult
	resp1 := doJSON(t, http.MethodPost, "/api/v1/internal/tenants/provision-trial", headers, req, &first)
	require.Equal(t, http.StatusCreated, resp1.StatusCode)
	require.NotEqual(t, uuid.Nil, first.KeycloakUserID)

	var second service.ProvisionTrialResult
	resp2 := doJSON(t, http.MethodPost, "/api/v1/internal/tenants/provision-trial", headers, req, &second)
	require.Equal(t, http.StatusCreated, resp2.StatusCode, "a replayed idempotency key must return the original success status, not re-execute")
	require.Equal(t, first, second, "the replayed response body must be byte-for-byte identical to the original")

	// Exactly one Keycloak user exists for this tenant in the shared trial
	// realm — the second call never reached s.keycloak.CreateTrialUser.
	users, err := kcAdmin.ListUsersByTenant(ctx, sharedRealm, tenantID)
	require.NoError(t, err)
	require.Len(t, users, 1, "a replayed request must not create a second Keycloak user")
	require.Equal(t, first.KeycloakUserID, users[0])

	// Exactly one tenant_realms row exists for this tenant.
	var rowCount int
	err = rawPG.QueryRow(ctx, `SELECT count(*) FROM tenant_realms WHERE tenant_id = $1`, tenantID).Scan(&rowCount)
	require.NoError(t, err)
	require.Equal(t, 1, rowCount, "a replayed request must not create a second tenant_realms row")

	// Exactly one TermsAccepted audit entry queued, under the deterministic
	// key (A-4) — the replay enqueued nothing.
	var keys []string
	rows, err := rawPG.Query(ctx, `SELECT idempotency_key FROM pending_audit_entries WHERE tenant_id = $1`, tenantID)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		keys = append(keys, k)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"TermsAccepted:" + tenantID.String() + ":" + first.KeycloakUserID.String() + ":2026-09"}, keys)
}

// TestProvisionTrial_MissingIdempotencyKey_Rejected confirms the §10.4/§9.2
// contract at the edge: a mutating RP-1 call with no Idempotency-Key header
// at all is rejected by RequireIdempotencyKey before reaching the service
// layer (400 missing_idempotency_key), never silently treated as a
// first-time call.
func TestProvisionTrial_MissingIdempotencyKey_Rejected(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()
	slug := "e2e-noidem-" + uuid.NewString()[:8]
	req := provisionTrialBody{
		TenantID: tenantID, TenantName: "E2E No-Idem Co", TenantSlug: slug,
		AdminEmail: slug + "@example.com", AdminName: "No Idem Admin", Plan: "starter", TermsAcceptance: e2eTerms(),
	}
	resp, er := decodeErrorBody(t, http.MethodPost, "/api/v1/internal/tenants/provision-trial", sysHeaders(tenantID), req)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "MISSING_IDEMPOTENCY_KEY", er.ErrorCode)
}
