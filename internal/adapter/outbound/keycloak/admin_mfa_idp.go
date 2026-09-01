package keycloak

import (
	"context"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// AssignRequiresMFA is idempotent (assign-if-absent, §8.6).
func (a *Admin) AssignRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) error {
	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("assign_requires_mfa: token", err)
	}
	has, err := a.hasRealmRole(ctx, tok, realmName, userID)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	role, err := a.client.GetRealmRole(ctx, tok, realmName, RequiresMFARoleName)
	if err != nil {
		return wrapErr("assign_requires_mfa: get_role", err)
	}
	if err := a.client.AddRealmRoleToUser(ctx, tok, realmName, userID.String(), []gocloak.Role{*role}); err != nil {
		return wrapErr("assign_requires_mfa", err)
	}
	return nil
}

// RevokeRequiresMFA is idempotent (revoke-if-present, §8.6).
func (a *Admin) RevokeRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) error {
	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("revoke_requires_mfa: token", err)
	}
	has, err := a.hasRealmRole(ctx, tok, realmName, userID)
	if err != nil {
		return err
	}
	if !has {
		return nil
	}
	role, err := a.client.GetRealmRole(ctx, tok, realmName, RequiresMFARoleName)
	if err != nil {
		return wrapErr("revoke_requires_mfa: get_role", err)
	}
	if err := a.client.DeleteRealmRoleFromUser(ctx, tok, realmName, userID.String(), []gocloak.Role{*role}); err != nil {
		return wrapErr("revoke_requires_mfa", err)
	}
	return nil
}

// HasRequiresMFA reports whether userID currently holds the requires-mfa
// realm role in realmName.
func (a *Admin) HasRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) (bool, error) {
	tok, err := a.token(ctx)
	if err != nil {
		return false, wrapErr("has_requires_mfa: token", err)
	}
	return a.hasRealmRole(ctx, tok, realmName, userID)
}

func (a *Admin) hasRealmRole(ctx context.Context, tok, realmName string, userID uuid.UUID) (bool, error) {
	roles, err := a.client.GetRealmRolesByUserID(ctx, tok, realmName, userID.String())
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, wrapErr("get_realm_roles_by_user", err)
	}
	for _, r := range roles {
		if r.Name != nil && *r.Name == RequiresMFARoleName {
			return true, nil
		}
	}
	return false, nil
}

// ResetMFA deletes every OTP/WebAuthn credential for userID and, if
// requiresMFA, re-adds CONFIGURE_TOTP so the next login forces re-enroll
// (RP-9). Idempotent — no enrolled credentials is a no-op success.
func (a *Admin) ResetMFA(ctx context.Context, realmName string, userID uuid.UUID, requiresMFA bool) error {
	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("reset_mfa: token", err)
	}
	creds, err := a.client.GetCredentials(ctx, tok, realmName, userID.String())
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return wrapErr("reset_mfa: get_credentials", err)
	}
	for _, c := range creds {
		if c.Type == nil || c.ID == nil {
			continue
		}
		switch *c.Type {
		case "otp", "webauthn", "webauthn-passwordless":
			if err := a.client.DeleteCredentials(ctx, tok, realmName, userID.String(), *c.ID); err != nil && !isNotFound(err) {
				return wrapErr("reset_mfa: delete_credential", err)
			}
		}
	}
	if requiresMFA {
		user, err := a.client.GetUserByID(ctx, tok, realmName, userID.String())
		if err != nil {
			return wrapErr("reset_mfa: get_user", err)
		}
		actions := []string{"CONFIGURE_TOTP"}
		if user.RequiredActions != nil {
			actions = append(actions, *user.RequiredActions...)
		}
		user.RequiredActions = &actions
		if err := a.client.UpdateUser(ctx, tok, realmName, *user); err != nil {
			return wrapErr("reset_mfa: update_user", err)
		}
	}
	return nil
}

// CreateOrUpdateIdp configures a SAML/OIDC IdP on the dedicated realm
// (RP-6, behind the §11.6 gate enforced by the service layer). idp.Secret
// passes through transiently — never persisted by this adapter.
func (a *Admin) CreateOrUpdateIdp(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("configure_idp: token", err)
	}
	cfg := map[string]string{}
	for k, v := range idp.Config {
		if s, ok := v.(string); ok {
			cfg[k] = s
		}
	}
	if idp.Secret != "" {
		cfg["clientSecret"] = idp.Secret
	}
	rep := gocloak.IdentityProviderRepresentation{
		Alias:       &idp.Alias,
		ProviderID:  &idp.Protocol,
		DisplayName: &idp.DisplayName,
		Enabled:     &idp.Enabled,
		Config:      &cfg,
	}
	existing, err := a.client.GetIdentityProvider(ctx, tok, realmName, idp.Alias)
	if err != nil && !isNotFound(err) {
		return wrapErr("configure_idp: get", err)
	}
	if existing != nil {
		if err := a.client.UpdateIdentityProvider(ctx, tok, realmName, idp.Alias, rep); err != nil {
			return wrapErr("configure_idp: update", err)
		}
		return nil
	}
	if _, err := a.client.CreateIdentityProvider(ctx, tok, realmName, rep); err != nil {
		return wrapErr("configure_idp: create", err)
	}
	return nil
}

// DeleteIdp removes the Keycloak IdP (RP-7 — the caller/service also
// removes the OpenBao secret and the local row).
func (a *Admin) DeleteIdp(ctx context.Context, realmName, alias string) error {
	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("delete_idp: token", err)
	}
	if err := a.client.DeleteIdentityProvider(ctx, tok, realmName, alias); err != nil && !isNotFound(err) {
		return wrapErr("delete_idp", err)
	}
	return nil
}

// ApplyRealmConfig applies a realm-affecting setting synchronously
// (RP-INV-6, §8.10). Only local_accounts_enabled is currently implemented;
// idp:<alias> settings toggle that IdP's Enabled flag.
func (a *Admin) ApplyRealmConfig(ctx context.Context, realmName, settingKey string, desiredValue any) error {
	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("apply_realm_config: token", err)
	}
	switch settingKey {
	case "local_accounts_enabled":
		enabled, _ := desiredValue.(bool) //nolint:errcheck // non-bool desiredValue defaults to false; these settings are bool-only per domain.IsAllowedSettingKey's allow-list
		// registrationAllowed doubles as the "local accounts" toggle on
		// realms federated via an external IdP — Keycloak has no single
		// dedicated field for this; the realm attribute below is this
		// service's own convention, read back by realm-config GET (RP-11).
		if err := a.client.UpdateRealm(ctx, tok, gocloak.RealmRepresentation{
			Realm:      &realmName,
			Attributes: &map[string]string{"local_accounts_enabled": boolStr(enabled)},
		}); err != nil {
			return wrapErr("apply_realm_config", err)
		}
	default:
		alias, ok := idpAliasFromSettingKey(settingKey)
		if !ok {
			return wrapErr("apply_realm_config", errUnsupportedSetting(settingKey))
		}
		enabled, _ := desiredValue.(bool) //nolint:errcheck // non-bool desiredValue defaults to false; these settings are bool-only per domain.IsAllowedSettingKey's allow-list
		idp, err := a.client.GetIdentityProvider(ctx, tok, realmName, alias)
		if err != nil {
			return wrapErr("apply_realm_config: get_idp", err)
		}
		idp.Enabled = &enabled
		if err := a.client.UpdateIdentityProvider(ctx, tok, realmName, alias, *idp); err != nil {
			return wrapErr("apply_realm_config: update_idp", err)
		}
	}
	return nil
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func idpAliasFromSettingKey(key string) (string, bool) {
	const prefix = "idp:"
	if len(key) > len(prefix) && key[:len(prefix)] == prefix {
		return key[len(prefix):], true
	}
	return "", false
}

type unsupportedSettingErr string

func (e unsupportedSettingErr) Error() string { return "unsupported realm setting: " + string(e) }

func errUnsupportedSetting(key string) error { return unsupportedSettingErr(key) }
