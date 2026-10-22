package port

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RegisterPrincipalRequest is TS-4's request body (§2.5, RP-17) — sent
// after CreateServiceAccountClient mints the Keycloak client.
type RegisterPrincipalRequest struct {
	PrincipalSub     uuid.UUID
	KeycloakClientID string
}

// RegisterPrincipalResult is TS-4's response body. Created is false on an
// idempotent repeat (200) or a carry-over update (RP-3 conversion,
// re-registering a differing principal_sub against the same principal).
type RegisterPrincipalResult struct {
	PrincipalID   uuid.UUID
	Created       bool
	RecordVersion int
}

// IssueCredentialRequest is TS-1's request body — a caller-generated
// rotation_id makes the call idempotent per attempt (§9.2 of the Token
// Service LLD).
type IssueCredentialRequest struct {
	RotationID     uuid.UUID
	OverlapSeconds *int
}

// IssueCredentialResult is TS-1's response body. Secret is the plaintext
// private key, returned exactly once — this service discards it immediately
// without ever touching Keycloak with it (EXT-6, corrected from the earlier
// apply-a-client-secret design: Keycloak already trusts the JWKS Token
// Service serves at JWKSURLFor, and fetches the public half itself).
// Replayed is true when RotationID matched an already-committed
// issue/rotate rather than generating new material (§9.2).
type IssueCredentialResult struct {
	Version        int
	Secret         string
	OpenBaoPath    string
	ExpiresPriorAt *time.Time
	RecordVersion  int
	Replayed       bool
}

// TokenServiceClient is this service's outbound dependency on the Token
// Service's credential-lifecycle API (§2.5, RP-17/RP-18) — RP mints the
// Keycloak client and clears its JWKS key cache; Token Service generates,
// custodies, and directly serves the credential material's public half as
// a JWKS Keycloak fetches itself (EXT-6, RP-INV-1 preserved: this service
// never lets Token Service touch Keycloak).
type TokenServiceClient interface {
	// RegisterPrincipal calls TS-4 to register the automation principal
	// after CreateServiceAccountClient mints its Keycloak client.
	RegisterPrincipal(ctx context.Context, tenantID uuid.UUID, req RegisterPrincipalRequest) (*RegisterPrincipalResult, error)
	// IssueOrRotateCredential calls TS-1 to generate (or, on a rotation_id
	// replay, re-return) the principal's credential material.
	IssueOrRotateCredential(ctx context.Context, tenantID, principalID uuid.UUID, req IssueCredentialRequest) (*IssueCredentialResult, error)
	// RevokeCredential calls TS-2 to revoke the active credential(s) for
	// principalID. Called during RP-3 conversion after TS-1 has issued the
	// new dedicated-realm credential, to retire the old shared-realm
	// credential so it can no longer authenticate. Must run after TS-1 and
	// before the old shared-realm Keycloak client is deleted.
	RevokeCredential(ctx context.Context, tenantID, principalID uuid.UUID) error
	// RevokePrincipal calls TS-7 to revoke the tenant's platform-automation
	// principal: Token Service revokes every credential version, deletes its
	// OpenBao material, unschedules rotation and sets the principal
	// `revoked`. Used by orphaned-signup-reap, whose tenant never reaches
	// O&M and so never gets the TenantMembershipsPurged that otherwise
	// drives Token Service's own cleanup (§8.7). Idempotent: an
	// already-revoked principal is success, and so is a 404 whose problem
	// errorCode is PRINCIPAL_NOT_FOUND. Any other 404 (a Token Service
	// without TS-7) is an error, never mistaken for "already gone".
	RevokePrincipal(ctx context.Context, tenantID uuid.UUID) error
	// ReadPrincipal calls TS-3 to read principal + credential-version
	// metadata (RP-18) — never a secret.
	ReadPrincipal(ctx context.Context, tenantID, principalID uuid.UUID) (*ReadPrincipalResult, error)
	// JWKSURLFor returns the JWKS URL Token Service serves tenantID's
	// platform-automation public keys at (EXT-6, §2.5) — pure string
	// construction from this client's own configured base URL, no network
	// call. Keyed by tenant_id alone (valid: at most one platform_automation
	// principal per tenant, uq_sap_active_principal), so it's available
	// before CreateServiceAccountClient has minted a principal_id or
	// keycloak_client_id to key it by instead.
	JWKSURLFor(tenantID uuid.UUID) string
}

// CredentialSummary is one entry in ReadPrincipalResult.Credentials.
// RotationCadenceDays and NextRotationAt are Token Service TS-D14 additions
// — both optional, both omitted from older TS responses (nil).
type CredentialSummary struct {
	Version             int
	Status              string
	OpenBaoPath         string
	IssuedAt            time.Time
	ExpiresAt           *time.Time
	RotationCadenceDays *int
	NextRotationAt      *time.Time
}

// ReadPrincipalResult is TS-3's response body (RP-18).
type ReadPrincipalResult struct {
	PrincipalID      uuid.UUID
	TenantID         uuid.UUID
	KeycloakClientID string
	PrincipalType    string
	Status           string
	RecordVersion    int
	Credentials      []CredentialSummary
}
