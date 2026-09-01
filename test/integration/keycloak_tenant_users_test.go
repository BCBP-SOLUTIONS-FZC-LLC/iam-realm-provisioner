//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// TestKeycloakAdmin_TenantScopedUserOps batches DisableUsersInRealm,
// EnableUsersInRealm, HardDeleteUsersInRealm, ForceLogoutRealm, and
// ResetMFA's credential-bearing branches against one shared-style realm
// holding two tenants' users, asserting every tenant-scoped op only ever
// touches its own tenant's users (RP-INV-2 — never the whole realm) and
// that ForceLogoutRealm tolerates users with no active session.
func TestKeycloakAdmin_TenantScopedUserOps(t *testing.T) {
	t.Parallel()
	admin, baseURL := setupKeycloakWithURL(t)
	ctx := context.Background()
	realmName := "tenant-" + uuid.NewString()[:8]
	require.NoError(t, admin.CreateRealmFromTemplate(ctx, realmName, "v1"))
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, realmName) })

	tenantA := uuid.New()
	tenantB := uuid.New()

	userA1, err := admin.CreateInvitedUser(ctx, realmName, port.CreateUserRequest{
		Email: "a1@example.com", FullName: "Tenant A One", TenantID: tenantA,
	})
	require.NoError(t, err)
	userA2, err := admin.CreateInvitedUser(ctx, realmName, port.CreateUserRequest{
		Email: "a2@example.com", FullName: "Tenant A Two", TenantID: tenantA,
	})
	require.NoError(t, err)
	userB1, err := admin.CreateInvitedUser(ctx, realmName, port.CreateUserRequest{
		Email: "b1@example.com", FullName: "Tenant B One", TenantID: tenantB,
	})
	require.NoError(t, err)

	foundA, err := admin.ListUsersByTenant(ctx, realmName, tenantA)
	require.NoError(t, err)
	require.ElementsMatch(t, []uuid.UUID{userA1, userA2}, foundA)

	require.NoError(t, admin.DisableUsersInRealm(ctx, realmName, tenantA))

	stillB, err := admin.ListUsersByTenant(ctx, realmName, tenantB)
	require.NoError(t, err)
	require.ElementsMatch(t, []uuid.UUID{userB1}, stillB, "disabling tenant A must not touch tenant B's users")

	require.NoError(t, admin.EnableUsersInRealm(ctx, realmName, tenantA))

	// A duplicate email within the same realm must fail — CreateInvitedUser
	// has no special-cased conflict handling (unlike CreateRealmFromTemplate's
	// isConflict branch), so this exercises the generic wrapErr error path.
	_, err = admin.CreateInvitedUser(ctx, realmName, port.CreateUserRequest{
		Email: "a1@example.com", FullName: "Duplicate Email", TenantID: tenantA,
	})
	require.Error(t, err)

	// hasRealmRole's isNotFound branch: a nonexistent user has no roles,
	// not an error.
	has, err := admin.HasRequiresMFA(ctx, realmName, uuid.New())
	require.NoError(t, err)
	require.False(t, has)

	require.NoError(t, admin.ForceLogoutRealm(ctx, realmName), "no active sessions in this realm — must be a no-op success")

	// ResetMFA's credential-bearing branches (RP-9): a user actually holding
	// OTP + WebAuthn credentials must have both deleted, and CONFIGURE_TOTP
	// re-added only when requiresMFA is true.
	raw, tok := rawAdminClient(t, baseURL)
	mfaUserID, err := raw.CreateUser(ctx, tok, realmName, gocloak.User{
		Username: gocloak.StringP("mfa-user@example.com"), Email: gocloak.StringP("mfa-user@example.com"),
		Enabled:     gocloak.BoolP(true),
		Credentials: &[]gocloak.CredentialRepresentation{otpCredential(), webauthnCredential()},
	})
	require.NoError(t, err)
	mfaUUID := uuid.MustParse(mfaUserID)

	require.NoError(t, admin.ResetMFA(ctx, realmName, mfaUUID, true))
	creds, err := raw.GetCredentials(ctx, tok, realmName, mfaUserID)
	require.NoError(t, err)
	require.Empty(t, creds, "ResetMFA must delete every OTP/WebAuthn credential")
	resetUser, err := raw.GetUserByID(ctx, tok, realmName, mfaUserID)
	require.NoError(t, err)
	require.NotNil(t, resetUser.RequiredActions)
	require.Contains(t, *resetUser.RequiredActions, "CONFIGURE_TOTP", "requiresMFA=true must re-add CONFIGURE_TOTP")

	mfaUserID2, err := raw.CreateUser(ctx, tok, realmName, gocloak.User{
		Username: gocloak.StringP("mfa-user2@example.com"), Email: gocloak.StringP("mfa-user2@example.com"),
		Enabled:     gocloak.BoolP(true),
		Credentials: &[]gocloak.CredentialRepresentation{otpCredential()},
	})
	require.NoError(t, err)
	mfaUUID2 := uuid.MustParse(mfaUserID2)
	require.NoError(t, admin.ResetMFA(ctx, realmName, mfaUUID2, false))
	resetUser2, err := raw.GetUserByID(ctx, tok, realmName, mfaUserID2)
	require.NoError(t, err)
	if resetUser2.RequiredActions != nil {
		require.NotContains(t, *resetUser2.RequiredActions, "CONFIGURE_TOTP", "requiresMFA=false must not re-add CONFIGURE_TOTP")
	}

	require.NoError(t, admin.HardDeleteUsersInRealm(ctx, realmName, tenantA))

	goneA, err := admin.ListUsersByTenant(ctx, realmName, tenantA)
	require.NoError(t, err)
	require.Empty(t, goneA)

	stillB2, err := admin.ListUsersByTenant(ctx, realmName, tenantB)
	require.NoError(t, err)
	require.ElementsMatch(t, []uuid.UUID{userB1}, stillB2, "hard-deleting tenant A must not touch tenant B's users")
}
