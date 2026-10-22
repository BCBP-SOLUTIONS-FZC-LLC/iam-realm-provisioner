package jobs

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// TrialHardDelete is RP-C2 (0 3 * * *, §8.7): for trials past
// TRIAL_GRACE_DAYS grace, hard-deletes the users from the shared realm via
// the Admin API and soft-deletes/scrubs the local tenant_realms row
// (retaining only tenant_id for audit-FK, §15.2). O&M runs its own
// separate cleanup cron for its `tenants` row — no coordination needed.
func TrialHardDelete(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	rows, err := listHardDeleteCandidates(ctx, jctx)
	if err != nil {
		return res, err
	}

	for _, tenantID := range rows {
		res.Attempted++
		if err := hardDeleteTrialTenant(ctx, jctx, tenantID); err != nil {
			jctx.Logger.WarnContext(ctx, "trial-hard-delete: defer", "tenant_id", tenantID, "error", err.Error())
			res.Failed++
			continue
		}
		res.Succeeded++
	}
	jctx.Logger.InfoContext(ctx, "trial-hard-delete complete", "attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed)
	return res, nil
}

func listHardDeleteCandidates(ctx context.Context, jctx *Context) ([]tenantRealmRow, error) {
	var out []tenantRealmRow
	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgcommon.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT tenant_id, realm_name FROM tenant_realms
			WHERE strategy = 'shared' AND status = 'disabled' AND deleted_at IS NULL
			  AND disabled_at < now() - ($1::int * interval '1 day')
			ORDER BY disabled_at ASC
			LIMIT $2`, jctx.TrialGraceDays, jctx.BatchLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r tenantRealmRow
			if err := rows.Scan(&r.TenantID, &r.RealmName); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

func hardDeleteTrialTenant(ctx context.Context, jctx *Context, row tenantRealmRow) error {
	if err := jctx.Keycloak.HardDeleteUsersInRealm(ctx, row.RealmName, row.TenantID); err != nil {
		return err
	}
	if jctx.SysPool == nil {
		return fmt.Errorf("trial-hard-delete: SysPool is not set — GDPR PII deletion cannot proceed; SYSTEM_DATABASE_URL must be configured (tenant_id=%s)", row.TenantID)
	}
	// §15.2/GDPR: scrub PII and mark deleted atomically in a single sysPool
	// transaction. Combining them prevents the inconsistent window where PII
	// is already gone but status is still 'disabled' (observable by any
	// BYPASSRLS query). All DELETEs are idempotent — safe to retry after a
	// crash because listHardDeleteCandidates re-selects by status='disabled'.
	return runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgcommon.Tx) error {
		return scrubAndTombstoneTenantRealm(ctx, tx, row.TenantID)
	})
}

// scrubAndTombstoneTenantRealm is the shared §15.2/GDPR erasure tail of
// RP-C2 (trial-hard-delete) and orphaned-signup-reap: delete every
// tenant-scoped PII row (scrubTenantPII) and soft-delete the tenant_realms
// row (status='deleted', deleted_at — only tenant_id survives, for the audit
// FK). tx must be a BYPASSRLS sysPool transaction, so the scrub and the
// tombstone commit or roll back together. The UPDATE bumps record_version
// (touch_row), so any writer holding a pre-tombstone copy of the row — e.g.
// a TrialStarted consumer racing the reaper — loses its optimistic lock.
func scrubAndTombstoneTenantRealm(ctx context.Context, tx pgcommon.Tx, tenantID uuid.UUID) error {
	if err := scrubTenantPII(ctx, tx, tenantID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE tenant_realms SET status = 'deleted', deleted_at = now(), updated_at = now()
		WHERE tenant_id = $1`, tenantID)
	return err
}

type tenantRealmRow struct {
	TenantID  uuid.UUID
	RealmName string
}
