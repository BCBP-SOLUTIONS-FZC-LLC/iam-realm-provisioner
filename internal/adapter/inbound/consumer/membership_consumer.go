package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
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
//
// Unlike TenantConsumer and BillingConsumer, this handler's only side effect
// is a Keycloak realm-role mutation — it writes nothing to Postgres. There
// is therefore no local write for the processed_events claim to be atomic
// with: the inbox claim commits on its own after the Keycloak call (never
// around it — §9.1 keeps Keycloak calls outside database transactions). A
// crash between the Keycloak call and the claim replays the handler, and
// SyncRequiresMfa is written as a reconcile (assign-if-absent /
// revoke-if-present, RP-EVT-6) precisely so replay is a no-op.
type MembershipConsumer struct {
	mfa   mfaSyncer
	inbox port.Inbox
	log   port.Logger
}

// NewMembershipConsumer constructs a MembershipConsumer for
// membership-realm-q.
func NewMembershipConsumer(mfa mfaSyncer, inbox port.Inbox, log port.Logger) *MembershipConsumer {
	return &MembershipConsumer{mfa: mfa, inbox: inbox, log: log}
}

var mfaRequiringLevels = map[string]bool{"reviewer": true, "approver": true}

// Handle implements events.Handler.
func (c *MembershipConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	switch env.Type {
	case domain.EventDepartmentMembershipGranted,
		domain.EventDepartmentMembershipLevelChanged,
		domain.EventTenantRoleGranted,
		domain.EventTenantRoleRevoked:
	default:
		if err := ackUnknown(ctx, c.inbox, c.log, domain.ConsumerMembership, env); err != nil {
			return fmt.Errorf("membership consumer: %w", err)
		}
		return nil
	}

	processed, err := skipDuplicate(ctx, c.inbox, domain.ConsumerMembership, env.ID)
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
	}
	if handleErr != nil {
		return fmt.Errorf("membership consumer: %s: %w", env.Type, handleErr)
	}
	if err := processOnce(ctx, c.inbox, domain.ConsumerMembership, env, noLocalWrite); err != nil {
		return fmt.Errorf("membership consumer: %w", err)
	}
	return nil
}
