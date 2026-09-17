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

// IssueCredentialResult is TS-1's response body. Secret is the plaintext,
// returned exactly once — the caller (this service) must apply it to
// Keycloak and discard it immediately (RP-17, TS-INV-2). Replayed is true
// when RotationID matched an already-committed issue/rotate rather than
// generating new material (§9.2).
type IssueCredentialResult struct {
	Version        int
	Secret         string
	OpenBaoPath    string
	ExpiresPriorAt *time.Time
	RecordVersion  int
	Replayed       bool
}

// TokenServiceClient is this service's outbound dependency on the Token
// Service's credential-lifecycle API (§2.5, RP-17/RP-18) — RP mints and
// applies, Token Service generates and custodies (RP-INV-1 preserved:
// this service never lets Token Service touch Keycloak).
type TokenServiceClient interface {
	// RegisterPrincipal calls TS-4 to register the automation principal
	// after CreateServiceAccountClient mints its Keycloak client.
	RegisterPrincipal(ctx context.Context, tenantID uuid.UUID, req RegisterPrincipalRequest) (*RegisterPrincipalResult, error)
	// IssueOrRotateCredential calls TS-1 to generate (or, on a rotation_id
	// replay, re-return) the principal's credential material.
	IssueOrRotateCredential(ctx context.Context, tenantID, principalID uuid.UUID, req IssueCredentialRequest) (*IssueCredentialResult, error)
	// ReadPrincipal calls TS-3 to read principal + credential-version
	// metadata (RP-18) — never a secret.
	ReadPrincipal(ctx context.Context, tenantID, principalID uuid.UUID) (*ReadPrincipalResult, error)
}

// CredentialSummary is one entry in ReadPrincipalResult.Credentials.
type CredentialSummary struct {
	Version     int
	Status      string
	OpenBaoPath string
	IssuedAt    time.Time
	ExpiresAt   *time.Time
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
