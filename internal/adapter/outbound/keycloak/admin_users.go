package keycloak

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

const tenantIDAttribute = "tenant_id"

// buildCredential converts bff-signup's HashedPasswordSeed JSON blob into
// Keycloak's import format. SecretData/CredentialData tell Keycloak to store
// the credential without re-hashing (contrast with Value, which Keycloak
// hashes using the realm's configured algorithm, causing a double-hash).
func buildCredential(hashedPasswordSeed string) (*gocloak.CredentialRepresentation, error) {
	var seed struct {
		Algorithm  string `json:"algorithm"`
		Iterations int    `json:"iterations"`
		Salt       string `json:"salt"`
		Hash       string `json:"hash"`
	}
	if err := json.Unmarshal([]byte(hashedPasswordSeed), &seed); err != nil {
		return nil, fmt.Errorf("hashed_password_seed is not valid JSON: %w", err)
	}
	secretData, err := json.Marshal(map[string]any{
		"value": seed.Hash,
		"salt":  seed.Salt,
	})
	if err != nil {
		return nil, err
	}
	credentialData, err := json.Marshal(map[string]any{
		"hashIterations": seed.Iterations,
		"algorithm":      seed.Algorithm,
	})
	if err != nil {
		return nil, err
	}
	return &gocloak.CredentialRepresentation{
		Type:           gocloak.StringP("password"),
		SecretData:     gocloak.StringP(string(secretData)),
		CredentialData: gocloak.StringP(string(credentialData)),
		Temporary:      gocloak.BoolP(false),
	}, nil
}

func buildUser(req port.CreateUserRequest) (gocloak.User, error) {
	first, last := splitName(req.FullName)
	u := gocloak.User{
		Username:      &req.Email,
		Email:         &req.Email,
		FirstName:     &first,
		LastName:      &last,
		Enabled:       gocloak.BoolP(true),
		EmailVerified: gocloak.BoolP(req.EmailVerified),
		Attributes:    &map[string][]string{tenantIDAttribute: {req.TenantID.String()}},
	}
	if len(req.RequiredActions) > 0 {
		u.RequiredActions = &req.RequiredActions
	}
	if req.HashedPasswordSeed != "" {
		cred, err := buildCredential(req.HashedPasswordSeed)
		if err != nil {
			return gocloak.User{}, err
		}
		u.Credentials = &[]gocloak.CredentialRepresentation{*cred}
	}
	return u, nil
}

func splitName(full string) (first, last string) {
	for i := len(full) - 1; i >= 0; i-- {
		if full[i] == ' ' {
			return full[:i], full[i+1:]
		}
	}
	return full, ""
}

// CreateTrialUser creates a user in the shared trial realm (RP-1, RP-INV-2).
func (a *Admin) CreateTrialUser(ctx context.Context, req port.CreateUserRequest) (userID uuid.UUID, err error) {
	ctx, done := a.begin(ctx, "create_trial_user")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return uuid.Nil, wrapErr("create_trial_user: token", err)
	}
	if len(req.RequiredActions) == 0 {
		req.RequiredActions = []string{"VERIFY_EMAIL"}
	}
	user, err := buildUser(req)
	if err != nil {
		return uuid.Nil, wrapErr("create_trial_user: build_user", err)
	}
	id, err := a.client.CreateUser(ctx, tok, "trial", user)
	if err != nil {
		return uuid.Nil, wrapCreateUserErr("create_trial_user", req.Email, err)
	}
	if err := a.client.SendVerifyEmail(ctx, tok, id, "trial"); err != nil {
		a.log.Warn("send verify email failed", map[string]any{"error": err.Error(), "user_id": id})
	}
	return uuid.Parse(id)
}

// CreateInvitedUser creates a user in realmName with requiredActions
// applied verbatim (RP-5, F5).
func (a *Admin) CreateInvitedUser(ctx context.Context, realmName string, req port.CreateUserRequest) (userID uuid.UUID, err error) {
	ctx, done := a.begin(ctx, "create_invited_user")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return uuid.Nil, wrapErr("create_invited_user: token", err)
	}
	user, err := buildUser(req)
	if err != nil {
		return uuid.Nil, wrapErr("create_invited_user: build_user", err)
	}
	id, err := a.client.CreateUser(ctx, tok, realmName, user)
	if err != nil {
		return uuid.Nil, wrapCreateUserErr("create_invited_user", req.Email, err)
	}
	return uuid.Parse(id)
}

// wrapCreateUserErr classifies a CreateUser failure: a 409 (Keycloak's
// username/email uniqueness constraint) surfaces as the distinguishable
// domain.ErrUserEmailConflict — instead of the generic wrapErr's
// ErrKeycloakUnavailable, which a caller could never tell apart from an
// actual Keycloak outage — so provisioning_service.go can resolve it via
// FindUserByEmail (§9.2: a lost-response retry of the caller's own request
// vs. a genuine conflict). Every other error keeps wrapErr's semantics.
func wrapCreateUserErr(op, email string, err error) error {
	if isConflict(err) {
		return domain.NewError(domain.ErrUserEmailConflict, "email already exists in realm").WithDetails(map[string]any{"email": email})
	}
	return wrapErr(op, err)
}

// FindUserByEmail looks up a user by exact email match in realmName (§9.2)
// — used by callers of CreateTrialUser/CreateInvitedUser to disambiguate a
// lost-response retry of their own request from a genuine email conflict.
func (a *Admin) FindUserByEmail(ctx context.Context, realmName, email string) (existing *port.ExistingUser, err error) {
	ctx, done := a.begin(ctx, "find_user_by_email")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return nil, wrapErr("find_user_by_email: token", err)
	}
	exact := true
	users, err := a.client.GetUsers(ctx, tok, realmName, gocloak.GetUsersParams{Email: &email, Exact: &exact})
	if err != nil {
		return nil, wrapErr("find_user_by_email", err)
	}
	if len(users) == 0 || users[0].ID == nil {
		return nil, domain.NewError(domain.ErrKeycloakUserNotFound, "no user with this email in realm").WithDetails(map[string]any{"realm": realmName})
	}
	id, err := uuid.Parse(*users[0].ID)
	if err != nil {
		return nil, wrapErr("find_user_by_email: parse id", err)
	}
	existing = &port.ExistingUser{ID: id}
	if users[0].Attributes != nil {
		if vals, ok := (*users[0].Attributes)[tenantIDAttribute]; ok && len(vals) > 0 {
			if tid, parseErr := uuid.Parse(vals[0]); parseErr == nil {
				existing.TenantID = tid
			}
		}
	}
	return existing, nil
}

// MigrateUser recreates a trial user in the dedicated realm, preserving
// sub, copying TOTP, and flagging WEBAUTHN_REGISTER when the source held a
// WebAuthn credential (WebAuthn cannot migrate across realms, §8.3).
func (a *Admin) MigrateUser(ctx context.Context, sourceRealm, targetRealm string, keycloakUserID uuid.UUID) (webauthnReenrollRequired bool, err error) {
	ctx, done := a.begin(ctx, "migrate_user")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return false, wrapErr("migrate_user: token", err)
	}
	src, err := a.client.GetUserByID(ctx, tok, sourceRealm, keycloakUserID.String())
	if err != nil {
		return false, wrapErr("migrate_user: get_user", err)
	}
	creds, err := a.client.GetCredentials(ctx, tok, sourceRealm, keycloakUserID.String())
	if err != nil {
		return false, wrapErr("migrate_user: get_credentials", err)
	}

	target := *src
	target.ID = gocloak.StringP(keycloakUserID.String()) // preserve sub
	var totp *gocloak.CredentialRepresentation
	webauthnReenroll := false
	for _, c := range creds {
		if c.Type == nil {
			continue
		}
		switch *c.Type {
		case "otp":
			totp = c
		case "webauthn", "webauthn-passwordless":
			webauthnReenroll = true
		}
	}
	if totp != nil {
		target.Credentials = &[]gocloak.CredentialRepresentation{*totp}
	} else {
		target.Credentials = nil
	}
	if webauthnReenroll {
		var actions []string
		if target.RequiredActions != nil {
			actions = append(actions, *target.RequiredActions...)
		}
		actions = append(actions, "webauthn-register")
		target.RequiredActions = &actions
	}

	// Keycloak's user id is a global primary key shared across every
	// realm in the deployment, not scoped per-realm (confirmed
	// empirically: importing a user with an id that still exists in
	// ANOTHER realm 409s with "Duplicate resource error", even into a
	// brand-new target realm). The source user must therefore be deleted
	// BEFORE the target is created with the same id — safe here because
	// §8.3's caller force-logs-out the shared-realm session for this user
	// as part of the same conversion flow, and a mid-flight failure after
	// this delete is recovered by the caller's compensating teardown +
	// idempotent retry (§9.3): a retry's GetUserByID on the now-absent
	// source will fail fast rather than silently re-reading stale data.
	//
	// The standard POST /admin/realms/{realm}/users endpoint does NOT
	// honor a caller-supplied id — Keycloak always server-generates one
	// (also confirmed empirically). sub preservation (RP-INV-2/§8.3)
	// instead requires the partialImport endpoint, which Keycloak
	// documents as import/restore tooling and DOES accept an explicit id.
	// gocloak v13 has no wrapper for it, so this uses its resty escape
	// hatch directly.
	//
	// isNotFound-tolerant like every sibling delete in this file
	// (DeleteUser, HardDeleteUsersInRealm, RevokeUserSessions,
	// ForceLogoutRealm) — required here specifically because
	// retryTransport can transparently retry this exact DELETE: if the
	// first attempt actually succeeded at Keycloak but its response was
	// lost (timeout/5xx), the retry lands on an already-deleted user and
	// gets a 404. Treating that as fatal would abort MigrateUser after
	// the source user is already gone and never created in the target —
	// silent, permanent data loss. A genuine 404 (the user never existed)
	// is equally safe to tolerate: it only means the user is already
	// absent from the source, which is exactly the state this delete is
	// trying to reach before creating them in the target.
	if err := a.client.DeleteUser(ctx, tok, sourceRealm, keycloakUserID.String()); err != nil && !isNotFound(err) {
		return false, wrapErr("migrate_user: delete_source", err)
	}
	if err := a.partialImportUser(ctx, tok, targetRealm, target); err != nil {
		return false, wrapErr("migrate_user: partial_import", err)
	}
	return webauthnReenroll, nil
}

// partialImportUser calls POST /admin/realms/{realm}/partialImport with a
// single user, ifResourceExists=FAIL so a genuine id collision surfaces as
// an error rather than silently overwriting an unrelated user.
func (a *Admin) partialImportUser(ctx context.Context, tok, realm string, user gocloak.User) error {
	body := map[string]any{
		"ifResourceExists": "FAIL",
		"users":            []gocloak.User{user},
	}
	resp, err := a.client.GetRequestWithBearerAuth(ctx, tok).
		SetBody(body).
		Post(a.cfg.BaseURL + "/admin/realms/" + realm + "/partialImport")
	if err != nil {
		return err
	}
	if resp.IsError() {
		return fmt.Errorf("partialImport: unexpected status %d: %s", resp.StatusCode(), resp.String())
	}
	var result struct {
		Overwritten int `json:"overwritten"`
		Added       int `json:"added"`
		Skipped     int `json:"skipped"`
		Results     []struct {
			Action       string `json:"action"`
			ResourceType string `json:"resourceType"`
			ResourceName string `json:"resourceName"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp.Body(), &result); err != nil {
		return fmt.Errorf("partialImport: decode response: %w", err)
	}
	if result.Added != 1 {
		return fmt.Errorf("partialImport: expected 1 user added, got added=%d skipped=%d overwritten=%d (body: %s)",
			result.Added, result.Skipped, result.Overwritten, resp.String())
	}
	return nil
}

// usersByTenantAttribute lists every user in realmName carrying tenant_id
// == tenantID (used for shared-trial suspend/expiry/hard-delete — never
// touches the shared realm itself, RP-INV-2/§8.12).
func (a *Admin) usersByTenantAttribute(ctx context.Context, tok, realmName string, tenantID uuid.UUID) ([]*gocloak.User, error) {
	q := fmt.Sprintf("tenant_id:%s", tenantID.String())
	return a.client.GetUsers(ctx, tok, realmName, gocloak.GetUsersParams{Q: &q})
}

// ListUsersByTenant returns the Keycloak user IDs carrying tenant_id
// (§8.3 — enumerates trial users to migrate at conversion).
func (a *Admin) ListUsersByTenant(ctx context.Context, realmName string, tenantID uuid.UUID) (userIDs []uuid.UUID, err error) {
	ctx, done := a.begin(ctx, "list_users_by_tenant")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return nil, wrapErr("list_users_by_tenant: token", err)
	}
	users, err := a.usersByTenantAttribute(ctx, tok, realmName, tenantID)
	if err != nil {
		// A 404 on this endpoint specifically means the realm itself is
		// absent — a realm that exists but matches zero users returns 200
		// with an empty list, never a 404 — so this is a reliable signal,
		// not ambiguous with "no matching users". Surfaced as a
		// distinguishable domain code (rather than the generic
		// ErrKeycloakUnavailable every other wrapErr call produces here)
		// so a caller like RevertConversion can tell "the realm is
		// genuinely gone — I must be resuming a retry whose earlier
		// attempt already deleted it" apart from a real outage.
		if isNotFound(err) {
			return nil, domain.NewError(domain.ErrTenantRealmNotFound, "keycloak realm not found").WithDetails(map[string]any{"realm": realmName})
		}
		return nil, wrapErr("list_users_by_tenant", err)
	}
	ids := make([]uuid.UUID, 0, len(users))
	for _, u := range users {
		if u.ID == nil {
			continue
		}
		id, err := uuid.Parse(*u.ID)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// DisableUsersInRealm disables every user tagged with tenantID's
// tenant_id attribute in realmName (used for a trial tenant's suspend,
// since the shared realm itself is never disabled).
func (a *Admin) DisableUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) (err error) {
	ctx, done := a.begin(ctx, "disable_users")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("disable_users: token", err)
	}
	users, err := a.usersByTenantAttribute(ctx, tok, realmName, tenantID)
	if err != nil {
		return wrapErr("disable_users: list", err)
	}
	for _, u := range users {
		u.Enabled = gocloak.BoolP(false)
		if err := a.client.UpdateUser(ctx, tok, realmName, *u); err != nil {
			return wrapErr("disable_users: update", err)
		}
	}
	return nil
}

// EnableUsersInRealm re-enables every user tagged with tenantID's
// tenant_id attribute in realmName.
func (a *Admin) EnableUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) (err error) {
	ctx, done := a.begin(ctx, "enable_users")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("enable_users: token", err)
	}
	users, err := a.usersByTenantAttribute(ctx, tok, realmName, tenantID)
	if err != nil {
		return wrapErr("enable_users: list", err)
	}
	for _, u := range users {
		u.Enabled = gocloak.BoolP(true)
		if err := a.client.UpdateUser(ctx, tok, realmName, *u); err != nil {
			return wrapErr("enable_users: update", err)
		}
	}
	return nil
}

// HardDeleteUsersInRealm hard-deletes every user carrying tenant_id
// (RP-C2 PII scrub, §15.2).
func (a *Admin) HardDeleteUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) (err error) {
	ctx, done := a.begin(ctx, "hard_delete_users")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("hard_delete_users: token", err)
	}
	users, err := a.usersByTenantAttribute(ctx, tok, realmName, tenantID)
	if err != nil {
		return wrapErr("hard_delete_users: list", err)
	}
	for _, u := range users {
		if u.ID == nil {
			continue
		}
		if err := a.client.DeleteUser(ctx, tok, realmName, *u.ID); err != nil && !isNotFound(err) {
			return wrapErr("hard_delete_users: delete", err)
		}
	}
	return nil
}

// DeleteUser is idempotent — deleting an absent user succeeds (RP-15).
func (a *Admin) DeleteUser(ctx context.Context, realmName string, keycloakUserID uuid.UUID) (err error) {
	ctx, done := a.begin(ctx, "delete_user")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("delete_user: token", err)
	}
	if err := a.client.DeleteUser(ctx, tok, realmName, keycloakUserID.String()); err != nil && !isNotFound(err) {
		return wrapErr("delete_user", err)
	}
	return nil
}

// RevokeUserSessions logs out and invalidates every session/token for
// keycloakUserID (RP-16). Idempotent — no active sessions is a no-op.
func (a *Admin) RevokeUserSessions(ctx context.Context, realmName string, keycloakUserID uuid.UUID) (err error) {
	ctx, done := a.begin(ctx, "revoke_sessions")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("revoke_sessions: token", err)
	}
	if err := a.client.LogoutAllSessions(ctx, tok, realmName, keycloakUserID.String()); err != nil && !isNotFound(err) {
		return wrapErr("revoke_sessions", err)
	}
	return nil
}

// ForceLogoutRealm terminates every active session in realmName (§8.3
// conversion — force-logout shared-realm sessions).
func (a *Admin) ForceLogoutRealm(ctx context.Context, realmName string) (err error) {
	ctx, done := a.begin(ctx, "force_logout_realm")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("force_logout_realm: token", err)
	}
	users, err := a.client.GetUsers(ctx, tok, realmName, gocloak.GetUsersParams{})
	if err != nil {
		return wrapErr("force_logout_realm: list_users", err)
	}
	for _, u := range users {
		if u.ID == nil {
			continue
		}
		if err := a.client.LogoutAllSessions(ctx, tok, realmName, *u.ID); err != nil && !isNotFound(err) {
			return wrapErr("force_logout_realm: logout", err)
		}
	}
	return nil
}
