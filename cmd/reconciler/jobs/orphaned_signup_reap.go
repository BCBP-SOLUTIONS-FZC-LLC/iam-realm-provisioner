package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// DefaultOrphanSignupAfter is ORPHAN_SIGNUP_AFTER's default: how old a
// shared-realm signup that never received TrialStarted must be before
// orphaned-signup-reap treats it as abandoned.
const DefaultOrphanSignupAfter = 48 * time.Hour

// MinOrphanSignupAfter is the floor ORPHAN_SIGNUP_AFTER is validated
// against. The reap is destructive (hard-deletes the Keycloak user), and a
// live signup's I-1 — the BFF's own call, its retries, or O&M's
// TenantIdentityProvisioned backstop — lands within minutes, so a window
// below an hour can only be a misconfiguration.
const MinOrphanSignupAfter = time.Hour

// orphanedSignupPredicate selects a tenant_realms row RP-1 wrote that O&M
// never acknowledged: a live shared-realm trial row whose trial_ends_at was
// never set by TrialStarted (tenant_consumer, RP-4), and that no other
// lifecycle path has touched. Every lifecycle column is pinned so that only
// RP-1's freshly-written shape can match:
//   - strategy='shared' — a direct-paid (RP-2) or converted (RP-3) tenant is
//     'dedicated', and also has converted_at set.
//   - status='active', deleted_at/disabled_at/suspension_source/exported_at
//     NULL — never expired, suspended, offboarded or already reaped.
//   - trial_ends_at NULL — TrialStarted never arrived (RP-10 trial
//     reactivation and RP-4 revert-conversion both set it, so a reactivated
//     trial cannot match either).
//   - converted_at NULL — never converted (revert-conversion clears it but
//     sets trial_ends_at, see above).
//
// Used verbatim by the candidate list and by the FOR UPDATE re-check inside
// the reap transaction, so the two can never disagree.
const orphanedSignupPredicate = `
	strategy = 'shared' AND status = 'active'
	AND deleted_at IS NULL AND trial_ends_at IS NULL AND converted_at IS NULL
	AND disabled_at IS NULL AND suspension_source IS NULL AND exported_at IS NULL
	AND created_at < now() - make_interval(secs => $2)`

// OrphanedSignupReap (`orphaned-signup-reap`, hourly, §8.7; Signup BFF open
// gap A-3) reaps signups that RP-1 provisioned but Org & Membership never
// created a tenant for — I-1 failed for good (slug taken and the user gave
// up, a validation failure the backstop consumer dead-letters, …). Such a
// row never gets trial_ends_at (only O&M's TrialStarted sets it), so
// trial-expiry and trial-hard-delete never see it, and its Keycloak user
// keeps the email: every later signup with that email is a new tenant_id and
// fails RP-1 with 409 USER_EMAIL_CONFLICT.
//
// A row matching orphanedSignupPredicate and older than ORPHAN_SIGNUP_AFTER
// (default 48h) is reaped the way RP-C2 hard-deletes an expired trial. Its
// users and its platform-automation client are deleted from the shared realm,
// and its Token Service credentials are revoked, which RP-C2 leaves to O&M's
// TenantMembershipsPurged; an orphan never gets that event. Then its PII is
// scrubbed and the row soft-deleted (scrubAndTombstoneTenantRealm, §15.2).
// No event is emitted — trial-hard-delete emits none either, and O&M has no
// tenant to apply one to (its EVT-17 rule would retry it into the DLQ). Each
// reap is logged at WARN with tenant_id only.
//
// Race with a late I-1: the row is re-checked with SELECT … FOR UPDATE
// inside the reap transaction and the lock is held across the Keycloak call
// until the tombstone commits. A TrialStarted that committed first fails the
// re-check (skipped); one that arrives later loses its optimistic lock on
// the tombstoned row and is dropped by tenant_consumer.
func OrphanedSignupReap(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	if jctx.OrphanSignupAfter < MinOrphanSignupAfter {
		return res, fmt.Errorf("orphaned-signup-reap: ORPHAN_SIGNUP_AFTER %s is below the %s floor", jctx.OrphanSignupAfter, MinOrphanSignupAfter)
	}
	if jctx.SysPool == nil {
		return res, errors.New("orphaned-signup-reap: SysPool is not set — the cross-tenant sweep and GDPR scrub need SYSTEM_DATABASE_URL")
	}

	candidates, err := listOrphanedSignupCandidates(ctx, jctx)
	if err != nil {
		return res, err
	}

	for _, tenantID := range candidates {
		res.Attempted++
		reaped, err := reapOrphanedSignup(ctx, jctx, tenantID)
		switch {
		case err != nil:
			jctx.Logger.WarnContext(ctx, "orphaned-signup-reap: defer", "tenant_id", tenantID, "error", err.Error())
			metrics.OrphanedSignupReapTotal.WithLabelValues("deferred").Inc()
			res.Failed++
		case !reaped:
			jctx.Logger.InfoContext(ctx, "orphaned-signup-reap: skipped — the tenant row changed since selection (TrialStarted landed)", "tenant_id", tenantID)
			metrics.OrphanedSignupReapTotal.WithLabelValues("skipped").Inc()
			res.Skipped++
		default:
			jctx.Logger.WarnContext(ctx, "orphaned-signup-reap: reaped a signup that never received TrialStarted — shared-realm users hard-deleted, row scrubbed",
				"tenant_id", tenantID, "orphan_signup_after", jctx.OrphanSignupAfter.String())
			metrics.OrphanedSignupReapTotal.WithLabelValues("reaped").Inc()
			res.Succeeded++
		}
	}
	jctx.Logger.InfoContext(ctx, "orphaned-signup-reap complete",
		"attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed, "skipped", res.Skipped)
	return res, nil
}

func listOrphanedSignupCandidates(ctx context.Context, jctx *Context) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgcommon.Tx) error {
		out = out[:0] // a retried tx must not append twice
		rows, err := tx.Query(ctx, `
			SELECT tenant_id FROM tenant_realms
			WHERE `+orphanedSignupPredicate+`
			ORDER BY created_at ASC
			LIMIT $1`, jctx.BatchLimit, jctx.OrphanSignupAfter.Seconds())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	return out, err
}

// reapOrphanedSignup runs in one sysPool transaction. It re-checks and locks
// the row, then:
//  1. hard-deletes the tenant's users,
//  2. deletes its shared-realm platform-automation client,
//  3. revokes its Token Service principal (TS-7),
//  4. scrubs and tombstones the row.
//
// It returns reaped=false, and touches nothing, when the row no longer matches
// orphanedSignupPredicate. Any failed step rolls the transaction back and
// leaves the whole tenant for the next run. Every step treats "already gone"
// as success, so a retry converges.
func reapOrphanedSignup(ctx context.Context, jctx *Context, tenantID uuid.UUID) (reaped bool, err error) {
	err = runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgcommon.Tx) error {
		reaped = false
		var realmName string
		var clientID *string
		var principalID *uuid.UUID
		scanErr := tx.QueryRow(ctx, `
			SELECT realm_name, service_account_keycloak_client_id, service_account_principal_id FROM tenant_realms
			WHERE tenant_id = $1 AND `+orphanedSignupPredicate+`
			FOR UPDATE`, tenantID, jctx.OrphanSignupAfter.Seconds()).Scan(&realmName, &clientID, &principalID)
		if errors.Is(scanErr, pgcommon.ErrNoRows) {
			return nil
		}
		if scanErr != nil {
			return scanErr
		}
		if err := jctx.Keycloak.HardDeleteUsersInRealm(ctx, realmName, tenantID); err != nil {
			return err
		}
		if err := deleteSharedAutomationClient(ctx, jctx, realmName, tenantID, clientID); err != nil {
			return err
		}
		if err := revokeAutomationPrincipal(ctx, jctx, tenantID, principalID); err != nil {
			return err
		}
		if err := scrubAndTombstoneTenantRealm(ctx, tx, tenantID); err != nil {
			return err
		}
		reaped = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return reaped, nil
}
