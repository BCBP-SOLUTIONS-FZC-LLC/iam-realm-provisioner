package port

import (
	"context"

	"github.com/google/uuid"
)

// OpenBaoClient is the outbound seam onto OpenBao's KV v2 API (§10.2,
// RP-D10). Per-tenant IdP secrets live at the deterministic path
// `secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>` — no
// reference to this path is ever persisted in Postgres; it is always
// recomputed from (tenant_id, idp_alias).
type OpenBaoClient interface {
	// WriteIdpSecret writes secret (a JSON-serializable bundle of whatever
	// the protocol needs — OIDC brokered-client secret, SAML SP signing
	// key) at the tenant/alias path (RP-6).
	WriteIdpSecret(ctx context.Context, tenantID uuid.UUID, idpAlias string, secret map[string]any) error
	// ReadIdpSecret reads the secret back for re-applying an IdP to
	// Keycloak (realm rebuild, DR restore).
	ReadIdpSecret(ctx context.Context, tenantID uuid.UUID, idpAlias string) (map[string]any, error)
	// DeleteIdpSecret removes the KV entry on IdP removal (RP-7).
	DeleteIdpSecret(ctx context.Context, tenantID uuid.UUID, idpAlias string) error
}
