package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// MfaService implements RP-9 (§8.6, HLD §8.2.6) and the membership-realm-q
// consumer's requires-mfa sync (SyncRequiresMfa, §8.6/RP-EVT-6).
type MfaService struct {
	realms port.TenantRealmRepository
	kc     port.KeycloakAdmin
	orgMem port.OrgMembershipClient
	log    port.SlogStyleLogger
}

// NewMfaService wires MfaService's dependencies.
func NewMfaService(realms port.TenantRealmRepository, kc port.KeycloakAdmin, orgMem port.OrgMembershipClient) *MfaService {
	return &MfaService{realms: realms, kc: kc, orgMem: orgMem}
}

// WithLogger attaches a logger and returns s for chaining.
func (s *MfaService) WithLogger(log port.Logger) *MfaService {
	s.log = port.NewSlogStyleLogger(log)
	return s
}

// ResetMFA implements RP-9: resolves the realm from tenant_realms, removes
// every OTP/WebAuthn credential, and re-adds CONFIGURE_TOTP if the user
// still holds requires-mfa. Idempotent (absent-credential reset -> success,
// no error). Fail-closed — persists nothing locally, no reconciler.
func (s *MfaService) ResetMFA(ctx context.Context, tenantID, keycloakUserID uuid.UUID) error {
	row, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		metrics.MfaResetTotal.WithLabelValues("fail").Inc()
		return err
	}
	requiresMFA, err := s.kc.HasRequiresMFA(ctx, row.RealmName, keycloakUserID)
	if err != nil {
		metrics.MfaResetTotal.WithLabelValues("fail").Inc()
		return err
	}
	if err := s.kc.ResetMFA(ctx, row.RealmName, keycloakUserID, requiresMFA); err != nil {
		metrics.MfaResetTotal.WithLabelValues("fail").Inc()
		return err
	}
	metrics.MfaResetTotal.WithLabelValues("success").Inc()
	return nil
}

// SyncRequiresMfa implements the membership-realm-q consumer's core logic
// (§8.6, RP-EVT-6): a *reconcile*, not an apply-a-delta, so
// redelivery/out-of-order convergence is automatic.
//
//   - grant: assign-if-absent, unconditional (DepartmentMembershipGranted at
//     reviewer/approver, DepartmentMembershipLevelChanged elevating to
//     those levels, TenantRoleGranted).
//   - revoke: revoke-if-present, ONLY on TenantRoleRevoked, and only after
//     confirming (via O&M's I-8) the user holds no other MFA-requiring
//     role/level.
func (s *MfaService) SyncRequiresMfa(ctx context.Context, tenantID, userID uuid.UUID, grant bool) error {
	row, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		return err
	}
	if grant {
		if err := s.kc.AssignRequiresMFA(ctx, row.RealmName, userID); err != nil {
			return err
		}
		metrics.MfaRoleSyncTotal.WithLabelValues("assign").Inc()
		return nil
	}

	// Drop path: consult O&M's I-8 residual-MFA-requirement check before
	// revoking — RP holds no membership state of its own (§8.6).
	residual, err := s.orgMem.HasResidualMFARequirement(ctx, tenantID, userID)
	if err != nil {
		return err
	}
	if residual {
		s.log.DebugContext(ctx, "sync_requires_mfa: residual MFA requirement found — not revoking", "tenant_id", tenantID, "user_id", userID)
		return nil
	}
	if err := s.kc.RevokeRequiresMFA(ctx, row.RealmName, userID); err != nil {
		return err
	}
	metrics.MfaRoleSyncTotal.WithLabelValues("revoke").Inc()
	return nil
}
