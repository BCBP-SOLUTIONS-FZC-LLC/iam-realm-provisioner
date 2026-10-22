package jobs

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// deleteSharedAutomationClient removes the tenant's platform-automation
// service-account client from the shared trial realm (§2.5: RP-1 mints
// `platform-automation-<tenant_id>` there). It is the trial-teardown step that
// trial-hard-delete and orphaned-signup-reap share. The shared realm itself
// is never deleted, so without this step every removed trial leaves a client
// behind. The client name comes from tenant_realms.service_account_keycloak_client_id
// when it is recorded. Otherwise the deterministic shared name is used, since a
// row without the column may still have a client from an interrupted mint.
// DeleteServiceAccountClientIfExists is idempotent (absent or 404 is
// success), so a retried teardown converges.
func deleteSharedAutomationClient(ctx context.Context, jctx *Context, realmName string, tenantID uuid.UUID, clientID *string) error {
	name := domain.SharedPlatformAutomationClientID(tenantID)
	if clientID != nil && *clientID != "" {
		name = *clientID
	}
	return jctx.Keycloak.DeleteServiceAccountClientIfExists(ctx, realmName, name)
}

// revokeAutomationPrincipal retires the tenant's Token Service principal with
// one TS-7 call: every credential version revoked and its OpenBao material
// deleted, rotation unscheduled, and the principal set to `revoked` (LLD
// §8.7). Only orphaned-signup-reap needs it. A hard-deleted trial's tenant
// reaches O&M, whose trial-cleanup emits TenantMembershipsPurged, and on that
// event Token Service hard-deletes the principal itself. An orphan's tenant
// never existed in O&M, so no purge ever comes.
//
// TS-7 is addressed by tenant, not principal_id, so it is called even when
// tenant_realms has no service_account_principal_id: RP-1 registers the
// principal (TS-4) before it records the id, and a signup that failed in
// between still has one. Token Service's 404 PRINCIPAL_NOT_FOUND counts as
// done. Without a TokenService client a tenant with a recorded principal is
// deferred (an error), and one without is reaped.
func revokeAutomationPrincipal(ctx context.Context, jctx *Context, tenantID uuid.UUID, principalID *uuid.UUID) error {
	if jctx.TokenService == nil {
		if principalID == nil || *principalID == uuid.Nil {
			return nil
		}
		return errors.New("orphaned-signup-reap: TokenService is not set — TOKEN_SERVICE_BASE_URL is required to revoke the automation principal")
	}
	return jctx.TokenService.RevokePrincipal(ctx, tenantID)
}
