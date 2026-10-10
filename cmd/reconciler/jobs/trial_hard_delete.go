package jobs

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
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
	return runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		// §15.2/GDPR: scrub all tenant PII from every tenant-scoped table in
		// one atomic transaction before marking the row deleted. DELETE is
		// idempotent — safe to retry if the tx commits after a crash.
		// outbox_events (platform-events schema) has a tenant_id column but is
		// not deleted here; its prune job clears it within OUTBOX_RETENTION_DAYS
		// (default 8 days) after the events are published. Deleting it
		// synchronously would silently drop the TenantOffboarded event.
		for _, q := range []string{
			`DELETE FROM keycloak_idp_configs            WHERE tenant_id = $1`,
			`DELETE FROM keycloak_idp_config_validations WHERE tenant_id = $1`,
			`DELETE FROM realm_config_reconciliations    WHERE tenant_id = $1`,
			`DELETE FROM provisioning_operations         WHERE tenant_id = $1`,
			`DELETE FROM pending_audit_entries           WHERE tenant_id = $1`,
		} {
			if _, err := tx.Exec(ctx, q, row.TenantID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `
			UPDATE tenant_realms SET status = 'deleted', deleted_at = now(), updated_at = now()
			WHERE tenant_id = $1`, row.TenantID)
		return err
	})
}

type tenantRealmRow struct {
	TenantID  uuid.UUID
	RealmName string
}
