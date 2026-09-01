package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// BillingConsumer handles billing-realm-q (§7.1, §8.11, RP-EVT-7):
// Billing's own TenantReactivated (source=billing) -> re-enable the
// dedicated realm only if it had reached `suspended`; otherwise a no-op
// (still recorded in processed_events). This is a DIFFERENT message from
// this service's own produced TenantReactivated (source=operator) — they
// ride different topics, so there is no self-consumption (RP-D13).
type BillingConsumer struct {
	realms   port.TenantRealmRepository
	keycloak port.KeycloakAdmin
	dedup    port.ProcessedEventsStore
	log      port.Logger
}

// NewBillingConsumer constructs a BillingConsumer for billing-realm-q.
func NewBillingConsumer(realms port.TenantRealmRepository, keycloak port.KeycloakAdmin, dedup port.ProcessedEventsStore, log port.Logger) *BillingConsumer {
	return &BillingConsumer{realms: realms, keycloak: keycloak, dedup: dedup, log: log}
}

// Handle dispatches one billing-realm-q message (RP-EVT-7).
func (c *BillingConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	processed, err := c.dedup.IsProcessed(ctx, domain.ConsumerBilling, env.ID)
	if err != nil {
		return fmt.Errorf("billing consumer: check processed_events: %w", err)
	}
	if processed {
		return nil
	}

	if env.Type != domain.EventTenantReactivated {
		c.log.Warn("billing consumer: no handler for event type — acknowledging", map[string]any{"event_type": env.Type, "event_id": env.ID})
		return nil
	}
	var p domain.BillingTenantReactivatedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return fmt.Errorf("billing consumer: decode %s: %w", env.Type, err)
	}

	gucCtx := withTenantGUC(ctx, p.TenantID)
	row, err := c.realms.FindByTenantID(gucCtx, p.TenantID)
	if err != nil {
		return fmt.Errorf("billing consumer: find tenant_realms row: %w", err)
	}
	if row.Status == domain.RealmStatusSuspended {
		if err := c.keycloak.EnableRealm(ctx, row.RealmName); err != nil {
			return fmt.Errorf("billing consumer: enable realm: %w", err)
		}
		row.Status = domain.RealmStatusActive
		row.DisabledAt = nil
		if err := c.realms.Update(gucCtx, row); err != nil {
			return fmt.Errorf("billing consumer: update tenant_realms: %w", err)
		}
	}
	// Never reached suspended (only cancelled/read-only) -> no Keycloak //nolint:misspell // "cancelled" mirrors O&M's actual enum spelling (frozen contract term)
	// effect; still record the processed event (§7.1).
	if err := c.dedup.MarkProcessed(ctx, domain.ConsumerBilling, env.ID); err != nil {
		return fmt.Errorf("billing consumer: mark processed: %w", err)
	}
	return nil
}
