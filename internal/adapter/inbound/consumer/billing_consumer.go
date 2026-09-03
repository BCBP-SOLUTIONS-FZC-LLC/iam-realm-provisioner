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
	tx       port.TxRunner
	log      port.Logger
}

// NewBillingConsumer constructs a BillingConsumer for billing-realm-q.
func NewBillingConsumer(realms port.TenantRealmRepository, keycloak port.KeycloakAdmin, dedup port.ProcessedEventsStore, tx port.TxRunner, log port.Logger) *BillingConsumer {
	return &BillingConsumer{realms: realms, keycloak: keycloak, dedup: dedup, tx: tx, log: log}
}

// Handle dispatches one billing-realm-q message (RP-EVT-7).
func (c *BillingConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	if env.Type != domain.EventTenantReactivated {
		if err := ackUnknown(ctx, c.tx, c.dedup, c.log, domain.ConsumerBilling, env); err != nil {
			return fmt.Errorf("billing consumer: %w", err)
		}
		return nil
	}

	processed, err := skipDuplicate(ctx, c.dedup, domain.ConsumerBilling, env.ID)
	if err != nil {
		return fmt.Errorf("billing consumer: check processed_events: %w", err)
	}
	if processed {
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

	if row.Status != domain.RealmStatusSuspended {
		// Never reached suspended (only cancelled/read-only) -> no Keycloak //nolint:misspell // "cancelled" mirrors O&M's actual enum spelling (frozen contract term)
		// effect and no local write. MarkProcessed still goes through
		// RunInTx so it joins withPool the same way iam-org-membership's
		// no-op / unknown-type path does.
		if err := markProcessedInTx(gucCtx, c.tx, c.dedup, domain.ConsumerBilling, env.ID); err != nil {
			return fmt.Errorf("billing consumer: %w", err)
		}
		return nil
	}

	// Keycloak-effect-first, then the local commit (§9.1 / CONS-2): the
	// realm is re-enabled outside any transaction, so a committed row always
	// reflects real Keycloak state. A crash between the two replays this
	// handler and EnableRealm reconciles to a no-op.
	if err := c.keycloak.EnableRealm(ctx, row.RealmName); err != nil {
		return fmt.Errorf("billing consumer: enable realm: %w", err)
	}
	return c.tx.RunInTx(gucCtx, func(txCtx context.Context) error {
		row.Status = domain.RealmStatusActive
		row.DisabledAt = nil
		row.SuspensionSource = nil
		if err := c.realms.Update(txCtx, row); err != nil {
			return fmt.Errorf("billing consumer: update tenant_realms: %w", err)
		}
		if err := c.dedup.MarkProcessed(txCtx, domain.ConsumerBilling, env.ID); err != nil {
			return fmt.Errorf("billing consumer: mark processed: %w", err)
		}
		return nil
	})
}
