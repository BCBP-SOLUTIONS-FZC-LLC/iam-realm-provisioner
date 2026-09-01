package jobs

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// RealmConfigReconcile is RP-C4 (*/10 * * * *, §8.10, RP-INV-6): retries
// every realm_config_reconciliations row in status=pending — re-attempts
// the Keycloak apply; on success marks applied; on failure increments
// attempts and records last_error for the next tick.
func RealmConfigReconcile(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	pending, err := jctx.Reconciliations.ListPending(ctx, jctx.BatchLimit)
	if err != nil {
		return res, err
	}

	for _, rec := range pending {
		res.Attempted++
		realm, err := jctx.TenantRealms.FindByTenantID(ctx, rec.TenantID)
		if err != nil {
			jctx.Logger.WarnContext(ctx, "realm-config-reconcile: tenant_realms lookup failed", "tenant_id", rec.TenantID, "error", err.Error())
			markReconciliationFailed(ctx, jctx, rec.ID, err.Error())
			res.Failed++
			continue
		}
		var desired any
		if err := json.Unmarshal(rec.DesiredValue, &desired); err != nil {
			markReconciliationFailed(ctx, jctx, rec.ID, "invalid desired_value JSON: "+err.Error())
			res.Failed++
			continue
		}
		if err := jctx.Keycloak.ApplyRealmConfig(ctx, realm.RealmName, rec.SettingKey, desired); err != nil {
			jctx.Logger.WarnContext(ctx, "realm-config-reconcile: apply failed — leaving pending", "tenant_id", rec.TenantID, "setting_key", rec.SettingKey, "error", err.Error())
			markReconciliationFailed(ctx, jctx, rec.ID, err.Error())
			res.Failed++
			continue
		}
		if err := jctx.Reconciliations.MarkApplied(ctx, rec.ID); err != nil {
			res.Failed++
			continue
		}
		res.Succeeded++
	}
	jctx.Logger.InfoContext(ctx, "realm-config-reconcile complete", "attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed)
	return res, nil
}

// markReconciliationFailed records lastErr on reconciliation id, logging
// (not failing the job) if that write itself fails — the row simply stays
// pending for the next tick either way.
func markReconciliationFailed(ctx context.Context, jctx *Context, id uuid.UUID, lastErr string) {
	if err := jctx.Reconciliations.MarkFailed(ctx, id, lastErr); err != nil {
		jctx.Logger.WarnContext(ctx, "realm-config-reconcile: mark failed did not persist", "reconciliation_id", id, "error", err.Error())
	}
}
