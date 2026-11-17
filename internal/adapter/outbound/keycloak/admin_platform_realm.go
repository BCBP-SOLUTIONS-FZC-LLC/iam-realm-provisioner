package keycloak

import (
	"context"
	"fmt"
	"strings"

	"github.com/Nerzal/gocloak/v13"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// platformTokenSettings are the short-session settings applied to the
// platform realm: 5-min access token, 30-min idle session, 8-h max session.
// Shorter than tenant realms (12/24 h idle/max) because operator consoles
// must time out quickly (CD-52 "short sessions").
var platformTokenSettings = realmTokenSettings{
	AccessTokenLifespan:      gocloak.IntP(300),
	SsoSessionIdleTimeout:    gocloak.IntP(1800),
	SsoSessionMaxLifespan:    gocloak.IntP(28800),
	ClientSessionIdleTimeout: gocloak.IntP(1800),
	ClientSessionMaxLifespan: gocloak.IntP(28800),
	RevokeRefreshToken:       gocloak.BoolP(true),
	RefreshTokenMaxReuse:     gocloak.IntP(0),
}

// EnsurePlatformRealm creates the operator platform realm when absent, then
// converges it: platform_operator role, realm-wide OTP enforcement, and short
// session lifetimes (CD-52 item 2). Idempotent — a concurrent create by
// another replica (409) is treated as an existing realm and converged.
func (a *Admin) EnsurePlatformRealm(ctx context.Context, realmName string) (err error) {
	ctx, done := a.begin(ctx, "ensure_platform_realm")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("ensure_platform_realm: token", err)
	}
	_, err = a.client.GetRealm(ctx, tok, realmName)
	switch {
	case err == nil:
	case !isNotFound(err):
		return wrapErr("ensure_platform_realm: get_realm", err)
	default:
		if tok, err = a.createPlatformRealm(ctx, tok, realmName); err != nil {
			if !isConflict(err) {
				return err
			}
			// Lost create race to another replica: re-mint token so it
			// carries the admin grant on the newly-created realm.
			// Known: if another goroutine's token refresh is mid-singleflight
			// when Invalidate() fires, token() may join that in-flight call
			// and return a pre-grant token. convergePlatformRealm will then
			// 403 on CreateRealmRole and log ERROR — the 30 s retry loop
			// recovers correctly on the next attempt with a fresh token.
			a.tokens.Invalidate()
			if tok, err = a.token(ctx); err != nil {
				return wrapErr("ensure_platform_realm: token", err)
			}
		}
	}
	return a.convergePlatformRealm(ctx, tok, realmName)
}

// createPlatformRealm creates a minimal platform realm (no tenant-realm
// template — the operator realm carries no tender-web or tenant_id
// concepts) and returns a freshly-minted token.
func (a *Admin) createPlatformRealm(ctx context.Context, tok, realmName string) (string, error) {
	rep := gocloak.RealmRepresentation{
		Realm:   gocloak.StringP(realmName),
		Enabled: gocloak.BoolP(true),
	}
	if _, err := a.client.CreateRealm(ctx, tok, rep); err != nil {
		if isConflict(err) {
			return "", err
		}
		return "", wrapErr("create_platform_realm", err)
	}
	// Re-mint: the token must postdate the realm-admin grant (same
	// reasoning as createRealm — confirmed empirically against live KC).
	a.tokens.Invalidate()
	newTok, err := a.token(ctx)
	if err != nil {
		return "", wrapErr("create_platform_realm: refresh token", err)
	}
	return newTok, nil
}

// convergePlatformRealm applies the three invariants of the platform realm:
// platform_operator role, realm-wide OTP requirement, and short sessions.
// Every step is idempotent so a re-run on an already-correct realm is a no-op.
func (a *Admin) convergePlatformRealm(ctx context.Context, tok, realmName string) error {
	if _, err := a.client.CreateRealmRole(ctx, tok, realmName, gocloak.Role{
		Name: gocloak.StringP(domain.PlatformOperatorRoleName),
	}); err != nil && !isConflict(err) {
		return wrapErr("ensure_platform_realm: create_role", err)
	}
	if err := a.enforceOTPForAll(ctx, tok, realmName); err != nil {
		return err
	}
	return a.applyPlatformTokenSettings(ctx, tok, realmName)
}

// enforceOTPForAll sets the "Browser - Conditional OTP" subflow in the
// realm's browser authentication flow to REQUIRED so that every login
// requires a second factor, regardless of whether the user holds the
// requires-mfa realm role (the per-user conditional used in tenant realms).
// Idempotent: if the subflow is already REQUIRED the update is skipped.
//
// The flow alias "browser" is Keycloak's standard default and is unchanged
// in Keycloak 26. The subflow display name "Browser - Conditional OTP" has
// been stable since Keycloak 12; if a future Keycloak version renames it
// this returns an error so the retry loop surfaces the drift rather than
// silently leaving MFA unenforced (a security invariant, not just config).
func (a *Admin) enforceOTPForAll(ctx context.Context, tok, realmName string) error {
	executions, err := a.client.GetAuthenticationExecutions(ctx, tok, realmName, "browser")
	if err != nil {
		return wrapErr("ensure_platform_realm: get_auth_executions", err)
	}
	for _, exec := range executions {
		if exec == nil || exec.DisplayName == nil {
			continue
		}
		if !strings.Contains(*exec.DisplayName, "Conditional OTP") {
			continue
		}
		if exec.Requirement != nil && *exec.Requirement == "REQUIRED" {
			return nil // already enforced
		}
		exec.Requirement = gocloak.StringP("REQUIRED")
		if err := a.client.UpdateAuthenticationExecution(ctx, tok, realmName, "browser", *exec); err != nil {
			return wrapErr("ensure_platform_realm: enforce_otp", err)
		}
		return nil
	}
	// The "Browser - Conditional OTP" execution was not found — the browser
	// flow structure has drifted from the expected Keycloak default. Return
	// an error so the ensure-loop retries and logs at ERROR until an
	// operator investigates; leaving MFA silently unenforced is not
	// acceptable on the platform realm.
	return fmt.Errorf("keycloak: platform realm %q: 'Browser - Conditional OTP' execution not found in browser flow — cannot enforce MFA for all users", realmName)
}

// applyPlatformTokenSettings writes the platform realm's short-session
// settings via a partial realm PUT. platformTokenSettings is a package-level
// var, so a single test can verify the values without touching Keycloak.
func (a *Admin) applyPlatformTokenSettings(ctx context.Context, tok, realmName string) error {
	resp, err := a.client.GetRequestWithBearerAuth(ctx, tok).
		SetBody(platformTokenSettings).
		Put(a.cfg.BaseURL + "/admin/realms/" + realmName)
	if err != nil {
		return wrapErr("ensure_platform_realm: token_settings", err)
	}
	if resp.IsError() {
		return fmt.Errorf("keycloak: update platform realm token settings: unexpected status %d: %s",
			resp.StatusCode(), resp.String())
	}
	return nil
}
