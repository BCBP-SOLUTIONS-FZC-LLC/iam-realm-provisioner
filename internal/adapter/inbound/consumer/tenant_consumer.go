package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// TenantConsumer handles tenant-realm-q (§7.1, RP-4): O&M's TrialStarted ->
// idempotent upsert of trial_ends_at onto the existing tenant_realms row
// (the row already exists — created synchronously at RP-1 before O&M's
// TrialStarted would even fire). RP never queries O&M for trial data.
// This consumer's only side effect is a Postgres write, so the projection
// update and the processed_events claim are committed in ONE transaction
// through the platform-events inbox (§7.1, IDEMP-2) — matching the sibling
// iam-org-membership consumer.
type TenantConsumer struct {
	realms port.TenantRealmRepository
	inbox  port.Inbox
	log    port.Logger
}

// NewTenantConsumer constructs a TenantConsumer for tenant-realm-q.
func NewTenantConsumer(realms port.TenantRealmRepository, inbox port.Inbox, log port.Logger) *TenantConsumer {
	return &TenantConsumer{realms: realms, inbox: inbox, log: log}
}

// Handle dispatches one tenant-realm-q message.
func (c *TenantConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	if env.Type != domain.EventTrialStarted {
		if err := ackUnknown(ctx, c.inbox, c.log, domain.ConsumerTenant, env); err != nil {
			return fmt.Errorf("tenant consumer: %w", err)
		}
		return nil
	}

	var p domain.TrialStartedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return fmt.Errorf("tenant consumer: decode %s: %w", env.Type, err)
	}
	trialEndsAt, err := time.Parse(time.RFC3339, p.TrialEndsAt)
	if err != nil {
		return fmt.Errorf("tenant consumer: parse trial_ends_at %q: %w", p.TrialEndsAt, err)
	}

	// One transaction for the claim, the read and the update: the
	// repositories join the inbox's tx bound into txCtx, so either the
	// trial_ends_at write and the processed_events row both land or neither
	// does.
	gucCtx := withTenantGUC(ctx, p.TenantID)
	err = processOnce(gucCtx, c.inbox, domain.ConsumerTenant, env, func(txCtx context.Context) error {
		row, err := c.realms.FindByTenantID(txCtx, p.TenantID)
		if isTenantRealmNotFound(err) {
			// A tombstoned row means this tenant's identity was already
			// erased — typically orphaned-signup-reap beat a very late I-1
			// (§8.7, ORPHAN_SIGNUP_AFTER), or trial-hard-delete/offboarding
			// ran. Retrying can never succeed, so the event is acknowledged
			// (claimed and dropped) instead of cycling into the DLQ. A row
			// that does not exist at all keeps the retry: it is not ours to
			// guess about.
			tombstoned, terr := c.realms.IsTombstoned(txCtx, p.TenantID)
			if terr != nil {
				return fmt.Errorf("tenant consumer: check tenant_realms tombstone: %w", terr)
			}
			if tombstoned {
				c.log.Warn("tenant consumer: TrialStarted for an erased tenant (reaped orphaned signup or hard-deleted trial) — dropped; O&M holds a tenant whose identity no longer exists",
					map[string]any{"tenant_id": p.TenantID.String(), "event_id": env.ID})
				return nil
			}
		}
		if err != nil {
			return fmt.Errorf("tenant consumer: find tenant_realms row: %w", err)
		}
		row.TrialEndsAt = &trialEndsAt
		if err := c.realms.Update(txCtx, row); err != nil {
			return fmt.Errorf("tenant consumer: update trial_ends_at: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("tenant consumer: %w", err)
	}
	return nil
}

// isTenantRealmNotFound reports whether err is the repository's "no live
// tenant_realms row" signal.
func isTenantRealmNotFound(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.ErrTenantRealmNotFound
}
