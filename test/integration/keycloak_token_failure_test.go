//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	keycloakadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/keycloak"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// TestKeycloakAdmin_TokenFailure_PropagatesFromEveryMethod covers the
// "operation: token" wrapErr branch repeated at the top of every Admin
// method — no Keycloak testcontainer is needed for this, only a server that
// always rejects the admin token mint, so this exercises ~20 otherwise
// expensive-to-reach error branches in well under a second.
func TestKeycloakAdmin_TokenFailure_PropagatesFromEveryMethod(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"forced test failure"}`))
	}))
	t.Cleanup(srv.Close)

	admin := keycloakadapter.New(keycloakadapter.Config{
		BaseURL: srv.URL, AdminRealm: "master", ClientID: "x", ClientSecret: "wrong",
	}, noopLogger{})
	ctx := context.Background()
	realm := "irrelevant"
	userID := uuid.New()

	require.Error(t, admin.Ready(ctx))
	require.Error(t, admin.CreateRealmFromTemplate(ctx, realm, "v1"))
	_, err := admin.RegisterOIDCClients(ctx, realm, "slug")
	require.Error(t, err)
	require.Error(t, admin.DisableRealm(ctx, realm))
	require.Error(t, admin.EnableRealm(ctx, realm))
	_, err = admin.ExportRealm(ctx, realm)
	require.Error(t, err)
	require.Error(t, admin.DeleteRealm(ctx, realm))
	require.Error(t, admin.DeleteRealmIfExists(ctx, realm))

	_, err = admin.CreateTrialUser(ctx, port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: uuid.New()})
	require.Error(t, err)
	_, err = admin.CreateInvitedUser(ctx, realm, port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: uuid.New()})
	require.Error(t, err)
	_, err = admin.MigrateUser(ctx, realm, realm, userID)
	require.Error(t, err)
	_, err = admin.ListUsersByTenant(ctx, realm, uuid.New())
	require.Error(t, err)
	require.Error(t, admin.DisableUsersInRealm(ctx, realm, uuid.New()))
	require.Error(t, admin.EnableUsersInRealm(ctx, realm, uuid.New()))
	require.Error(t, admin.HardDeleteUsersInRealm(ctx, realm, uuid.New()))
	require.Error(t, admin.DeleteUser(ctx, realm, userID))
	require.Error(t, admin.RevokeUserSessions(ctx, realm, userID))
	require.Error(t, admin.ForceLogoutRealm(ctx, realm))

	require.Error(t, admin.AssignRequiresMFA(ctx, realm, userID))
	require.Error(t, admin.RevokeRequiresMFA(ctx, realm, userID))
	_, err = admin.HasRequiresMFA(ctx, realm, userID)
	require.Error(t, err)
	require.Error(t, admin.ResetMFA(ctx, realm, userID, true))

	require.Error(t, admin.CreateOrUpdateIdp(ctx, realm, port.IdpKeycloakConfig{Alias: "x", Protocol: "oidc"}))
	require.Error(t, admin.DeleteIdp(ctx, realm, "x"))
	require.Error(t, admin.ApplyRealmConfig(ctx, realm, "local_accounts_enabled", true))
}
