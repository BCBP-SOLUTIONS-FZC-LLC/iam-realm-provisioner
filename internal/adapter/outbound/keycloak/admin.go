// Package keycloak implements port.KeycloakAdmin (RP-INV-1) — the ONLY
// package in this service, and the only service in the platform, permitted
// to hold a Keycloak admin credential or call gocloak. CI greps enforce
// that no other package imports gocloak.
package keycloak

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Nerzal/gocloak/v13"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// RequiresMFARoleName is the frozen Keycloak realm-role name the
// conditional authenticator checks to force MFA enrollment (§6.5, §8.6).
const RequiresMFARoleName = "requires-mfa"

// Config configures the gocloak-backed KeycloakAdmin adapter (§12).
type Config struct {
	BaseURL      string
	AdminRealm   string // "master"
	ClientID     string // "iam-realm-provisioner"
	ClientSecret string
	RateLimitRPS int
	Timeout      time.Duration
}

// Admin implements port.KeycloakAdmin.
type Admin struct {
	client *gocloak.GoCloak
	tokens *tokenCache
	cfg    Config
	log    port.Logger
	limit  *rateLimiter
}

var _ port.KeycloakAdmin = (*Admin)(nil)

// New constructs the sole Keycloak Admin API client.
func New(cfg Config, log port.Logger) *Admin {
	if cfg.Timeout == 0 {
		cfg.Timeout = 8 * time.Second
	}
	if cfg.RateLimitRPS == 0 {
		cfg.RateLimitRPS = 20
	}
	client := gocloak.NewClient(cfg.BaseURL)
	return &Admin{
		client: client,
		tokens: newTokenCache(client, cfg.AdminRealm, cfg.ClientID, cfg.ClientSecret),
		cfg:    cfg,
		log:    log,
		limit:  newRateLimiter(cfg.RateLimitRPS),
	}
}

// token returns a cached admin token, throttled by the client-side rate
// limiter (§9.4).
func (a *Admin) token(ctx context.Context) (string, error) {
	a.limit.Wait(ctx)
	return a.tokens.Token(ctx)
}

// Ready mints (or reuses the cached) admin token, used by /readyz (§3.1).
func (a *Admin) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	_, err := a.token(ctx)
	if err != nil {
		return domain.NewError(domain.ErrKeycloakUnavailable, "keycloak admin token bootstrap failed").WithDetails(map[string]any{"error": err.Error()})
	}
	return nil
}

// wrapErr classifies a gocloak error as keycloak_unavailable — the typed
// domain error flows so callers/consumers can distinguish a retryable
// transport failure (§9.4, §17).
func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return domain.NewError(domain.ErrKeycloakUnavailable, fmt.Sprintf("keycloak %s failed", op)).WithDetails(map[string]any{"error": err.Error()})
}

//go:embed realm-template.json
var embeddedTemplateFS embed.FS

// loadRealmTemplate reads deploy/keycloak/realm-template.json when present
// on disk (deploy-time mount, RP-3/§12.2), falling back to the minimal
// embedded default so the service also boots in local dev without that
// mount.
func loadRealmTemplate() (gocloak.RealmRepresentation, error) {
	var raw []byte
	var err error
	if p := os.Getenv("REALM_TEMPLATE_PATH"); p != "" {
		raw, err = os.ReadFile(filepath.Clean(p))
	}
	if len(raw) == 0 {
		raw, err = embeddedTemplateFS.ReadFile("realm-template.json")
	}
	if err != nil {
		return gocloak.RealmRepresentation{}, err
	}
	var rep gocloak.RealmRepresentation
	if err := json.Unmarshal(raw, &rep); err != nil {
		return gocloak.RealmRepresentation{}, err
	}
	return rep, nil
}

// CreateRealmFromTemplate clones the versioned realm template into a new
// realm named realmName (RP-3, §12.2).
func (a *Admin) CreateRealmFromTemplate(ctx context.Context, realmName, _ string) error {
	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("create_realm: token", err)
	}
	rep, err := loadRealmTemplate()
	if err != nil {
		return fmt.Errorf("keycloak: load realm template: %w", err)
	}
	rep.Realm = gocloak.StringP(realmName)
	rep.Enabled = gocloak.BoolP(true)
	rep.ID = gocloak.StringP(realmName)
	if _, err := a.client.CreateRealm(ctx, tok, rep); err != nil {
		if isConflict(err) {
			return domain.NewError(domain.ErrRealmAlreadyDedicated, "realm already exists").WithDetails(map[string]any{"realm": realmName})
		}
		return wrapErr("create_realm", err)
	}
	// Keycloak grants the create-realm principal full admin on realms it
	// creates (§10.2), but that grant is a role-mapping added to the
	// service account AFTER this call returns — a token minted before the
	// realm existed does not carry it and gets 403 on the realm's own
	// admin endpoints (confirmed empirically against a live Keycloak).
	// Invalidate and re-mint so every subsequent call in this
	// provisioning flow (this one and the caller's own follow-on calls,
	// which share the same cache) uses a token issued after the grant.
	a.tokens.Invalidate()
	tok, err = a.token(ctx)
	if err != nil {
		return wrapErr("create_realm: refresh token post-create", err)
	}
	// requires-mfa realm role must exist on every dedicated realm for §8.6
	// to have something to assign.
	if _, err := a.client.CreateRealmRole(ctx, tok, realmName, gocloak.Role{Name: gocloak.StringP(RequiresMFARoleName)}); err != nil && !isConflict(err) {
		return wrapErr("create_realm_role", err)
	}
	// Keycloak's declarative User Profile (24+) rejects any user attribute
	// not explicitly declared in its schema by default — confirmed
	// empirically that the tenant_id attribute this service stamps on
	// every user (RP-INV-2's entire mechanism for JWT-claim injection via
	// a protocol mapper) is SILENTLY DROPPED on create without this.
	// gocloak v13 has no wrapper for the user-profile endpoint, so this
	// uses the resty escape hatch directly, same as partialImportUser.
	if err := a.enableUnmanagedAttributes(ctx, tok, realmName); err != nil {
		return wrapErr("create_realm: enable_unmanaged_attributes", err)
	}
	return nil
}

// enableUnmanagedAttributes sets unmanagedAttributePolicy=ENABLED on
// realmName's User Profile config so custom attributes (tenant_id) persist
// on user create/update instead of being silently dropped.
func (a *Admin) enableUnmanagedAttributes(ctx context.Context, tok, realmName string) error {
	url := a.cfg.BaseURL + "/admin/realms/" + realmName + "/users/profile"
	getResp, err := a.client.GetRequestWithBearerAuth(ctx, tok).Get(url)
	if err != nil {
		return err
	}
	if getResp.IsError() {
		return fmt.Errorf("get user profile: unexpected status %d: %s", getResp.StatusCode(), getResp.String())
	}
	var profile map[string]any
	if err := json.Unmarshal(getResp.Body(), &profile); err != nil {
		return fmt.Errorf("decode user profile: %w", err)
	}
	profile["unmanagedAttributePolicy"] = "ENABLED"
	putResp, err := a.client.GetRequestWithBearerAuth(ctx, tok).SetBody(profile).Put(url)
	if err != nil {
		return err
	}
	if putResp.IsError() {
		return fmt.Errorf("update user profile: unexpected status %d: %s", putResp.StatusCode(), putResp.String())
	}
	return nil
}

// RegisterOIDCClients registers the standard OIDC client set (already
// present on the cloned template, per-tenant redirect URIs scoped to the
// tenant slug) and returns their client IDs.
func (a *Admin) RegisterOIDCClients(ctx context.Context, realmName, _ string) ([]string, error) {
	tok, err := a.token(ctx)
	if err != nil {
		return nil, wrapErr("register_oidc_clients: token", err)
	}
	clients, err := a.client.GetClients(ctx, tok, realmName, gocloak.GetClientsParams{})
	if err != nil {
		return nil, wrapErr("get_clients", err)
	}
	builtin := map[string]bool{"realm-management": true, "account": true, "account-console": true,
		"broker": true, "security-admin-console": true, "admin-cli": true}
	var ids []string
	for _, c := range clients {
		if c.ClientID == nil || builtin[*c.ClientID] {
			continue
		}
		ids = append(ids, *c.ClientID)
	}
	return ids, nil
}

// DisableRealm sets realmName's Enabled flag to false, blocking all logins.
func (a *Admin) DisableRealm(ctx context.Context, realmName string) error {
	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("disable_realm: token", err)
	}
	if err := a.client.UpdateRealm(ctx, tok, gocloak.RealmRepresentation{Realm: &realmName, Enabled: gocloak.BoolP(false)}); err != nil {
		return wrapErr("disable_realm", err)
	}
	return nil
}

// EnableRealm sets realmName's Enabled flag to true, restoring logins.
func (a *Admin) EnableRealm(ctx context.Context, realmName string) error {
	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("enable_realm: token", err)
	}
	if err := a.client.UpdateRealm(ctx, tok, gocloak.RealmRepresentation{Realm: &realmName, Enabled: gocloak.BoolP(true)}); err != nil {
		return wrapErr("enable_realm", err)
	}
	return nil
}

// ExportRealm builds a JSON snapshot of the realm representation plus every
// user (including credentials) for offboarding (RP-EVT-4, §15.2). gocloak
// v13 has no wrapped `partial-export` endpoint, so this assembles an
// equivalent document from GetRealm + GetUsers — sufficient for the
// compliance-retention/DR-restore use case this export exists for.
func (a *Admin) ExportRealm(ctx context.Context, realmName string) ([]byte, error) {
	tok, err := a.token(ctx)
	if err != nil {
		return nil, wrapErr("export_realm: token", err)
	}
	realm, err := a.client.GetRealm(ctx, tok, realmName)
	if err != nil {
		return nil, wrapErr("export_realm: get_realm", err)
	}
	users, err := a.client.GetUsers(ctx, tok, realmName, gocloak.GetUsersParams{})
	if err != nil {
		return nil, wrapErr("export_realm: get_users", err)
	}
	for _, u := range users {
		if u.ID == nil {
			continue
		}
		creds, err := a.client.GetCredentials(ctx, tok, realmName, *u.ID)
		if err != nil {
			return nil, wrapErr("export_realm: get_credentials", err)
		}
		cr := make([]gocloak.CredentialRepresentation, 0, len(creds))
		for _, c := range creds {
			cr = append(cr, *c)
		}
		u.Credentials = &cr
	}
	idps, err := a.client.GetIdentityProviders(ctx, tok, realmName)
	if err != nil {
		return nil, wrapErr("export_realm: get_idps", err)
	}
	doc := struct {
		Realm      *gocloak.RealmRepresentation              `json:"realm"`
		Users      []*gocloak.User                           `json:"users"`
		Providers  []*gocloak.IdentityProviderRepresentation `json:"identity_providers"`
		ExportedAt time.Time                                 `json:"exported_at"`
	}{Realm: realm, Users: users, Providers: idps, ExportedAt: time.Now().UTC()}
	return json.Marshal(doc)
}

// DeleteRealm permanently deletes realmName.
func (a *Admin) DeleteRealm(ctx context.Context, realmName string) error {
	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("delete_realm: token", err)
	}
	if err := a.client.DeleteRealm(ctx, tok, realmName); err != nil {
		return wrapErr("delete_realm", err)
	}
	return nil
}

// DeleteRealmIfExists is the idempotent compensating-teardown primitive
// (§9.3) — deleting an absent realm succeeds.
func (a *Admin) DeleteRealmIfExists(ctx context.Context, realmName string) error {
	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("delete_realm_if_exists: token", err)
	}
	if err := a.client.DeleteRealm(ctx, tok, realmName); err != nil {
		if isNotFound(err) {
			return nil
		}
		return wrapErr("delete_realm_if_exists", err)
	}
	return nil
}

// isConflict/isNotFound classify gocloak's *APIError by HTTP status —
// gocloak surfaces the Keycloak REST response status verbatim.
func isConflict(err error) bool { return apiErrStatus(err) == 409 }
func isNotFound(err error) bool { return apiErrStatus(err) == 404 }

func apiErrStatus(err error) int {
	if apiErr, ok := err.(gocloak.APIError); ok {
		return apiErr.Code
	}
	if apiErr, ok := err.(*gocloak.APIError); ok {
		return apiErr.Code
	}
	return 0
}
