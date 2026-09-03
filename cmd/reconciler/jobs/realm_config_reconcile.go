package jobs

import (
	"context"
	"encoding/json"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// RealmConfigReconcile is RP-C4 (*/10 * * * *, §8.10, RP-INV-6): retries
// every realm_config_reconciliations row in status=pending — re-attempts
// the Keycloak apply; on success marks applied; on failure increments
// attempts and records last_error for the next tick, up to
// jctx.MaxReconciliationAttempts (the poison-pill cap) — past that a row
// moves to the terminal 'failed' status instead of retrying forever.
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
			markReconciliationFailed(ctx, jctx, rec, err.Error(), &res)
			continue
		}
		var desired any
		if err := json.Unmarshal(rec.DesiredValue, &desired); err != nil {
			markReconciliationFailed(ctx, jctx, rec, "invalid desired_value JSON: "+err.Error(), &res)
			continue
		}
		if err := jctx.Keycloak.ApplyRealmConfig(ctx, realm.RealmName, rec.SettingKey, desired); err != nil {
			jctx.Logger.WarnContext(ctx, "realm-config-reconcile: apply failed — leaving pending", "tenant_id", rec.TenantID, "setting_key", rec.SettingKey, "error", err.Error())
			markReconciliationFailed(ctx, jctx, rec, err.Error(), &res)
			continue
		}
		if err := jctx.Reconciliations.MarkApplied(ctx, rec.ID); err != nil {
			res.Failed++
			continue
		}
		res.Succeeded++
	}
	jctx.Logger.InfoContext(ctx, "realm-config-reconcile complete", "attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed, "abandoned", res.Abandoned)
	return res, nil
}

// markReconciliationFailed records lastErr on rec (poison-pill capped at
// jctx.MaxReconciliationAttempts, RP-C4 §8.10): once the underlying store
// reports the row moved to the terminal 'failed' status, this counts it as
// Abandoned and logs distinctly, so a permanently-broken setting surfaces
// for operator attention instead of silently retrying forever. A failure
// to persist MarkFailed itself is logged (not escalated) — the row simply
// stays pending for the next tick either way.
func markReconciliationFailed(ctx context.Context, jctx *Context, rec *domain.RealmConfigReconciliation, lastErr string, res *Result) {
	res.Failed++
	status, err := jctx.Reconciliations.MarkFailed(ctx, rec.ID, lastErr, jctx.MaxReconciliationAttempts)
	if err != nil {
		jctx.Logger.WarnContext(ctx, "realm-config-reconcile: mark failed did not persist", "reconciliation_id", rec.ID, "error", err.Error())
		return
	}
	if status == domain.ReconStatusFailed {
		res.Abandoned++
		jctx.Logger.WarnContext(ctx, "realm-config-reconcile: giving up after max attempts, marking permanently failed",
			"reconciliation_id", rec.ID, "tenant_id", rec.TenantID, "setting_key", rec.SettingKey, "attempts", rec.Attempts+1)
	}
}
