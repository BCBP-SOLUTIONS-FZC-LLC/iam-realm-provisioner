package jobs

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
)

// gdprScrubQueries is the single authoritative list of tenant-scoped tables
// whose rows must be deleted on hard-delete and offboarding (§15.2/GDPR).
// Add a DELETE here whenever a new RLS-scoped table that holds PII is
// introduced — both RP-C1 (trial-hard-delete) and RP-C3 (subscription-lapse
// offboard) call scrubTenantPII, so one update covers both erasure paths.
var gdprScrubQueries = []string{
	`DELETE FROM keycloak_idp_configs            WHERE tenant_id = $1`,
	`DELETE FROM keycloak_idp_config_validations WHERE tenant_id = $1`,
	`DELETE FROM realm_config_reconciliations    WHERE tenant_id = $1`,
	`DELETE FROM provisioning_operations         WHERE tenant_id = $1`,
	`DELETE FROM pending_audit_entries           WHERE tenant_id = $1`,
}

// scrubTenantPII executes gdprScrubQueries for tenantID inside tx, which must
// be a BYPASSRLS sysPool transaction. All DELETEs are idempotent — re-running
// against already-absent rows is a safe no-op and is expected on retry.
func scrubTenantPII(ctx context.Context, tx pgcommon.Tx, tenantID uuid.UUID) error {
	for _, q := range gdprScrubQueries {
		if _, err := tx.Exec(ctx, q, tenantID); err != nil {
			return err
		}
	}
	return nil
}

// NewGDPRScrubFn returns the production Context.GDPRScrubFn implementation:
// runs scrubTenantPII inside a single BYPASSRLS sysPool transaction so all
// five DELETEs are atomic. Wire this in cmd/reconciler/main.go; unit tests
// inject a no-op instead.
func NewGDPRScrubFn(sysPool *pgcommon.Pool) func(ctx context.Context, tenantID uuid.UUID) error {
	return func(ctx context.Context, tenantID uuid.UUID) error {
		return runInTxWithSysPool(ctx, sysPool, func(ctx context.Context, tx pgcommon.Tx) error {
			return scrubTenantPII(ctx, tx, tenantID)
		})
	}
}
