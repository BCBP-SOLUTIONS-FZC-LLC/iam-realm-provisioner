package keycloak

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// TestTokenErrorPropagates exercises every method's leading `a.token(ctx)`
// failure branch — each wraps the token-mint error with wrapErr and returns
// immediately, before ever reaching a Keycloak REST call. A single
// always-401 fake token endpoint drives all of them.
func TestTokenErrorPropagates(t *testing.T) {
	a := newFakeAdmin(t, []tokenResponse{failTokenResponse()}, nil)
	ctx := context.Background()
	tenantID := uuid.New()
	userID := uuid.New()

	cases := map[string]func() error{
		"CreateRealmFromTemplate": func() error { return a.CreateRealmFromTemplate(ctx, "r", "tpl") },
		"RegisterOIDCClients": func() error {
			_, err := a.RegisterOIDCClients(ctx, "r", "slug")
			return err
		},
		"DisableRealm":        func() error { return a.DisableRealm(ctx, "r") },
		"EnableRealm":         func() error { return a.EnableRealm(ctx, "r") },
		"ExportRealm":         func() error { _, err := a.ExportRealm(ctx, "r"); return err },
		"DeleteRealm":         func() error { return a.DeleteRealm(ctx, "r") },
		"DeleteRealmIfExists": func() error { return a.DeleteRealmIfExists(ctx, "r") },
		"CreateTrialUser": func() error {
			_, err := a.CreateTrialUser(ctx, port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: tenantID})
			return err
		},
		"CreateInvitedUser": func() error {
			_, err := a.CreateInvitedUser(ctx, "r", port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: tenantID})
			return err
		},
		"MigrateUser": func() error {
			_, err := a.MigrateUser(ctx, "src", "dst", userID)
			return err
		},
		"ListUsersByTenant": func() error {
			_, err := a.ListUsersByTenant(ctx, "r", tenantID)
			return err
		},
		"DisableUsersInRealm":    func() error { return a.DisableUsersInRealm(ctx, "r", tenantID) },
		"EnableUsersInRealm":     func() error { return a.EnableUsersInRealm(ctx, "r", tenantID) },
		"HardDeleteUsersInRealm": func() error { return a.HardDeleteUsersInRealm(ctx, "r", tenantID) },
		"DeleteUser":             func() error { return a.DeleteUser(ctx, "r", userID) },
		"RevokeUserSessions":     func() error { return a.RevokeUserSessions(ctx, "r", userID) },
		"ForceLogoutRealm":       func() error { return a.ForceLogoutRealm(ctx, "r") },
		"AssignRequiresMFA":      func() error { return a.AssignRequiresMFA(ctx, "r", userID) },
		"RevokeRequiresMFA":      func() error { return a.RevokeRequiresMFA(ctx, "r", userID) },
		"HasRequiresMFA": func() error {
			_, err := a.HasRequiresMFA(ctx, "r", userID)
			return err
		},
		"ResetMFA": func() error { return a.ResetMFA(ctx, "r", userID, false) },
		"CreateOrUpdateIdp": func() error {
			return a.CreateOrUpdateIdp(ctx, "r", port.IdpKeycloakConfig{Alias: "idp1"})
		},
		"DeleteIdp":        func() error { return a.DeleteIdp(ctx, "r", "idp1") },
		"ApplyRealmConfig": func() error { return a.ApplyRealmConfig(ctx, "r", "local_accounts_enabled", true) },
	}

	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			err := call()
			require.Error(t, err, "%s must propagate a token-mint failure", name)
			var derr *domain.Error
			require.ErrorAs(t, err, &derr)
			require.Equal(t, domain.ErrKeycloakUnavailable, derr.Code)
		})
	}
}

// TestReady_TokenError confirms Ready() classifies a token bootstrap
// failure as ErrKeycloakUnavailable (used by /readyz).
func TestReady_TokenError(t *testing.T) {
	a := newFakeAdmin(t, []tokenResponse{failTokenResponse()}, nil)
	err := a.Ready(context.Background())
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	require.Equal(t, domain.ErrKeycloakUnavailable, derr.Code)
}

// TestReady_Success confirms a healthy token mint satisfies Ready().
func TestReady_Success(t *testing.T) {
	a := newFakeAdmin(t, nil, nil)
	require.NoError(t, a.Ready(context.Background()))
}

// TestNew_Defaults confirms New fills in the documented zero-value
// defaults for Timeout and RateLimitRPS.
func TestNew_Defaults(t *testing.T) {
	a := New(Config{BaseURL: "http://example.invalid"}, nopLogger{})
	require.Equal(t, 8_000_000_000, int(a.cfg.Timeout)) // 8s in ns
	require.Equal(t, 20, a.cfg.RateLimitRPS)
}

// TestNew_ExplicitConfigPreserved confirms non-zero Config fields are not
// overwritten by New's defaulting.
func TestNew_ExplicitConfigPreserved(t *testing.T) {
	a := New(Config{BaseURL: "http://example.invalid", Timeout: 1, RateLimitRPS: 5}, nopLogger{})
	require.Equal(t, 1, int(a.cfg.Timeout))
	require.Equal(t, 5, a.cfg.RateLimitRPS)
}
