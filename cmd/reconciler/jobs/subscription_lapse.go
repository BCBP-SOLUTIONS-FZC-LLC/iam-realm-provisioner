package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/google/uuid"
)

// SubscriptionLapse is RP-C3 (0 2 * * *, §8.8/§8.9): the daily billing-lapse
// sweep, in two independently-sourced phases:
//
//   - Suspend: O&M's I-16 GET /subscription-lapses (port.SubscriptionLapseReader)
//     names every dedicated tenant already cancelled past its grace period —
//     O&M owns that grace-period math, so RP never re-derives it. For each,
//     disable the dedicated realm, set status='suspended' with
//     suspension_source='billing_lapse', emit TenantSuspended(source=
//     'billing_lapse'). Self-idempotent on O&M's side (suspending drops the
//     tenant from the next poll) and on RP's side (the idempotency ledger).
//   - Offboard: purely local — tenant_realms rows already
//     status='suspended' AND suspension_source='billing_lapse' (never
//     'operator', §8.8) whose disabled_at is past RETENTION_DAYS
//     (port.TenantRealmRepository.ListSuspendedPastRetention). O&M's
//     endpoint carries no signal for this half; it only ever reports the
//     cancel event, not how long a tenant has been suspended. Export realm
//     to S3 THEN delete it, status='exported'->'deleted', scrub row, emit
//     TenantOffboarded (terminal, RP-INV-5). Never deletes without a
//     confirmed export.
func SubscriptionLapse(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	if jctx.SubscriptionLapses == nil {
		return res, fmt.Errorf("subscription-lapse: ORG_MEMBERSHIP_BASE_URL is not set — suspend phase cannot run; configure the env var before scheduling RP-C3")
	} else if err := runSuspendPhase(ctx, jctx, &res); err != nil {
		return res, err
	}

	if err := runOffboardPhase(ctx, jctx, &res); err != nil {
		return res, err
	}

	jctx.Logger.InfoContext(ctx, "subscription-lapse complete", "attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed, "skipped", res.Skipped)
	return res, nil
}

func runSuspendPhase(ctx context.Context, jctx *Context, res *Result) error {
	lapsed, err := jctx.SubscriptionLapses.ListLapsedSubscriptions(ctx)
	if err != nil {
		return err
	}
	for _, l := range lapsed {
		if res.Attempted >= jctx.BatchLimit {
			break
		}
		if l.RealmType != "dedicated" {
			// A "shared" entry is a trial tenant — owned by RP-C1/RP-C2's
			// own expiry/hard-delete path, not this billing-lapse sweep.
			// Anything other than "dedicated"/"shared" (empty, garbled,
			// a value O&M added without this service knowing) is folded
			// into the same Skipped counter to fail safe — never suspend
			// on ambiguous input — but it gets a distinguishing WARN so a
			// genuinely-dedicated tenant misreported by schema drift isn't
			// silently indistinguishable from a routine shared-realm skip.
			if l.RealmType != "shared" {
				jctx.Logger.WarnContext(ctx, "subscription-lapse: unexpected realm_type from O&M, skipping",
					"tenant_id", l.TenantID, "realm_type", l.RealmType)
			}
			res.Skipped++
			continue
		}
		res.Attempted++
		if err := suspendLapsedTenant(ctx, jctx, l); err != nil {
			jctx.Logger.WarnContext(ctx, "subscription-lapse: suspend defer", "tenant_id", l.TenantID, "error", err.Error())
			res.Failed++
			continue
		}
		metrics.LapseProcessedTotal.WithLabelValues("suspend").Inc()
		res.Succeeded++
	}
	return nil
}

func runOffboardPhase(ctx context.Context, jctx *Context, res *Result) error {
	remaining := jctx.BatchLimit - res.Attempted
	if remaining <= 0 {
		return nil
	}
	candidates, err := jctx.TenantRealms.ListSuspendedPastRetention(ctx, jctx.RetentionDays, remaining)
	if err != nil {
		return err
	}
	for _, realm := range candidates {
		if res.Attempted >= jctx.BatchLimit {
			break
		}
		res.Attempted++
		if err := offboardTenant(ctx, jctx, realm); err != nil {
			jctx.Logger.WarnContext(ctx, "subscription-lapse: offboard defer", "tenant_id", realm.TenantID, "error", err.Error())
			res.Failed++
			continue
		}
		metrics.LapseProcessedTotal.WithLabelValues("offboard").Inc()
		res.Succeeded++
	}
	return nil
}

func suspendLapsedTenant(ctx context.Context, jctx *Context, l port.LapsedSubscription) error {
	realm, err := jctx.TenantRealms.FindByTenantID(ctx, l.TenantID)
	if err != nil {
		return err
	}
	if realm.Status == domain.RealmStatusSuspended || realm.IsOffboarded() {
		// Already acted on — O&M's next poll should have dropped this
		// entry, but a same-day redelivery/race is a no-op, not an error.
		return nil
	}
	idemKey := "lapse-suspend:" + l.TenantID.String()
	_, found, err := jctx.Idempotency.Begin(ctx, idemKey, l.TenantID, domain.OpSuspend)
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	if err := jctx.Keycloak.DisableRealm(ctx, realm.RealmName); err != nil {
		failIdempotency(ctx, jctx, idemKey)
		return err
	}
	now := time.Now().UTC()
	err = jctx.TxRunner.RunInTx(ctx, func(ctx context.Context) error {
		realm.Status = domain.RealmStatusSuspended
		realm.DisabledAt = &now
		source := domain.SuspensionSourceBillingLapse
		realm.SuspensionSource = &source
		if err := jctx.TenantRealms.Update(ctx, realm); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		cancelledAt := l.CancelledAt.UTC().Format(time.RFC3339)
		return pub.Enqueue(ctx, &domain.Event{
			Type:     domain.EventTenantSuspended,
			TenantID: l.TenantID,
			Actor:    domain.SystemActorID,
			Data: domain.TenantSuspendedPayload{
				TenantID:    l.TenantID,
				Realm:       realm.RealmName,
				CancelledAt: &cancelledAt,
				SuspendedAt: now.Format(time.RFC3339),
				Source:      domain.SuspensionSourceBillingLapse,
			},
		})
	})
	if err != nil {
		failIdempotency(ctx, jctx, idemKey)
		return err
	}
	return jctx.Idempotency.Complete(ctx, idemKey, nil)
}

func offboardTenant(ctx context.Context, jctx *Context, realm *domain.TenantRealm) error {
	idemKey := "offboard:" + realm.TenantID.String()
	_, found, err := jctx.Idempotency.Begin(ctx, idemKey, realm.TenantID, "offboard")
	if err != nil {
		return err
	}
	if found {
		return nil
	}

	// RP-EVT-4: export MUST succeed and be durably stored before the realm
	// is ever deleted.
	export, err := jctx.Keycloak.ExportRealm(ctx, realm.RealmName)
	if err != nil {
		failIdempotency(ctx, jctx, idemKey)
		metrics.RealmExportTotal.WithLabelValues("fail").Inc()
		return err
	}
	exportKey := "realm-exports/" + realm.RealmName + "/" + uuid.NewString() + ".json"
	if err := jctx.ObjectStore.PutRealmExport(ctx, exportKey, export); err != nil {
		failIdempotency(ctx, jctx, idemKey)
		metrics.RealmExportTotal.WithLabelValues("fail").Inc()
		return err
	}
	metrics.RealmExportTotal.WithLabelValues("success").Inc()

	if err := jctx.Keycloak.DeleteRealmIfExists(ctx, realm.RealmName); err != nil {
		failIdempotency(ctx, jctx, idemKey)
		return err
	}

	// §15.2/GDPR: scrub all tenant PII from every tenant-scoped table.
	// Runs in its own sysPool transaction (idempotent on retry) before the
	// final commit so the deletions succeed even if the event-emission
	// transaction later rolls back — re-deleting already-gone rows is a no-op.
	// SysPool is always set in production; a nil check guards unit tests that
	// construct a minimal Context without a real pool.
	if jctx.SysPool == nil {
		jctx.Logger.WarnContext(ctx, "offboard: SysPool not set — GDPR PII deletion skipped; this must not occur in production", "tenant_id", realm.TenantID)
	} else if err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{
			`DELETE FROM keycloak_idp_configs            WHERE tenant_id = $1`,
			`DELETE FROM keycloak_idp_config_validations WHERE tenant_id = $1`,
			`DELETE FROM realm_config_reconciliations    WHERE tenant_id = $1`,
			`DELETE FROM provisioning_operations         WHERE tenant_id = $1`,
			`DELETE FROM pending_audit_entries           WHERE tenant_id = $1`,
		} {
			if _, err := tx.Exec(ctx, q, realm.TenantID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		failIdempotency(ctx, jctx, idemKey)
		return err
	}

	now := time.Now().UTC()
	err = jctx.TxRunner.RunInTx(ctx, func(ctx context.Context) error {
		realm.Status = domain.RealmStatusDeleted
		realm.ExportedAt = &now
		realm.ExportS3Key = &exportKey
		if err := jctx.TenantRealms.Update(ctx, realm); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		return pub.Enqueue(ctx, &domain.Event{
			Type:     domain.EventTenantOffboarded,
			TenantID: realm.TenantID,
			Actor:    domain.SystemActorID,
			Data: domain.TenantOffboardedPayload{
				TenantID:     realm.TenantID,
				Realm:        realm.RealmName,
				ExportS3Key:  exportKey,
				OffboardedAt: now.Format(time.RFC3339),
			},
		})
	})
	if err != nil {
		failIdempotency(ctx, jctx, idemKey)
		return err
	}
	return jctx.Idempotency.Complete(ctx, idemKey, nil)
}
