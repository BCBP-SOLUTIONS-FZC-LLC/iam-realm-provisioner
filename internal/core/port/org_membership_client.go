package port

import (
	"context"

	"github.com/google/uuid"
)

// OrgMembershipClient serves the single outbound dependency this service
// has on Org & Membership: the I-8 residual-MFA-requirement check
// (GET /api/v1/internal/users/:id/memberships), consulted only on the
// TenantRoleRevoked drop path (§8.6, RP-1).
type OrgMembershipClient interface {
	// HasResidualMFARequirement reports whether userID still holds any
	// MFA-requiring role or department level (tenant_owner/tenant_admin,
	// or a reviewer/approver department mapping) after a TenantRoleRevoked
	// event — used to decide whether requires-mfa may be safely revoked.
	HasResidualMFARequirement(ctx context.Context, tenantID, userID uuid.UUID) (bool, error)
}
