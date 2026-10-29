package keycloak

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/Nerzal/gocloak/v13"
)

// ── tender-web client config (HLD §11.3, LLD §12.2) ───────────────────────

// WebClientConfig is the environment-specific half of the tender-web SPA
// client: the template carries its flow settings (public, Auth Code + PKCE
// S256, no implicit, no direct grants), this carries where it may redirect
// to. Every base URL contributes an exact "<base>/*" redirect and
// post-logout URI (the app's /t/<slug>/... paths and the emailed
// /t/<slug>/ links all sit under it) and its exact origin as a web origin —
// never a wildcard host and never "+".
type WebClientConfig struct {
	ClientID string   // "tender-web"
	BaseURLs []string // canonical WEB_APP_BASE_URL first, then WEB_APP_EXTRA_BASE_URLS
}

// NewWebClientConfig validates baseURL plus the comma-separated extra list
// (other origins the same realm must serve, e.g. a staging SPA host). A base
// URL must be an absolute https origin with no path, query or fragment;
// http is accepted only for localhost, for local development.
func NewWebClientConfig(clientID, baseURL, extra string) (WebClientConfig, error) {
	if clientID == "" {
		return WebClientConfig{}, fmt.Errorf("web client: client id is empty")
	}
	cfg := WebClientConfig{ClientID: clientID}
	for _, raw := range append([]string{baseURL}, strings.Split(extra, ",")...) {
		raw = strings.TrimRight(strings.TrimSpace(raw), "/")
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			return WebClientConfig{}, fmt.Errorf("web client: base URL %q: %w", raw, err)
		}
		local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
		if u.Host == "" || strings.Contains(u.Host, "*") || (u.Scheme != "https" && (u.Scheme != "http" || !local)) ||
			u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return WebClientConfig{}, fmt.Errorf("web client: base URL %q must be a bare https origin (scheme://host[:port])", raw)
		}
		if !slices.Contains(cfg.BaseURLs, raw) {
			cfg.BaseURLs = append(cfg.BaseURLs, raw)
		}
	}
	if len(cfg.BaseURLs) == 0 {
		return WebClientConfig{}, fmt.Errorf("web client: no base URL configured")
	}
	return cfg, nil
}

// WebClientConfigFromEnv reads the tender-web config cmd/server and
// cmd/reconciler share: WEB_CLIENT_ID (default tender-web),
// WEB_APP_BASE_URL (default https://app.bcbpsolutions.com — also the base of
// the emailed /t/<slug>/ links) and the optional comma-separated
// WEB_APP_EXTRA_BASE_URLS.
func WebClientConfigFromEnv() (WebClientConfig, error) {
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	return NewWebClientConfig(env("WEB_CLIENT_ID", "tender-web"),
		env("WEB_APP_BASE_URL", "https://app.bcbpsolutions.com"), os.Getenv("WEB_APP_EXTRA_BASE_URLS"))
}

func (w WebClientConfig) configured() bool { return w.ClientID != "" && len(w.BaseURLs) > 0 }

// apply sets c's redirect URIs, web origins and post-logout redirect URIs
// from the base URLs. Base URLs are already bare origins, so each one is
// its own web origin.
func (w WebClientConfig) apply(c *gocloak.Client) {
	redirects := make([]string, 0, len(w.BaseURLs))
	for _, b := range w.BaseURLs {
		redirects = append(redirects, b+"/*")
	}
	origins := slices.Clone(w.BaseURLs)
	c.RedirectURIs, c.WebOrigins = &redirects, &origins
	attrs := map[string]string{}
	if c.Attributes != nil {
		attrs = maps.Clone(*c.Attributes)
	}
	attrs["post.logout.redirect.uris"] = strings.Join(redirects, "##")
	c.Attributes = &attrs
}

// ── Shared trial realm: create-or-converge (LLD §8.1, §12.2) ────────────

// tenantIDMapperName names the protocol mapper that copies the tenant_id
// user attribute into the tenant_id claim (RP-INV-2, HLD §5.4).
const tenantIDMapperName = "tenant_id"

// tenantIDMapper is the shared realm's tenant_id mapper: access and ID
// tokens (AuthZ Enrichment reads the access token) plus introspection, not
// userinfo — nothing reads it there.
func tenantIDMapper() gocloak.ProtocolMapperRepresentation {
	return gocloak.ProtocolMapperRepresentation{
		Name:           gocloak.StringP(tenantIDMapperName),
		Protocol:       gocloak.StringP("openid-connect"),
		ProtocolMapper: gocloak.StringP("oidc-usermodel-attribute-mapper"),
		Config: &map[string]string{
			"user.attribute":            tenantIDAttribute,
			"claim.name":                tenantIDAttribute,
			"jsonType.label":            "String",
			"multivalued":               "false",
			"access.token.claim":        "true",
			"id.token.claim":            "true",
			"introspection.token.claim": "true",
			"userinfo.token.claim":      "false",
		},
	}
}

// EnsureSharedRealm creates the shared trial realm from the template when
// it is absent, then converges it (RP-INV-2): requires-mfa role, user
// profile, token settings, the tender-web client, and the tenant_id mapper
// on every public client. A concurrent create by another replica (409) is
// not an error — the realm exists, so it is converged like any other.
func (a *Admin) EnsureSharedRealm(ctx context.Context, realmName string) (err error) {
	ctx, done := a.begin(ctx, "ensure_shared_realm")
	defer func() { done(err) }()

	if !a.cfg.Web.configured() {
		return fmt.Errorf("ensure_shared_realm: web client config is not set")
	}
	tok, err := a.token(ctx)
	if err != nil {
		return wrapErr("ensure_shared_realm: token", err)
	}
	_, err = a.client.GetRealm(ctx, tok, realmName)
	switch {
	case err == nil:
	case !isNotFound(err):
		return wrapErr("ensure_shared_realm: get_realm", err)
	default:
		created, cerr := a.createRealm(ctx, tok, realmName)
		switch {
		case cerr == nil:
			tok = created
		case !isConflict(cerr):
			return cerr
		default:
			// Lost a create race to another replica: re-mint so the token
			// carries the admin grant on the realm it just created.
			a.tokens.Invalidate()
			if tok, err = a.token(ctx); err != nil {
				return wrapErr("ensure_shared_realm: token", err)
			}
		}
	}
	return a.convergeSharedRealm(ctx, tok, realmName)
}

// convergeSharedRealm applies the template-owned settings to the existing
// shared realm. Each step is idempotent and touches only the fields this
// service owns.
func (a *Admin) convergeSharedRealm(ctx context.Context, tok, realmName string) error {
	if err := a.initRealm(ctx, tok, realmName); err != nil {
		return err
	}
	tmpl, err := loadRealmTemplate()
	if err != nil {
		return fmt.Errorf("keycloak: load realm template: %w", err)
	}
	web := templateClient(tmpl, a.cfg.Web.ClientID)
	if web == nil {
		return fmt.Errorf("keycloak: realm template has no %q client", a.cfg.Web.ClientID)
	}
	a.cfg.Web.apply(web)
	if err := a.ensureClient(ctx, tok, realmName, *web); err != nil {
		return wrapErr("converge_realm: web_client", err)
	}
	if tmpl.Clients == nil {
		return nil
	}
	for _, c := range *tmpl.Clients {
		if c.ClientID == nil || c.PublicClient == nil || !*c.PublicClient {
			continue
		}
		if err := a.ensureTenantIDMapper(ctx, tok, realmName, *c.ClientID); err != nil {
			return wrapErr("converge_realm: tenant_id_mapper", err)
		}
	}
	return nil
}

// templateClient returns a copy of the template's clientID client, or nil.
func templateClient(tmpl gocloak.RealmRepresentation, clientID string) *gocloak.Client {
	if tmpl.Clients == nil {
		return nil
	}
	for _, c := range *tmpl.Clients {
		if c.ClientID != nil && *c.ClientID == clientID {
			return &c
		}
	}
	return nil
}

// ensureClient creates want in realmName when absent; otherwise it writes
// want's flow, redirect and attribute settings onto the existing client,
// leaving every field want does not set (name, mappers, scopes, ...) as is.
func (a *Admin) ensureClient(ctx context.Context, tok, realmName string, want gocloak.Client) error {
	existing, err := a.findClientByID(ctx, tok, realmName, *want.ClientID)
	if err != nil {
		return fmt.Errorf("get_clients: %w", err)
	}
	if existing == nil || existing.ID == nil {
		if _, err := a.client.CreateClient(ctx, tok, realmName, want); err != nil {
			return fmt.Errorf("create_client: %w", err)
		}
		return nil
	}
	full, err := a.client.GetClient(ctx, tok, realmName, *existing.ID)
	if err != nil {
		return fmt.Errorf("get_client: %w", err)
	}
	for _, f := range []struct{ dst, src **bool }{
		{&full.PublicClient, &want.PublicClient},
		{&full.StandardFlowEnabled, &want.StandardFlowEnabled},
		{&full.ImplicitFlowEnabled, &want.ImplicitFlowEnabled},
		{&full.DirectAccessGrantsEnabled, &want.DirectAccessGrantsEnabled},
		{&full.ServiceAccountsEnabled, &want.ServiceAccountsEnabled},
	} {
		if *f.src != nil {
			*f.dst = *f.src
		}
	}
	if want.RedirectURIs != nil {
		full.RedirectURIs = want.RedirectURIs
	}
	if want.WebOrigins != nil {
		full.WebOrigins = want.WebOrigins
	}
	if want.Attributes != nil {
		attrs := map[string]string{}
		if full.Attributes != nil {
			attrs = maps.Clone(*full.Attributes)
		}
		maps.Copy(attrs, *want.Attributes)
		full.Attributes = &attrs
	}
	if err := a.client.UpdateClient(ctx, tok, realmName, *full); err != nil {
		return fmt.Errorf("update_client: %w", err)
	}
	return nil
}

// ensureTenantIDMapper adds the tenant_id mapper to clientID, or rewrites
// it when its config has drifted. A client the realm does not have is
// skipped: it cannot issue tokens, so it needs no claim.
func (a *Admin) ensureTenantIDMapper(ctx context.Context, tok, realmName, clientID string) error {
	existing, err := a.findClientByID(ctx, tok, realmName, clientID)
	if err != nil {
		return fmt.Errorf("get_clients: %w", err)
	}
	if existing == nil || existing.ID == nil {
		a.log.Warn("tenant_id mapper: client absent, skipped", map[string]any{"realm": realmName, "client_id": clientID})
		return nil
	}
	full, err := a.client.GetClient(ctx, tok, realmName, *existing.ID)
	if err != nil {
		return fmt.Errorf("get_client: %w", err)
	}
	want := tenantIDMapper()
	if full.ProtocolMappers != nil {
		for _, m := range *full.ProtocolMappers {
			if m.Name == nil || *m.Name != tenantIDMapperName {
				continue
			}
			if m.ProtocolMapper != nil && *m.ProtocolMapper == *want.ProtocolMapper && m.Config != nil && maps.Equal(*m.Config, *want.Config) {
				return nil
			}
			want.ID = m.ID
			if err := a.client.UpdateClientProtocolMapper(ctx, tok, realmName, *existing.ID, *m.ID, want); err != nil {
				return fmt.Errorf("update_protocol_mapper: %w", err)
			}
			return nil
		}
	}
	if _, err := a.client.CreateClientProtocolMapper(ctx, tok, realmName, *existing.ID, want); err != nil && !isConflict(err) {
		return fmt.Errorf("create_protocol_mapper: %w", err)
	}
	return nil
}

// ── Realm settings the gocloak representation cannot carry ───────────────

// realmTokenSettings are the template's session and token settings (HLD
// §11.3). gocloak's RealmRepresentation has no clientSession* fields, so
// they are read from the raw template and written with a partial realm
// PUT (Keycloak updates only the fields present).
type realmTokenSettings struct {
	AccessTokenLifespan      *int  `json:"accessTokenLifespan,omitempty"`
	SsoSessionIdleTimeout    *int  `json:"ssoSessionIdleTimeout,omitempty"`
	SsoSessionMaxLifespan    *int  `json:"ssoSessionMaxLifespan,omitempty"`
	ClientSessionIdleTimeout *int  `json:"clientSessionIdleTimeout,omitempty"`
	ClientSessionMaxLifespan *int  `json:"clientSessionMaxLifespan,omitempty"`
	RevokeRefreshToken       *bool `json:"revokeRefreshToken,omitempty"`
	RefreshTokenMaxReuse     *int  `json:"refreshTokenMaxReuse,omitempty"`
}

// applyRealmTokenSettings writes the template's token settings onto
// realmName. A template that sets none of them (a test template) is a
// no-op.
func (a *Admin) applyRealmTokenSettings(ctx context.Context, tok, realmName string) error {
	raw, err := readRealmTemplate()
	if err != nil {
		return fmt.Errorf("read realm template: %w", err)
	}
	var s realmTokenSettings
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("decode realm template: %w", err)
	}
	if s == (realmTokenSettings{}) {
		return nil
	}
	resp, err := a.client.GetRequestWithBearerAuth(ctx, tok).SetBody(s).Put(a.cfg.BaseURL + "/admin/realms/" + realmName)
	if err != nil {
		return err
	}
	if resp.IsError() {
		return fmt.Errorf("update realm: unexpected status %d: %s", resp.StatusCode(), resp.String())
	}
	return nil
}

// tenantIDProfileAttribute declares tenant_id in the user profile as
// admin-only: an undeclared attribute is user-editable under the ENABLED
// unmanaged policy, and a user who could edit tenant_id in the account
// console could move themselves into another trial tenant (HLD §5.4).
func tenantIDProfileAttribute() map[string]any {
	return map[string]any{
		"name":        tenantIDAttribute,
		"displayName": "Tenant ID",
		"multivalued": false,
		"permissions": map[string]any{"view": []any{"admin"}, "edit": []any{"admin"}},
	}
}

// applyUserProfile (Keycloak 24+ declarative user profile, the
// /users/profile endpoint) keeps unmanagedAttributePolicy=ENABLED, so
// other custom attributes persist instead of being silently dropped, and
// declares tenant_id admin-view/admin-edit only (tenantIDProfileAttribute),
// replacing any earlier tenant_id declaration.
func (a *Admin) applyUserProfile(ctx context.Context, tok, realmName string) error {
	profileURL := a.cfg.BaseURL + "/admin/realms/" + realmName + "/users/profile"
	getResp, err := a.client.GetRequestWithBearerAuth(ctx, tok).Get(profileURL)
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
	if profile == nil {
		profile = map[string]any{}
	}
	profile["unmanagedAttributePolicy"] = "ENABLED"
	var attrs []any
	if v, ok := profile["attributes"].([]any); ok {
		attrs = v
	}
	attrs = slices.DeleteFunc(attrs, func(v any) bool {
		m, ok := v.(map[string]any)
		return ok && m["name"] == tenantIDAttribute
	})
	profile["attributes"] = append(attrs, tenantIDProfileAttribute())
	putResp, err := a.client.GetRequestWithBearerAuth(ctx, tok).SetBody(profile).Put(profileURL)
	if err != nil {
		return err
	}
	if putResp.IsError() {
		return fmt.Errorf("update user profile: unexpected status %d: %s", putResp.StatusCode(), putResp.String())
	}
	return nil
}
