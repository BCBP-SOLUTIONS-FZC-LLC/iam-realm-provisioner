package port

import (
	"context"

	"github.com/google/uuid"
)

// KeycloakAdmin is the sole seam onto Keycloak's Admin API (RP-INV-1) — no
// other package in this service, and no other service in the platform, may
// hold a Keycloak admin credential or call gocloak directly. Every method
// wraps gocloak, the admin-token cache, retry/backoff, and the client-side
// rate limiter (§9.4).
type KeycloakAdmin interface {
	// Ready verifies the admin credential can mint a token (used by
	// /readyz, §3.1).
	Ready(ctx context.Context) error

	// ── Realm lifecycle (§8.1-8.4, §8.7-8.9, §8.11-8.12) ─────────────────

	// CreateRealmFromTemplate clones the versioned realm template
	// (deploy/keycloak/realm-template.json, RP-3) into a new realm named
	// realmName on shard.
	CreateRealmFromTemplate(ctx context.Context, realmName, templateVersion string) error
	// RegisterOIDCClients registers the standard OIDC client set on realmName
	// from the template, returning the registered client IDs.
	RegisterOIDCClients(ctx context.Context, realmName, tenantSlug string) ([]string, error)
	DisableRealm(ctx context.Context, realmName string) error
	EnableRealm(ctx context.Context, realmName string) error
	// ExportRealm returns a full realm export (users, IdPs, clients,
	// credentials) for offboarding (RP-EVT-4) — export MUST succeed and be
	// durably stored before DeleteRealm is ever called.
	ExportRealm(ctx context.Context, realmName string) ([]byte, error)
	DeleteRealm(ctx context.Context, realmName string) error
	// DeleteRealmIfExists is the idempotent compensating-teardown primitive
	// for a partially-created dedicated realm (§9.3).
	DeleteRealmIfExists(ctx context.Context, realmName string) error

	// ── User lifecycle (§8.1-8.3, §8.6-8.7, §8.13) ───────────────────────

	// CreateTrialUser creates a user in the shared trial realm with the
	// tenant_id attribute stamped (RP-INV-2), email unverified, and
	// triggers the VERIFY_EMAIL required action. Returns the Keycloak
	// user ID (== the future JWT sub).
	CreateTrialUser(ctx context.Context, req CreateUserRequest) (uuid.UUID, error)
	// CreateInvitedUser creates a user in realmName with requiredActions
	// applied verbatim (RP-5, F5 — this service never re-derives them).
	CreateInvitedUser(ctx context.Context, realmName string, req CreateUserRequest) (uuid.UUID, error)
	// FindUserByEmail looks up a user by exact email match within
	// realmName — used by callers of CreateTrialUser/CreateInvitedUser to
	// disambiguate a lost-response retry of their OWN request (the
	// original create succeeded but its response never reached the
	// caller) from a genuine email conflict on the retry's 409 (§9.2).
	// Returns a distinguishable ErrKeycloakUserNotFound, never a bare nil
	// result, when no user with that email exists in realmName.
	FindUserByEmail(ctx context.Context, realmName, email string) (*ExistingUser, error)
	// MigrateUser recreates a trial user in a dedicated realm, preserving
	// sub, copying the TOTP credential, and flagging WEBAUTHN_REGISTER when
	// the source user held a WebAuthn credential (WebAuthn cannot migrate
	// across realms, §8.3).
	MigrateUser(ctx context.Context, sourceRealm, targetRealm string, keycloakUserID uuid.UUID) (webauthnReenrollRequired bool, err error)
	// ListUsersByTenant returns the Keycloak user IDs of every user in
	// realmName carrying the given tenant_id attribute (§8.3 conversion —
	// enumerates the trial users to migrate).
	ListUsersByTenant(ctx context.Context, realmName string, tenantID uuid.UUID) ([]uuid.UUID, error)
	// DisableUsersInRealm disables every user carrying the given tenant_id
	// attribute within realmName — used for shared-trial suspend/expiry
	// (never disables the realm itself, RP-INV-2/§8.12).
	DisableUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) error
	EnableUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) error
	// HardDeleteUsersInRealm hard-deletes every user carrying tenant_id
	// (RP-C2 PII scrub, §15.2).
	HardDeleteUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) error
	// DeleteUser is idempotent — deleting an absent user succeeds (RP-15).
	DeleteUser(ctx context.Context, realmName string, keycloakUserID uuid.UUID) error
	// RevokeUserSessions logs out and invalidates every active session and
	// token for keycloakUserID (RP-16). Idempotent.
	RevokeUserSessions(ctx context.Context, realmName string, keycloakUserID uuid.UUID) error
	// ForceLogoutRealm terminates every active session in realmName (used
	// at conversion to force-logout shared-realm sessions, §8.3).
	ForceLogoutRealm(ctx context.Context, realmName string) error

	// ── MFA (§8.6) ────────────────────────────────────────────────────────

	// AssignRequiresMFA assigns the requires-mfa realm role to userID,
	// idempotent (assign-if-absent).
	AssignRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) error
	// RevokeRequiresMFA revokes the requires-mfa realm role, idempotent
	// (revoke-if-present).
	RevokeRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) error
	// HasRequiresMFA reports whether userID currently holds the role.
	HasRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) (bool, error)
	// ResetMFA deletes every OTP/WebAuthn credential for userID and, if
	// requiresMFA, re-adds the CONFIGURE_TOTP required action (RP-9).
	// Idempotent — no enrolled credentials is a no-op success.
	ResetMFA(ctx context.Context, realmName string, userID uuid.UUID, requiresMFA bool) error

	// ── IdP / federation (§8.5, RP-INV-4) ────────────────────────────────

	CreateOrUpdateIdp(ctx context.Context, realmName string, idp IdpKeycloakConfig) error
	DeleteIdp(ctx context.Context, realmName, alias string) error

	// ── Realm config (§8.10, RP-INV-6) ───────────────────────────────────

	ApplyRealmConfig(ctx context.Context, realmName, settingKey string, desiredValue any) error

	// ── Service-account clients (§2.5, RP-17) ────────────────────────────

	// CreateServiceAccountClient idempotently mints a confidential,
	// service-account-enabled client (clientID) in realmName, returning
	// the Keycloak service-account user's sub (the RP-supplied
	// `principal_sub` for TS-4). Resume-if-exists, mirroring
	// CreateRealmFromTemplate's idempotent-create pattern — a repeat call
	// against an already-minted client returns its existing sub rather
	// than erroring.
	CreateServiceAccountClient(ctx context.Context, realmName, clientID string) (sub uuid.UUID, err error)
	// SetClientSecret sets clientID's secret in realmName to secret — a
	// caller-supplied value (the plaintext returned once by Token
	// Service's TS-1), not a Keycloak-generated one. RP-17's apply step;
	// idempotent — setting the same value twice is a no-op.
	SetClientSecret(ctx context.Context, realmName, clientID, secret string) error
}

// CreateUserRequest is the Keycloak-facing shape for creating a trial or
// invited user (§8.1, §8.2.2).
type CreateUserRequest struct {
	Email              string
	FullName           string
	TenantID           uuid.UUID
	HashedPasswordSeed string   // optional — trial/direct-paid admin only
	RequiredActions    []string // applied verbatim (F5)
	EmailVerified      bool
}

// ExistingUser is FindUserByEmail's result — the minimal shape a caller
// needs to decide whether a create-user 409 is its own lost-response retry
// (TenantID matches the retried request) or a genuine conflict (§9.2).
// TenantID is the Go zero-value (uuid.Nil) if the Keycloak user carries no
// tenant_id attribute at all (a legacy/manually-created user) — that never
// matches a real request's TenantID, so it's correctly treated as a
// genuine conflict rather than a false idempotent-success.
type ExistingUser struct {
	ID       uuid.UUID
	TenantID uuid.UUID
}

// IdpKeycloakConfig is the Keycloak-facing shape for creating/updating an
// IdP (§8.5). Secret is passed through transiently — never persisted here.
type IdpKeycloakConfig struct {
	Alias       string
	Protocol    string // saml|oidc
	DisplayName string
	Enabled     bool
	Config      map[string]any // non-secret protocol params
	Secret      string         // brokered-client secret / SP signing key material
}
