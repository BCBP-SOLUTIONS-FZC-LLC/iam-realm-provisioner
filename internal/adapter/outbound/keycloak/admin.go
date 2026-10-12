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
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
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
	// SecretProvider, when set, overrides ClientSecret: the returned string is
	// fetched at every token refresh, enabling live rotation without a pod
	// restart (e.g. by reading from OpenBao instead of a static env-var copy).
	// When nil, ClientSecret is used as a static value — the pre-existing
	// behavior and what tests set directly.
	SecretProvider func(ctx context.Context) (string, error)
	RateLimitRPS   int
	Timeout        time.Duration
}

// Admin implements port.KeycloakAdmin.
type Admin struct {
	client *gocloak.GoCloak
	tokens *tokenCache
	cfg    Config
	log    port.Logger
	tracer gincommon.Tracer
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
	secretFn := cfg.SecretProvider
	if secretFn == nil {
		// Static fallback — wraps the value captured at construction time.
		// Use Config.SecretProvider for live rotation without a pod restart.
		s := cfg.ClientSecret
		secretFn = func(_ context.Context) (string, error) {
			if s == "" {
				return "", fmt.Errorf("keycloak: ClientSecret is not configured")
			}
			return s, nil
		}
	}
	tokens := newTokenCache(client, cfg.AdminRealm, cfg.ClientID, secretFn)
	limiter := newRateLimiter(cfg.RateLimitRPS)

	// gocloak drives resty, which owns its own http.Client — so the
	// service-wide instrumented transport has to be installed here rather
	// than passed in, or Keycloak calls would carry no traceparent (§11.3).
	// rateLimitTransport sits UNDER retryTransport (its base) so every
	// actual attempt — including retries — is throttled, not just the
	// first one per outer Admin method call.
	rc := client.RestyClient()
	base := rc.GetClient().Transport
	if base == nil {
		base = http.DefaultTransport
	}
	rc.SetTransport(retryTransport{base: rateLimitTransport{limiter: limiter, base: httpx.NewTransport(unauthorizedTransport{base: base, tokens: tokens})}})

	return &Admin{
		client: client,
		tokens: tokens,
		cfg:    cfg,
		log:    log,
		tracer: gincommon.NewTracer(tracerName),
	}
}

// token returns a cached admin token. Rate limiting happens per actual
// outbound HTTP request inside the transport chain (rateLimitTransport),
// not here — a token-cache hit makes no network call and correctly
// consumes no rate-limit budget.
func (a *Admin) token(ctx context.Context) (string, error) {
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
func (a *Admin) CreateRealmFromTemplate(ctx context.Context, realmName, _ string) (err error) {
	ctx, done := a.begin(ctx, "create_realm")
	defer func() { done(err) }()

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
	// Deliberately NOT setting rep.ID: Keycloak's REALM.ID column is a
	// VARCHAR(36) sized for a bare UUID, but realmName (the logical/URL
	// realm name, used only for rep.Realm) can run longer — RP-3's
	// "tenant-"+<36-char tenant UUID> is 43 characters and would overflow
	// it, and Keycloak wraps the resulting SQLException as a generic 409
	// "Conflict detected" rather than a clear error, so the conflict
	// branch below would silently treat a never-created realm as an
	// existing one. Leaving ID unset lets Keycloak assign its own
	// UUID primary key, independent of realmName's length.
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
func (a *Admin) RegisterOIDCClients(ctx context.Context, realmName, _ string) (clientIDs []string, err error) {
	ctx, done := a.begin(ctx, "register_oidc_clients")
	defer func() { done(err) }()

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
	ids := []string{}
	for _, c := range clients {
		if c.ClientID == nil || builtin[*c.ClientID] {
			continue
		}
		ids = append(ids, *c.ClientID)
	}
	return ids, nil
}

// DisableRealm sets realmName's Enabled flag to false, blocking all logins.
func (a *Admin) DisableRealm(ctx context.Context, realmName string) (err error) {
	ctx, done := a.begin(ctx, "disable_realm")
	defer func() { done(err) }()

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
func (a *Admin) EnableRealm(ctx context.Context, realmName string) (err error) {
	ctx, done := a.begin(ctx, "enable_realm")
	defer func() { done(err) }()

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
func (a *Admin) ExportRealm(ctx context.Context, realmName string) (export []byte, err error) {
	ctx, done := a.begin(ctx, "export_realm")
	defer func() { done(err) }()

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
func (a *Admin) DeleteRealm(ctx context.Context, realmName string) (err error) {
	ctx, done := a.begin(ctx, "delete_realm")
	defer func() { done(err) }()

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
func (a *Admin) DeleteRealmIfExists(ctx context.Context, realmName string) (err error) {
	ctx, done := a.begin(ctx, "delete_realm_if_exists")
	defer func() { done(err) }()

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

// findClientByID looks up realmName's client by its exact clientId
// (gocloak's ClientID query param may substring-match server-side, so this
// filters client-side for an exact match — the same defensive style
// RegisterOIDCClients already uses to filter the builtin set).
func (a *Admin) findClientByID(ctx context.Context, tok, realmName, clientID string) (*gocloak.Client, error) {
	clients, err := a.client.GetClients(ctx, tok, realmName, gocloak.GetClientsParams{ClientID: gocloak.StringP(clientID)})
	if err != nil {
		return nil, err
	}
	for _, c := range clients {
		if c.ClientID != nil && *c.ClientID == clientID {
			return c, nil
		}
	}
	return nil, nil
}

// CreateServiceAccountClient idempotently mints a confidential,
// service-account-enabled client (clientID) in realmName — the
// platform-automation principal (§2.5, RP-17) — authenticated via
// Keycloak's client-jwt (private_key_jwt) authenticator against the JWKS
// at jwksURL (EXT-6), and returns the Keycloak service-account user's sub.
// created reports whether this call minted a brand new client (true) or
// resumed one that already existed (false); a resumed client also has its
// auth config re-applied (ensureClientJWTAuth) so a drifted/stale existing
// client is corrected rather than silently left on the wrong mechanism.
func (a *Admin) CreateServiceAccountClient(ctx context.Context, realmName, clientID, jwksURL string) (sub uuid.UUID, created bool, err error) {
	ctx, done := a.begin(ctx, "create_service_account_client")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return uuid.Nil, false, wrapErr("create_service_account_client: token", err)
	}

	existing, err := a.findClientByID(ctx, tok, realmName, clientID)
	if err != nil {
		return uuid.Nil, false, wrapErr("create_service_account_client: get_clients", err)
	}

	var internalID string
	if existing != nil {
		// Not a Keycloak-unavailable condition (retrying changes nothing) —
		// a permanently inconsistent resource state, so a plain error
		// (-> 500 internal_error) rather than domain.ErrKeycloakUnavailable
		// (-> 502, which implies retry will help).
		if existing.ID == nil {
			return uuid.Nil, false, fmt.Errorf("create_service_account_client: existing client %q in realm %q has no internal id", clientID, realmName)
		}
		internalID = *existing.ID
	} else {
		internalID, err = a.client.CreateClient(ctx, tok, realmName, gocloak.Client{
			ClientID:                  gocloak.StringP(clientID),
			Protocol:                  gocloak.StringP("openid-connect"),
			PublicClient:              gocloak.BoolP(false),
			StandardFlowEnabled:       gocloak.BoolP(false),
			DirectAccessGrantsEnabled: gocloak.BoolP(false),
			ServiceAccountsEnabled:    gocloak.BoolP(true),
			Enabled:                   gocloak.BoolP(true),
			ClientAuthenticatorType:   gocloak.StringP("client-jwt"),
			Attributes: &map[string]string{
				"use.jwks.url": "true",
				"jwks.url":     jwksURL,
			},
		})
		if err != nil {
			if isConflict(err) {
				// Lost-response retry of our own create — resume by
				// looking the client back up rather than erroring. Treated
				// like the found-up-front branch above: not "created" by
				// this call.
				resumed, findErr := a.findClientByID(ctx, tok, realmName, clientID)
				if findErr != nil || resumed == nil || resumed.ID == nil {
					return uuid.Nil, false, wrapErr("create_service_account_client: resume_after_conflict", err)
				}
				internalID = *resumed.ID
			} else {
				return uuid.Nil, false, wrapErr("create_service_account_client", err)
			}
		} else {
			created = true
		}
	}

	// A resumed client (either found up front or recovered after our own
	// lost-response conflict) predates this call and may have drifted from
	// the expected auth config — re-applied once here rather than at each
	// of the two resume branches above, so there is a single place this
	// logic can change. A freshly-created client above already has it.
	if !created {
		if err := a.ensureClientJWTAuth(ctx, tok, realmName, internalID, jwksURL); err != nil {
			return uuid.Nil, false, wrapErr("create_service_account_client: reapply_auth_config", err)
		}
	}

	svcUser, err := a.client.GetClientServiceAccount(ctx, tok, realmName, internalID)
	if err != nil {
		return uuid.Nil, false, wrapErr("create_service_account_client: get_service_account", err)
	}
	if svcUser == nil || svcUser.ID == nil {
		return uuid.Nil, false, fmt.Errorf("create_service_account_client: service-account client %q in realm %q has no linked user", clientID, realmName)
	}
	subID, err := uuid.Parse(*svcUser.ID)
	if err != nil {
		return uuid.Nil, false, wrapErr("create_service_account_client: parse_sub", err)
	}
	return subID, created, nil
}

// ensureClientJWTAuth (re-)applies the client-jwt authenticator and JWKS
// attributes to an already-existing client found by CreateServiceAccountClient's
// resume path — CreateClient only sets these once, at creation, so a
// resumed client whose config had drifted (an out-of-band edit, a stale
// pre-EXT-6 client) would otherwise silently keep serving on the wrong auth
// mechanism with no error. Fetches the full representation first and PUTs
// it back with only the two fields changed: Keycloak's client update
// replaces the fields supplied, so writing a partial representation risks
// clobbering unrelated config (name, redirect URIs, ...) this service
// doesn't own. Skips the write entirely when already correctly configured.
func (a *Admin) ensureClientJWTAuth(ctx context.Context, tok, realmName, internalID, jwksURL string) error {
	// gocloak's GetClient always returns a non-nil *Client when err is nil
	// (verified against the vendored v13.9.0 source), so there is no
	// full == nil case to guard here.
	full, err := a.client.GetClient(ctx, tok, realmName, internalID)
	if err != nil {
		return fmt.Errorf("get_client: %w", err)
	}
	if full.ClientAuthenticatorType != nil && *full.ClientAuthenticatorType == "client-jwt" &&
		full.Attributes != nil && (*full.Attributes)["use.jwks.url"] == "true" && (*full.Attributes)["jwks.url"] == jwksURL {
		return nil
	}
	full.ClientAuthenticatorType = gocloak.StringP("client-jwt")
	if full.Attributes == nil {
		full.Attributes = &map[string]string{}
	}
	(*full.Attributes)["use.jwks.url"] = "true"
	(*full.Attributes)["jwks.url"] = jwksURL
	if err := a.client.UpdateClient(ctx, tok, realmName, *full); err != nil {
		return fmt.Errorf("update_client: %w", err)
	}
	return nil
}

// ClearServiceAccountKeysCache evicts Keycloak's cached JWKS keys for
// realmName (EXT-6, RP-17) — realm-scoped, not client-scoped. Called after
// every mint/rotate/revoke of a platform-automation credential so Keycloak
// actually notices the change; Keycloak does not self-refresh a
// jwks.url client's keys otherwise (verified empirically).
func (a *Admin) ClearServiceAccountKeysCache(ctx context.Context, realmName string) (err error) {
	ctx, done := a.begin(ctx, "clear_service_account_keys_cache")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("clear_service_account_keys_cache: token", err)
	}
	if err := a.client.ClearKeysCache(ctx, tok, realmName); err != nil {
		return wrapErr("clear_service_account_keys_cache", err)
	}
	return nil
}

// DeleteServiceAccountClientIfExists deletes clientID's client in realmName
// — compensating cleanup for a partial mint failure or a stale client left
// behind by RP-3 conversion (§2.5, §9.3). Idempotent: deleting an absent
// client succeeds, mirroring DeleteRealmIfExists.
func (a *Admin) DeleteServiceAccountClientIfExists(ctx context.Context, realmName, clientID string) (err error) {
	ctx, done := a.begin(ctx, "delete_service_account_client_if_exists")
	defer func() { done(err) }()

	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("delete_service_account_client_if_exists: token", err)
	}
	existing, err := a.findClientByID(ctx, tok, realmName, clientID)
	if err != nil {
		return wrapErr("delete_service_account_client_if_exists: get_clients", err)
	}
	if existing == nil || existing.ID == nil {
		return nil
	}
	if err := a.client.DeleteClient(ctx, tok, realmName, *existing.ID); err != nil {
		if isNotFound(err) {
			return nil
		}
		return wrapErr("delete_service_account_client_if_exists", err)
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
