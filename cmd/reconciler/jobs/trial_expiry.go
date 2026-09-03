package jobs

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// TrialExpiry is RP-C1 (0 2 * * *, §8.7): sweeps this service's own
// tenant_realms for strategy='shared' AND status='active' AND
// trial_ends_at < now() (idx_tenant_realms_trial_exp) — never queries O&M.
// For each row, applies the idempotent per-tenant expiry effect (mark
// disabled_at, disable the tenant's shared-realm users, emit TrialExpired)
// via the same idempotency ledger RP-12 uses.
func TrialExpiry(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	rows, err := jctx.TenantRealms.ListExpiredTrials(ctx, jctx.BatchLimit)
	if err != nil {
		return res, err
	}

	for _, row := range rows {
		res.Attempted++
		key := "trial-expiry:" + row.TenantID.String() + ":" + row.TrialEndsAt.UTC().Format(time.RFC3339)
		if err := expireTrialTenant(ctx, jctx, key, row); err != nil {
			jctx.Logger.WarnContext(ctx, "trial-expiry: defer", "tenant_id", row.TenantID, "error", err.Error())
			metrics.TrialExpiryDeferredTotal.Inc()
			res.Failed++
			continue
		}
		metrics.TrialExpiryProcessedTotal.Inc()
		res.Succeeded++
	}
	jctx.Logger.InfoContext(ctx, "trial-expiry complete", "attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed)
	return res, nil
}

// expireTrialTenant applies the idempotent RP-12 effect: Keycloak-effect
// first (disable users), then local commit (tenant_realms.disabled_at +
// TrialExpired) atomically with the idempotency ledger (§9.1, §9.2).
func expireTrialTenant(ctx context.Context, jctx *Context, idemKey string, row *domain.TenantRealm) error {
	_, found, err := jctx.Idempotency.Begin(ctx, idemKey, row.TenantID, domain.OpExpireTrial)
	if err != nil {
		return err
	}
	if found {
		return nil // already handled (succeeded or a concurrent in_progress) — next tick reconciles either way
	}

	if err := jctx.Keycloak.DisableUsersInRealm(ctx, row.RealmName, row.TenantID); err != nil {
		failIdempotency(ctx, jctx, idemKey)
		return err
	}

	now := time.Now().UTC()
	graceEnds := now.AddDate(0, 0, jctx.TrialGraceDays)

	err = jctx.TxRunner.RunInTx(ctx, func(ctx context.Context) error {
		row.DisabledAt = &now
		row.Status = domain.RealmStatusDisabled
		if err := jctx.TenantRealms.Update(ctx, row); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		return pub.Enqueue(ctx, &domain.Event{
			Type:     domain.EventTrialExpired,
			TenantID: row.TenantID,
			Actor:    domain.SystemActorID,
			Data: domain.TrialExpiredPayload{
				TenantID:    row.TenantID,
				TrialEndsAt: row.TrialEndsAt.UTC().Format(time.RFC3339),
				GraceEndsAt: graceEnds.Format(time.RFC3339),
			},
		})
	})
	if err != nil {
		failIdempotency(ctx, jctx, idemKey)
		return err
	}
	result := marshalIdempotencyResult(ctx, jctx, "trial-expiry", map[string]any{"tenant_id": row.TenantID, "disabled_at": now})
	return jctx.Idempotency.Complete(ctx, idemKey, result)
}
