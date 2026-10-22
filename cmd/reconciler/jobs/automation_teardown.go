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

// revokeAutomationCredentials retires the tenant's Token Service credential
// material: TS-2 for every live version, which deletes its OpenBao secret and
// stops cadence rotation. Only orphaned-signup-reap needs it. A hard-deleted
// trial's tenant reaches O&M, whose trial-cleanup emits TenantMembershipsPurged,
// and on that event Token Service hard-deletes the principal itself. An
// orphan's tenant never existed in O&M, so no purge ever comes. Token Service
// has no principal delete or deregister route, so the principal row stays
// behind with every credential revoked (LLD §8.7). A NULL principal_id means
// RP-1 never registered one, and the step is skipped.
func revokeAutomationCredentials(ctx context.Context, jctx *Context, tenantID uuid.UUID, principalID *uuid.UUID) error {
	if principalID == nil || *principalID == uuid.Nil {
		return nil
	}
	if jctx.TokenService == nil {
		return errors.New("orphaned-signup-reap: TokenService is not set — TOKEN_SERVICE_BASE_URL is required to revoke the automation credential")
	}
	return jctx.TokenService.RevokeAllCredentials(ctx, tenantID, *principalID)
}
