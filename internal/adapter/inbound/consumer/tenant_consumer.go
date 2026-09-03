package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// TenantConsumer handles tenant-realm-q (§7.1, RP-4): O&M's TrialStarted ->
// idempotent upsert of trial_ends_at onto the existing tenant_realms row
// (the row already exists — created synchronously at RP-1 before O&M's
// TrialStarted would even fire). RP never queries O&M for trial data.
// This consumer's only side effect is a Postgres write, so the projection
// update and the processed_events insert are committed in ONE transaction
// via tx (§7.1, IDEMP-2) — matching the sibling iam-org-membership
// consumer. Marking after a separate commit would let a crash in between
// replay the update on redelivery.
type TenantConsumer struct {
	realms port.TenantRealmRepository
	dedup  port.ProcessedEventsStore
	tx     port.TxRunner
	log    port.Logger
}

// NewTenantConsumer constructs a TenantConsumer for tenant-realm-q.
func NewTenantConsumer(realms port.TenantRealmRepository, dedup port.ProcessedEventsStore, tx port.TxRunner, log port.Logger) *TenantConsumer {
	return &TenantConsumer{realms: realms, dedup: dedup, tx: tx, log: log}
}

// Handle dispatches one tenant-realm-q message.
func (c *TenantConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	if env.Type != domain.EventTrialStarted {
		if err := ackUnknown(ctx, c.tx, c.dedup, c.log, domain.ConsumerTenant, env); err != nil {
			return fmt.Errorf("tenant consumer: %w", err)
		}
		return nil
	}

	processed, err := skipDuplicate(ctx, c.dedup, domain.ConsumerTenant, env.ID)
	if err != nil {
		return fmt.Errorf("tenant consumer: check processed_events: %w", err)
	}
	if processed {
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

	// One transaction for the read, the update, and the dedup mark: the
	// repositories and MarkProcessed both join the tx bound into txCtx, so
	// either the trial_ends_at write and the processed_events row both land
	// or neither does.
	gucCtx := withTenantGUC(ctx, p.TenantID)
	return c.tx.RunInTx(gucCtx, func(txCtx context.Context) error {
		row, err := c.realms.FindByTenantID(txCtx, p.TenantID)
		if err != nil {
			return fmt.Errorf("tenant consumer: find tenant_realms row: %w", err)
		}
		row.TrialEndsAt = &trialEndsAt
		if err := c.realms.Update(txCtx, row); err != nil {
			return fmt.Errorf("tenant consumer: update trial_ends_at: %w", err)
		}
		if err := c.dedup.MarkProcessed(txCtx, domain.ConsumerTenant, env.ID); err != nil {
			return fmt.Errorf("tenant consumer: mark processed: %w", err)
		}
		return nil
	})
}
