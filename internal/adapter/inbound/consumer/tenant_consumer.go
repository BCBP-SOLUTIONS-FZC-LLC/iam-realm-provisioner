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
type TenantConsumer struct {
	realms port.TenantRealmRepository
	dedup  port.ProcessedEventsStore
	log    port.Logger
}

// NewTenantConsumer constructs a TenantConsumer for tenant-realm-q.
func NewTenantConsumer(realms port.TenantRealmRepository, dedup port.ProcessedEventsStore, log port.Logger) *TenantConsumer {
	return &TenantConsumer{realms: realms, dedup: dedup, log: log}
}

// Handle dispatches one tenant-realm-q message.
func (c *TenantConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	processed, err := c.dedup.IsProcessed(ctx, domain.ConsumerTenant, env.ID)
	if err != nil {
		return fmt.Errorf("tenant consumer: check processed_events: %w", err)
	}
	if processed {
		return nil
	}

	if env.Type != domain.EventTrialStarted {
		c.log.Warn("tenant consumer: no handler for event type — acknowledging", map[string]any{"event_type": env.Type, "event_id": env.ID})
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

	gucCtx := withTenantGUC(ctx, p.TenantID)
	row, err := c.realms.FindByTenantID(gucCtx, p.TenantID)
	if err != nil {
		return fmt.Errorf("tenant consumer: find tenant_realms row: %w", err)
	}
	row.TrialEndsAt = &trialEndsAt
	if err := c.realms.Update(gucCtx, row); err != nil {
		return fmt.Errorf("tenant consumer: update trial_ends_at: %w", err)
	}
	if err := c.dedup.MarkProcessed(ctx, domain.ConsumerTenant, env.ID); err != nil {
		return fmt.Errorf("tenant consumer: mark processed: %w", err)
	}
	return nil
}
