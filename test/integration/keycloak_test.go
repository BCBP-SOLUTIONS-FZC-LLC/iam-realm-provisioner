//go:build integration

// Package integration_test exercises the gocloak-backed KeycloakAdmin
// adapter (RP-INV-1) against a real Keycloak testcontainer — §14.2's
// "Keycloak testcontainer" requirement: real realm clone, user create w/
// sub preservation, TOTP copy, requires-mfa role assign/revoke, realm
// disable/enable.
//
// Requires Docker on the runner. Tag: integration.
package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	keycloakadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/keycloak"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

const kcClientID = "iam-realm-provisioner"
const kcClientSecret = "test-secret"

// noopLogger satisfies port.Logger with no-ops — keeps test output focused
// on assertions, not adapter debug/info noise.
type noopLogger struct{}

func (noopLogger) Debug(string, map[string]any) {}
func (noopLogger) Info(string, map[string]any)  {}
func (noopLogger) Warn(string, map[string]any)  {}
func (noopLogger) Error(string, map[string]any) {}

var _ port.Logger = noopLogger{}

// startKeycloakContainer starts a Keycloak dev-mode container and bootstraps
// the "iam-realm-provisioner" master-realm service-account client (mirroring
// scripts/keycloak-bootstrap.sh) via kcadm.sh, returning its base URL.
// Factored out of setupKeycloak so other test files needing a raw gocloak
// client (fixture setup the KeycloakAdmin port doesn't expose, e.g. seeding
// a user with a pre-existing OTP/WebAuthn credential) can share the same
// bootstrap without duplicating it.
func startKeycloakContainer(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping keycloak integration test in short mode")
	}
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "quay.io/keycloak/keycloak:26.0",
		ExposedPorts: []string{"8080/tcp"},
		Env: map[string]string{
			"KEYCLOAK_ADMIN":          "admin",
			"KEYCLOAK_ADMIN_PASSWORD": "admin",
		},
		Cmd:        []string{"start-dev"},
		WaitingFor: wait.ForLog("Listening on: http://0.0.0.0:8080").WithStartupTimeout(120 * time.Second),
		// host.docker.internal lets the container reach an httptest server
		// bound to the host (EXT-6's client-jwt/jwks.url test) — Docker
		// Desktop (Mac/Windows) resolves this automatically; host-gateway
		// makes it resolve on Linux Docker (>=20.10) too.
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	mappedPort, err := container.MappedPort(ctx, "8080/tcp")
	require.NoError(t, err)
	host, err := container.Host(ctx)
	require.NoError(t, err)
	baseURL := fmt.Sprintf("http://%s:%s", host, mappedPort.Port())

	bootstrapScript := fmt.Sprintf(`
/opt/keycloak/bin/kcadm.sh config credentials --server http://localhost:8080 --realm master --user admin --password admin
/opt/keycloak/bin/kcadm.sh create clients -r master -s clientId=%s -s enabled=true -s publicClient=false -s serviceAccountsEnabled=true -s secret=%s
CLIENT_UUID=$(/opt/keycloak/bin/kcadm.sh get clients -r master -q clientId=%s --fields id --format csv --noquotes | tail -n1)
SA_USER_ID=$(/opt/keycloak/bin/kcadm.sh get clients/$CLIENT_UUID/service-account-user -r master --fields id --format csv --noquotes | tail -n1)
/opt/keycloak/bin/kcadm.sh add-roles -r master --uid $SA_USER_ID --rolename create-realm
`, kcClientID, kcClientSecret, kcClientID)

	exitCode, reader, err := container.Exec(ctx, []string{"bash", "-c", bootstrapScript})
	require.NoError(t, err)
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(reader)
	require.Equal(t, 0, exitCode, "keycloak bootstrap failed: %s", buf.String())

	return baseURL
}

// setupKeycloak starts a Keycloak dev-mode container and returns a ready
// KeycloakAdmin adapter wired to it.
func setupKeycloak(t *testing.T) *keycloakadapter.Admin {
	t.Helper()
	admin, _ := setupKeycloakWithURL(t)
	return admin
}

// setupKeycloakWithURL is setupKeycloak plus the container's base URL, for
// tests that also need a raw gocloak client (rawAdminClient) against the
// same server for fixture setup the KeycloakAdmin port doesn't expose.
func setupKeycloakWithURL(t *testing.T) (*keycloakadapter.Admin, string) {
	t.Helper()
	baseURL := startKeycloakContainer(t)
	admin := keycloakadapter.New(keycloakadapter.Config{
		BaseURL:      baseURL,
		AdminRealm:   "master",
		ClientID:     kcClientID,
		ClientSecret: kcClientSecret,
		RateLimitRPS: 50,
		Timeout:      10 * time.Second,
	}, noopLogger{})

	require.NoError(t, admin.Ready(context.Background()), "admin token bootstrap must succeed")
	return admin, baseURL
}

func TestKeycloakAdmin_Ready(t *testing.T) {
	t.Parallel()
	admin := setupKeycloak(t)
	require.NoError(t, admin.Ready(context.Background()))
}

func TestKeycloakAdmin_CreateRealmFromTemplate_And_DisableEnable(t *testing.T) {
	t.Parallel()
	admin := setupKeycloak(t)
	ctx := context.Background()
	realmName := "tenant-" + uuid.NewString()[:8]

	require.NoError(t, admin.CreateRealmFromTemplate(ctx, realmName, "v1"))
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, realmName) })

	// Idempotent teardown primitive works on a live realm too.
	require.NoError(t, admin.DisableRealm(ctx, realmName))
	require.NoError(t, admin.EnableRealm(ctx, realmName))

	clients, err := admin.RegisterOIDCClients(ctx, realmName, "acme")
	require.NoError(t, err)
	require.Contains(t, clients, "tender-web")
	require.Contains(t, clients, "tender-gateway")
}

func TestKeycloakAdmin_DeleteRealmIfExists_Idempotent(t *testing.T) {
	t.Parallel()
	admin := setupKeycloak(t)
	ctx := context.Background()
	// Deleting an absent realm must succeed (§9.3 compensating-teardown primitive).
	require.NoError(t, admin.DeleteRealmIfExists(ctx, "does-not-exist-"+uuid.NewString()))
}

func TestKeycloakAdmin_TrialUser_And_RequiresMFA(t *testing.T) {
	t.Parallel()
	admin := setupKeycloak(t)
	ctx := context.Background()
	realmName := "tenant-" + uuid.NewString()[:8]
	require.NoError(t, admin.CreateRealmFromTemplate(ctx, realmName, "v1"))
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, realmName) })

	tenantID := uuid.New()
	userID, err := admin.CreateInvitedUser(ctx, realmName, port.CreateUserRequest{
		Email: "owner@example.com", FullName: "Dana Owner", TenantID: tenantID,
		RequiredActions: []string{"VERIFY_EMAIL", "UPDATE_PASSWORD"}, EmailVerified: false,
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, userID)

	has, err := admin.HasRequiresMFA(ctx, realmName, userID)
	require.NoError(t, err)
	require.False(t, has, "requires-mfa must not be assigned yet")

	require.NoError(t, admin.AssignRequiresMFA(ctx, realmName, userID))
	has, err = admin.HasRequiresMFA(ctx, realmName, userID)
	require.NoError(t, err)
	require.True(t, has)

	// Idempotent — assigning again is a no-op success.
	require.NoError(t, admin.AssignRequiresMFA(ctx, realmName, userID))

	require.NoError(t, admin.RevokeRequiresMFA(ctx, realmName, userID))
	has, err = admin.HasRequiresMFA(ctx, realmName, userID)
	require.NoError(t, err)
	require.False(t, has)

	// Idempotent — revoking again (already absent) is a no-op success.
	require.NoError(t, admin.RevokeRequiresMFA(ctx, realmName, userID))

	// RP-9: resetting MFA for a user with no enrolled credentials is a
	// no-op success (idempotent absent-credential reset).
	require.NoError(t, admin.ResetMFA(ctx, realmName, userID, false))

	// RP-15: idempotent delete — first call removes the user, second is a no-op.
	require.NoError(t, admin.DeleteUser(ctx, realmName, userID))
	require.NoError(t, admin.DeleteUser(ctx, realmName, userID))

	// RP-16: revoking sessions for a user with none active is a no-op success.
	require.NoError(t, admin.RevokeUserSessions(ctx, realmName, userID))
}

// Regression test: Keycloak's declarative User Profile (24+) silently
// drops any user attribute not declared in its schema unless
// unmanagedAttributePolicy=ENABLED — confirmed empirically that the
// tenant_id attribute (RP-INV-2's entire mechanism) was otherwise dropped
// on create with no error. CreateRealmFromTemplate must enable it on
// every realm it creates.
func TestKeycloakAdmin_TenantIDAttribute_PersistsAndIsQueryable(t *testing.T) {
	t.Parallel()
	admin := setupKeycloak(t)
	ctx := context.Background()
	realmName := "tenant-" + uuid.NewString()[:8]
	require.NoError(t, admin.CreateRealmFromTemplate(ctx, realmName, "v1"))
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, realmName) })

	tenantID := uuid.New()
	userID, err := admin.CreateInvitedUser(ctx, realmName, port.CreateUserRequest{
		Email: "attr@example.com", FullName: "Attr Test", TenantID: tenantID,
	})
	require.NoError(t, err)

	found, err := admin.ListUsersByTenant(ctx, realmName, tenantID)
	require.NoError(t, err)
	require.Contains(t, found, userID, "the user created with this tenant_id must be findable by it")
}

func TestKeycloakAdmin_MigrateUser_PreservesSub(t *testing.T) {
	t.Parallel()
	admin := setupKeycloak(t)
	ctx := context.Background()

	// CreateTrialUser hardcodes realm "trial" (RP-INV-2) — this fresh
	// Keycloak instance only has "master" by default, so the shared trial
	// realm must be created first (reusing the same template is fine for
	// this test's purposes; production's shared trial realm is a
	// once-ever bootstrap, not per-tenant).
	require.NoError(t, admin.CreateRealmFromTemplate(ctx, "trial", "v1"))
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, "trial") })

	dedicatedRealm := "tenant-" + uuid.NewString()[:8]
	require.NoError(t, admin.CreateRealmFromTemplate(ctx, dedicatedRealm, "v1"))
	t.Cleanup(func() { _ = admin.DeleteRealmIfExists(ctx, dedicatedRealm) })

	tenantID := uuid.New()
	userID, err := admin.CreateTrialUser(ctx, port.CreateUserRequest{
		Email: "trial-user@example.com", FullName: "Trial User", TenantID: tenantID, EmailVerified: false,
	})
	require.NoError(t, err)

	webauthnReenroll, err := admin.MigrateUser(ctx, "trial", dedicatedRealm, userID)
	require.NoError(t, err)
	require.False(t, webauthnReenroll, "no WebAuthn credential existed on the source user")

	// sub preservation: the migrated user must exist in the dedicated
	// realm under the SAME id as the trial-realm sub.
	migratedHasMFA, err := admin.HasRequiresMFA(ctx, dedicatedRealm, userID)
	require.NoError(t, err) // succeeds only if userID resolves in dedicatedRealm
	require.False(t, migratedHasMFA)

	t.Cleanup(func() { _ = admin.HardDeleteUsersInRealm(ctx, "trial", tenantID) })
}
