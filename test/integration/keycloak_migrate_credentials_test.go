//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestKeycloakAdmin_MigrateUser_CredentialBranches exercises MigrateUser's
// three credential-shape branches (§8.3, RP-3) in one container: TOTP-only
// (copied, no re-enroll), WebAuthn-only (not copied — cannot migrate across
// realms — but flags webauthn-register and preserves prior required
// actions), and both together.
func TestKeycloakAdmin_MigrateUser_CredentialBranches(t *testing.T) {
	t.Parallel()
	admin, baseURL := setupKeycloakWithURL(t)
	ctx := context.Background()

	sourceRealm := "tenant-src-" + uuid.NewString()[:8]
	targetRealm := "tenant-dst-" + uuid.NewString()[:8]
	require.NoError(t, admin.CreateRealmFromTemplate(ctx, sourceRealm, "v1"))
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, sourceRealm) })
	require.NoError(t, admin.CreateRealmFromTemplate(ctx, targetRealm, "v1"))
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, targetRealm) })

	// The bootstrap service account is only granted admin rights on a realm
	// AFTER that realm's CreateRealm call returns (admin.go's own
	// enableUnmanagedAttributes/CreateRealmRole follow-ups rely on the same
	// re-mint) — a raw client token must therefore be minted here, after
	// both realms above already exist, or every raw call below 403s.
	raw, tok := rawAdminClient(t, baseURL)

	// KNOWN PRODUCTION BUG (found empirically here, not a test artifact):
	// Keycloak's GET /users/{id}/credentials never returns secretData (the
	// actual TOTP shared secret) — only the safe credentialData metadata —
	// confirmed both via this real server and directly with curl against a
	// throwaway container. MigrateUser (admin_users.go) copies whatever
	// GetCredentials returns straight into the partialImport payload, so the
	// copied "otp" credential is missing its secret and Keycloak rejects the
	// import. RP-3 conversion of any user with an enrolled TOTP credential
	// therefore fails today; see admin_users.go's MigrateUser/GetCredentials
	// call. This subtest pins the actual (broken) behavior rather than the
	// documented intent so a fix shows up as a test change, not a silent gap.
	t.Run("totp_only_fails_secretData_not_copied", func(t *testing.T) {
		email := "totp-" + uuid.NewString()[:8] + "@example.com"
		id, err := raw.CreateUser(ctx, tok, sourceRealm, gocloak.User{
			Username: &email, Email: &email, Enabled: gocloak.BoolP(true),
			Credentials: &[]gocloak.CredentialRepresentation{otpCredential()},
		})
		require.NoError(t, err)
		userID := uuid.MustParse(id)

		_, err = admin.MigrateUser(ctx, sourceRealm, targetRealm, userID)
		require.Error(t, err, "partialImport rejects an otp credential with no secretData (§8.3 TOTP-copy bug)")
	})

	t.Run("webauthn_only", func(t *testing.T) {
		email := "webauthn-" + uuid.NewString()[:8] + "@example.com"
		id, err := raw.CreateUser(ctx, tok, sourceRealm, gocloak.User{
			Username: &email, Email: &email, Enabled: gocloak.BoolP(true),
			RequiredActions: &[]string{"VERIFY_EMAIL"},
			Credentials:     &[]gocloak.CredentialRepresentation{webauthnCredential()},
		})
		require.NoError(t, err)
		userID := uuid.MustParse(id)

		reenroll, err := admin.MigrateUser(ctx, sourceRealm, targetRealm, userID)
		require.NoError(t, err)
		require.True(t, reenroll, "a source WebAuthn credential must flag re-enrollment")

		creds, err := raw.GetCredentials(ctx, tok, targetRealm, id)
		require.NoError(t, err)
		require.Empty(t, creds, "WebAuthn credentials cannot migrate across realms and must NOT be copied")

		target, err := raw.GetUserByID(ctx, tok, targetRealm, id)
		require.NoError(t, err)
		require.NotNil(t, target.RequiredActions)
		require.ElementsMatch(t, []string{"VERIFY_EMAIL", "webauthn-register"}, *target.RequiredActions,
			"webauthn-register must be appended, preserving the user's prior required actions")
	})

	// Same root cause as totp_only_fails_secretData_not_copied above — the
	// otp branch fails at partialImport before the webauthn required-action
	// bookkeeping (which happens earlier in MigrateUser, before the delete
	// +import) has any bearing on the outcome.
	t.Run("totp_and_webauthn_fails_secretData_not_copied", func(t *testing.T) {
		email := "both-" + uuid.NewString()[:8] + "@example.com"
		id, err := raw.CreateUser(ctx, tok, sourceRealm, gocloak.User{
			Username: &email, Email: &email, Enabled: gocloak.BoolP(true),
			Credentials: &[]gocloak.CredentialRepresentation{otpCredential(), webauthnCredential()},
		})
		require.NoError(t, err)
		userID := uuid.MustParse(id)

		_, err = admin.MigrateUser(ctx, sourceRealm, targetRealm, userID)
		require.Error(t, err)
	})

	t.Run("source_user_absent", func(t *testing.T) {
		_, err := admin.MigrateUser(ctx, sourceRealm, targetRealm, uuid.New())
		require.Error(t, err, "migrating a nonexistent source user must fail at GetUserByID")
	})

	t.Run("target_realm_absent", func(t *testing.T) {
		email := "orphan-" + uuid.NewString()[:8] + "@example.com"
		id, err := raw.CreateUser(ctx, tok, sourceRealm, gocloak.User{
			Username: &email, Email: &email, Enabled: gocloak.BoolP(true),
		})
		require.NoError(t, err)
		userID := uuid.MustParse(id)

		_, err = admin.MigrateUser(ctx, sourceRealm, "does-not-exist-"+uuid.NewString()[:8], userID)
		require.Error(t, err, "partialImport against a nonexistent realm must surface an error")

		_, getErr := raw.GetUserByID(ctx, tok, sourceRealm, id)
		require.Error(t, getErr, "the source user is deleted before the target import attempt, by design (§8.3)")
	})
}
