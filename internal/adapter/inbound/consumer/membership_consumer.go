package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// mfaSyncer is the narrow service-layer dependency this consumer needs —
// kept as an interface so this package doesn't import internal/core/service.
type mfaSyncer interface {
	SyncRequiresMfa(ctx context.Context, tenantID, userID uuid.UUID, grant bool) error
}

// MembershipConsumer handles membership-realm-q (§7.1, §8.6, RP-EVT-6):
// DepartmentMembershipGranted, DepartmentMembershipLevelChanged,
// TenantRoleGranted, TenantRoleRevoked -> MfaService.SyncRequiresMfa.
//
// mfaRequiringLevels/mfaRequiringRoles mirror O&M's vocabulary (§6.5/§8.6).
type MembershipConsumer struct {
	mfa   mfaSyncer
	dedup port.ProcessedEventsStore
	log   port.Logger
}

// NewMembershipConsumer constructs a MembershipConsumer for
// membership-realm-q.
func NewMembershipConsumer(mfa mfaSyncer, dedup port.ProcessedEventsStore, log port.Logger) *MembershipConsumer {
	return &MembershipConsumer{mfa: mfa, dedup: dedup, log: log}
}

var mfaRequiringLevels = map[string]bool{"reviewer": true, "approver": true}

// Handle implements events.Handler.
func (c *MembershipConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	processed, err := c.dedup.IsProcessed(ctx, domain.ConsumerMembership, env.ID)
	if err != nil {
		return fmt.Errorf("membership consumer: check processed_events: %w", err)
	}
	if processed {
		return nil
	}

	var handleErr error
	switch env.Type {
	case domain.EventDepartmentMembershipGranted:
		var p domain.DepartmentMembershipGrantedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return fmt.Errorf("membership consumer: decode %s: %w", env.Type, err)
		}
		if mfaRequiringLevels[p.Level] {
			handleErr = c.mfa.SyncRequiresMfa(withTenantGUC(ctx, p.TenantID), p.TenantID, p.UserID, true)
		}
	case domain.EventDepartmentMembershipLevelChanged:
		var p domain.DepartmentMembershipLevelChangedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return fmt.Errorf("membership consumer: decode %s: %w", env.Type, err)
		}
		if mfaRequiringLevels[p.NewLevel] {
			handleErr = c.mfa.SyncRequiresMfa(withTenantGUC(ctx, p.TenantID), p.TenantID, p.UserID, true)
		}
		// A de-elevation (approver -> preparator) is NOT a drop trigger
		// (§7.1: "the only drop trigger is TenantRoleRevoked") — no else
		// branch here by design.
	case domain.EventTenantRoleGranted:
		var p domain.TenantRoleGrantedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return fmt.Errorf("membership consumer: decode %s: %w", env.Type, err)
		}
		handleErr = c.mfa.SyncRequiresMfa(withTenantGUC(ctx, p.TenantID), p.TenantID, p.UserID, true)
	case domain.EventTenantRoleRevoked:
		var p domain.TenantRoleRevokedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return fmt.Errorf("membership consumer: decode %s: %w", env.Type, err)
		}
		handleErr = c.mfa.SyncRequiresMfa(withTenantGUC(ctx, p.TenantID), p.TenantID, p.UserID, false)
	default:
		c.log.Warn("membership consumer: no handler for event type — acknowledging", map[string]any{"event_type": env.Type, "event_id": env.ID})
		return nil
	}
	if handleErr != nil {
		return fmt.Errorf("membership consumer: %s: %w", env.Type, handleErr)
	}
	if err := c.dedup.MarkProcessed(ctx, domain.ConsumerMembership, env.ID); err != nil {
		return fmt.Errorf("membership consumer: mark processed: %w", err)
	}
	return nil
}
