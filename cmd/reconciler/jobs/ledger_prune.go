package jobs

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
)

// LedgerPrune deletes rows past their retention window from the tables no
// other job prunes (§15 retention): provisioning_operations (the
// idempotency ledger — one row per mutating request, kept
// PROVISIONING_OPERATIONS_TTL_DAYS, default 30, far past any caller's retry
// window) and rls_violation_log (sampled RLS-6 evidence, kept
// RLS_VIOLATION_LOG_TTL_DAYS, default 90; already counted into
// rls_violations_total). keycloak_idp_config_validations is the §11.6
// audit history and is deliberately not pruned; neither are `failed`
// pending_audit_entries (undelivered audit records — alerted on instead).
func LedgerPrune(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	if jctx.Retention == nil {
		jctx.Logger.WarnContext(ctx, "ledger-prune: no retention pruner wired — skipping", nil)
		return res, nil
	}
	day := 24 * time.Hour
	ops, err := jctx.Retention.PruneProvisioningOperations(ctx, time.Duration(jctx.ProvisioningOperationsTTLDays)*day, jctx.BatchLimit)
	res.Attempted += int(ops)
	res.Succeeded += int(ops)
	if ops > 0 {
		metrics.PruneRowsTotal.WithLabelValues("ledger", "provisioning_operations").Add(float64(ops))
	}
	if err != nil {
		return res, err
	}
	rls, err := jctx.Retention.PruneRLSViolationLog(ctx, time.Duration(jctx.RLSViolationLogTTLDays)*day, jctx.BatchLimit)
	res.Attempted += int(rls)
	res.Succeeded += int(rls)
	if rls > 0 {
		metrics.PruneRowsTotal.WithLabelValues("ledger", "rls_violation_log").Add(float64(rls))
	}
	if err != nil {
		return res, err
	}
	jctx.Logger.InfoContext(ctx, "ledger-prune complete", "provisioning_operations", ops, "rls_violation_log", rls)
	return res, nil
}
