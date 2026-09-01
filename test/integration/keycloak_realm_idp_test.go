//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// TestKeycloakAdmin_RealmLifecycle_ConflictExportDelete batches the realm
// lifecycle branches not covered by the CreateRealmFromTemplate/DisableEnable
// happy-path test: the create-conflict branch, a real ExportRealm snapshot,
// and DeleteRealm's non-idempotent (as opposed to DeleteRealmIfExists) double
// -delete behavior, all against one container.
func TestKeycloakAdmin_RealmLifecycle_ConflictExportDelete(t *testing.T) {
	t.Parallel()
	admin := setupKeycloak(t)
	ctx := context.Background()
	realmName := "tenant-" + uuid.NewString()[:8]

	require.NoError(t, admin.CreateRealmFromTemplate(ctx, realmName, "v1"))
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, realmName) })

	err := admin.CreateRealmFromTemplate(ctx, realmName, "v1")
	require.Error(t, err, "creating an already-existing realm must fail")
	var domainErr *domain.Error
	require.ErrorAs(t, err, &domainErr)
	require.Equal(t, domain.ErrRealmAlreadyDedicated, domainErr.Code)

	tenantID := uuid.New()
	_, err = admin.CreateInvitedUser(ctx, realmName, port.CreateUserRequest{
		Email: "export-user@example.com", FullName: "Export User", TenantID: tenantID,
	})
	require.NoError(t, err)

	exported, err := admin.ExportRealm(ctx, realmName)
	require.NoError(t, err)
	var doc struct {
		Realm struct {
			Realm *string `json:"realm"`
		} `json:"realm"`
		Users []struct {
			Email *string `json:"email"`
		} `json:"users"`
		ExportedAt string `json:"exported_at"`
	}
	require.NoError(t, json.Unmarshal(exported, &doc))
	require.Equal(t, realmName, *doc.Realm.Realm)
	require.NotEmpty(t, doc.ExportedAt)
	var foundEmail bool
	for _, u := range doc.Users {
		if u.Email != nil && *u.Email == "export-user@example.com" {
			foundEmail = true
		}
	}
	require.True(t, foundEmail, "ExportRealm must include the realm's users")

	require.NoError(t, admin.DeleteRealm(ctx, realmName))
	require.Error(t, admin.DeleteRealm(ctx, realmName), "DeleteRealm (unlike DeleteRealmIfExists) is not idempotent")
}

// TestKeycloakAdmin_Idp_CreateUpdateDelete_And_RealmConfig batches the IdP
// create/update/delete branches and every ApplyRealmConfig setting-key
// branch against one container.
func TestKeycloakAdmin_Idp_CreateUpdateDelete_And_RealmConfig(t *testing.T) {
	t.Parallel()
	admin := setupKeycloak(t)
	ctx := context.Background()
	realmName := "tenant-" + uuid.NewString()[:8]
	require.NoError(t, admin.CreateRealmFromTemplate(ctx, realmName, "v1"))
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, realmName) })

	alias := "acme-saml"
	require.NoError(t, admin.CreateOrUpdateIdp(ctx, realmName, port.IdpKeycloakConfig{
		Alias: alias, Protocol: "saml", DisplayName: "Acme SAML", Enabled: true,
		Config: map[string]any{"singleSignOnServiceUrl": "https://acme.example.com/sso"},
		Secret: "shh",
	}))

	require.NoError(t, admin.CreateOrUpdateIdp(ctx, realmName, port.IdpKeycloakConfig{
		Alias: alias, Protocol: "saml", DisplayName: "Acme SAML Updated", Enabled: true,
		Config: map[string]any{"singleSignOnServiceUrl": "https://acme.example.com/sso"},
	}))

	require.NoError(t, admin.ApplyRealmConfig(ctx, realmName, "local_accounts_enabled", true))
	require.NoError(t, admin.ApplyRealmConfig(ctx, realmName, "local_accounts_enabled", false))

	require.NoError(t, admin.ApplyRealmConfig(ctx, realmName, "idp:"+alias, false))
	require.NoError(t, admin.ApplyRealmConfig(ctx, realmName, "idp:"+alias, true))

	err := admin.ApplyRealmConfig(ctx, realmName, "idp:does-not-exist", true)
	require.Error(t, err, "toggling a nonexistent IdP alias must fail at get_idp")

	err = admin.ApplyRealmConfig(ctx, realmName, "unsupported_setting_key", true)
	require.Error(t, err, "an unlisted setting key must fail (domain.IsAllowedSettingKey's allow-list is enforced by the service layer, but the adapter itself must also reject it)")

	require.NoError(t, admin.DeleteIdp(ctx, realmName, alias))
	require.NoError(t, admin.DeleteIdp(ctx, realmName, alias), "deleting an already-absent IdP is idempotent")
}
