// Package consumer implements the three inbound SQS consumers (§7.1):
// membership-realm-q, tenant-realm-q, and billing-realm-q.
package consumer

import (
	"context"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// systemPrincipal is the GUC user-id stamped on every consumer-driven
// write — these are system/event-driven mutations, not human-attributed
// ones (§18.3: the reserved iam-system principal).
const systemPrincipal = "iam-system"

// withTenantGUC binds app.tenant_id (and app.user_id=iam-system) into ctx
// via pgcommon's GUCSet so the next pool checkout issues
// `SET LOCAL app.tenant_id = ...` (RLS-6). Every consumer handler MUST
// call this before any repository write.
func withTenantGUC(ctx context.Context, tenantID uuid.UUID) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID.String()
	g.UserID = systemPrincipal
	return pgcommon.WithGUCSet(ctx, g)
}
