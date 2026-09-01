package jobs

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/google/uuid"
)

// SubscriptionLapse is RP-C3 (0 2 * * *, §8.8/§8.9): daily sweep of
// dedicated tenants' subscription_status + timestamps (read from O&M via
// jctx.SubscriptionStatus — see port.SubscriptionStatusReader's doc for the
// open coordination item on the exact O&M endpoint) driving Keycloak
// effects:
//   - cancelled past CANCELLED_GRACE_DAYS -> disable dedicated realm,
//     status='suspended', emit TenantSuspended(source='billing_lapse').
//   - suspended (billing_lapse only — §8.8 excludes operator suspensions)
//     past RETENTION_DAYS total -> export realm to S3 THEN delete it,
//     status='exported'->'deleted', scrub row, emit TenantOffboarded
//     (terminal, RP-INV-5). Never deletes without a confirmed export.
func SubscriptionLapse(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	if jctx.SubscriptionStatus == nil {
		jctx.Logger.WarnContext(ctx, "subscription-lapse: no SubscriptionStatusReader wired — skipping (see port.SubscriptionStatusReader doc)", nil)
		return res, nil
	}

	infos, err := jctx.SubscriptionStatus.ListDedicatedTenantSubscriptions(ctx)
	if err != nil {
		return res, err
	}

	now := time.Now().UTC()
	for _, info := range infos {
		if res.Attempted >= jctx.BatchLimit {
			break
		}
		switch {
		case info.SubscriptionStatus == "cancelled" && info.CancelledAt != nil && //nolint:misspell // "cancelled" matches O&M's actual enum value spelling (frozen contract)
			now.Sub(*info.CancelledAt) >= time.Duration(jctx.CancelledGraceDays)*24*time.Hour:
			res.Attempted++
			if err := suspendLapsedTenant(ctx, jctx, info); err != nil {
				jctx.Logger.WarnContext(ctx, "subscription-lapse: suspend defer", "tenant_id", info.TenantID, "error", err.Error())
				res.Failed++
				continue
			}
			metrics.LapseProcessedTotal.WithLabelValues("suspend").Inc()
			res.Succeeded++

		case info.SubscriptionStatus == "suspended" && info.SuspensionSource == "billing_lapse" &&
			info.SuspendedAt != nil && now.Sub(*info.SuspendedAt) >= time.Duration(jctx.RetentionDays)*24*time.Hour:
			res.Attempted++
			if err := offboardTenant(ctx, jctx, info); err != nil {
				jctx.Logger.WarnContext(ctx, "subscription-lapse: offboard defer", "tenant_id", info.TenantID, "error", err.Error())
				res.Failed++
				continue
			}
			metrics.LapseProcessedTotal.WithLabelValues("offboard").Inc()
			res.Succeeded++

		default:
			res.Skipped++
		}
	}
	jctx.Logger.InfoContext(ctx, "subscription-lapse complete", "attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed, "skipped", res.Skipped)
	return res, nil
}

func suspendLapsedTenant(ctx context.Context, jctx *Context, info port.TenantSubscriptionInfo) error {
	realm, err := jctx.TenantRealms.FindByTenantID(ctx, info.TenantID)
	if err != nil {
		return err
	}
	idemKey := "lapse-suspend:" + info.TenantID.String()
	_, found, err := jctx.Idempotency.Begin(ctx, idemKey, info.TenantID, domain.OpSuspend)
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
		if err := jctx.TenantRealms.Update(ctx, realm); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		var cancelledAt *string
		if info.CancelledAt != nil {
			s := info.CancelledAt.UTC().Format(time.RFC3339)
			cancelledAt = &s
		}
		return pub.EnqueueCtx(ctx, &domain.Event{
			Type:     domain.EventTenantSuspended,
			TenantID: info.TenantID,
			Actor:    domain.SystemActorID,
			Data: domain.TenantSuspendedPayload{
				TenantID:    info.TenantID,
				Realm:       realm.RealmName,
				CancelledAt: cancelledAt,
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

func offboardTenant(ctx context.Context, jctx *Context, info port.TenantSubscriptionInfo) error {
	realm, err := jctx.TenantRealms.FindByTenantID(ctx, info.TenantID)
	if err != nil {
		return err
	}
	idemKey := "offboard:" + info.TenantID.String()
	_, found, err := jctx.Idempotency.Begin(ctx, idemKey, info.TenantID, "offboard")
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
		return pub.EnqueueCtx(ctx, &domain.Event{
			Type:     domain.EventTenantOffboarded,
			TenantID: info.TenantID,
			Actor:    domain.SystemActorID,
			Data: domain.TenantOffboardedPayload{
				TenantID:     info.TenantID,
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
