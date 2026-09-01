//go:build e2e

package e2e_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

type suspendBody struct {
	Reason string `json:"reason"`
}

// TestSuspendThenReactivate drives RP-14 (POST :id/suspend, operator-only)
// followed by RP-10 (POST :id/reactivate) against a dedicated-strategy
// tenant, asserting both the tenant_realms.status transition and the live
// Keycloak realm's Enabled flag at each step.
func TestSuspendThenReactivate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tenantID, dedicatedRealm := provisionDedicatedTenant(t)

	client, tok, err := rawKeycloakClient(ctx)
	require.NoError(t, err)

	// Freshly provisioned: realm is enabled.
	realmRep, err := client.GetRealm(ctx, tok, dedicatedRealm)
	require.NoError(t, err)
	require.NotNil(t, realmRep.Enabled)
	require.True(t, *realmRep.Enabled)

	// ── RP-14: suspend (operator role required) ─────────────────────────
	var suspendResult service.SuspendResult
	resp := doJSON(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/suspend",
		withHeader(operatorHeaders(tenantID), "Idempotency-Key", newIdempotencyKey()),
		suspendBody{Reason: "e2e emergency suspend test"}, &suspendResult)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "suspended", suspendResult.Status)

	assertTenantRealmRow(t, ctx, tenantID, "dedicated", dedicatedRealm, "suspended")

	realmRep, err = client.GetRealm(ctx, tok, dedicatedRealm)
	require.NoError(t, err)
	require.NotNil(t, realmRep.Enabled)
	require.False(t, *realmRep.Enabled, "the dedicated realm must be disabled in Keycloak after RP-14")

	// A non-operator caller (iam-system only) must be rejected by
	// RequireOperatorRole — defense-in-depth on the same route.
	resp2, er := decodeErrorBody(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/suspend",
		withHeader(sysHeaders(tenantID), "Idempotency-Key", newIdempotencyKey()),
		suspendBody{Reason: "should be rejected"})
	require.Equal(t, http.StatusForbidden, resp2.StatusCode)
	require.Equal(t, "insufficient_role", er.Error)

	// ── RP-10: reactivate ────────────────────────────────────────────────
	var reactivateResult service.ReactivateResult
	resp = doJSON(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/reactivate",
		withHeader(sysHeaders(tenantID), "Idempotency-Key", newIdempotencyKey()), nil, &reactivateResult)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "active", reactivateResult.Status)

	assertTenantRealmRow(t, ctx, tenantID, "dedicated", dedicatedRealm, "active")

	realmRep, err = client.GetRealm(ctx, tok, dedicatedRealm)
	require.NoError(t, err)
	require.NotNil(t, realmRep.Enabled)
	require.True(t, *realmRep.Enabled, "the dedicated realm must be re-enabled in Keycloak after RP-10")
}
