package keycloak

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Nerzal/gocloak/v13"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- WebClientConfig (tender-web hardening, HLD §11.3) ---

func TestNewWebClientConfig_BaseAndExtras(t *testing.T) {
	cfg, err := NewWebClientConfig("tender-web", "https://app.example/", " https://staging.example , ,https://app.example,http://localhost:5173")
	require.NoError(t, err)
	assert.Equal(t, "tender-web", cfg.ClientID)
	assert.Equal(t, []string{"https://app.example", "https://staging.example", "http://localhost:5173"}, cfg.BaseURLs,
		"trimmed, de-duplicated, canonical base first")
}

func TestNewWebClientConfig_Rejects(t *testing.T) {
	for name, tc := range map[string]struct{ clientID, base, extra string }{
		"empty client id":       {"", "https://app.example", ""},
		"no base URL":           {"tender-web", "", " , "},
		"path":                  {"tender-web", "https://app.example/t/acme", ""},
		"query":                 {"tender-web", "https://app.example?x=1", ""},
		"plain http non-local":  {"tender-web", "http://app.example", ""},
		"wildcard host":         {"tender-web", "https://*.example", ""},
		"relative":              {"tender-web", "app.example", ""},
		"bad extra":             {"tender-web", "https://app.example", "ftp://files.example"},
		"userinfo":              {"tender-web", "https://u:p@app.example", ""},
		"fragment":              {"tender-web", "https://app.example#x", ""},
		"unparseable extra url": {"tender-web", "https://app.example", "https://bad host"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewWebClientConfig(tc.clientID, tc.base, tc.extra)
			require.Error(t, err)
		})
	}
}

func TestWebClientConfigFromEnv(t *testing.T) {
	t.Setenv("WEB_CLIENT_ID", "")
	t.Setenv("WEB_APP_BASE_URL", "")
	t.Setenv("WEB_APP_EXTRA_BASE_URLS", "")
	cfg, err := WebClientConfigFromEnv()
	require.NoError(t, err)
	assert.Equal(t, WebClientConfig{ClientID: "tender-web", BaseURLs: []string{"https://app.bcbpsolutions.com"}}, cfg, "defaults")

	t.Setenv("WEB_APP_BASE_URL", "https://app.staging.example")
	t.Setenv("WEB_APP_EXTRA_BASE_URLS", "http://localhost:3000")
	cfg, err = WebClientConfigFromEnv()
	require.NoError(t, err)
	assert.Equal(t, []string{"https://app.staging.example", "http://localhost:3000"}, cfg.BaseURLs)

	t.Setenv("WEB_APP_BASE_URL", "https://*.example")
	_, err = WebClientConfigFromEnv()
	require.Error(t, err)
}

func TestWebClientConfig_Apply_ExactURIsKeepsOtherAttributes(t *testing.T) {
	cfg := WebClientConfig{ClientID: "tender-web", BaseURLs: []string{"https://app.example", "https://staging.example"}}
	c := gocloak.Client{Attributes: &map[string]string{"pkce.code.challenge.method": "S256"}}
	cfg.apply(&c)
	assert.Equal(t, []string{"https://app.example/*", "https://staging.example/*"}, *c.RedirectURIs)
	assert.Equal(t, []string{"https://app.example", "https://staging.example"}, *c.WebOrigins)
	assert.Equal(t, "https://app.example/*##https://staging.example/*", (*c.Attributes)["post.logout.redirect.uris"])
	assert.Equal(t, "S256", (*c.Attributes)["pkce.code.challenge.method"])
}

// The image ships deploy/keycloak/realm-template.json (REALM_TEMPLATE_PATH)
// and the binary embeds this package's copy; they must never diverge.
func TestRealmTemplate_DeployCopyMatchesEmbedded(t *testing.T) {
	deployed, err := os.ReadFile("../../../../deploy/keycloak/realm-template.json")
	require.NoError(t, err)
	embedded, err := embeddedTemplateFS.ReadFile("realm-template.json")
	require.NoError(t, err)
	assert.JSONEq(t, string(embedded), string(deployed))
}

// The template carries the hardened tender-web flow settings and the
// HLD §11.3 lifetimes; only the URIs come from config.
func TestRealmTemplate_TenderWebHardened(t *testing.T) {
	rep, err := loadRealmTemplate()
	require.NoError(t, err)
	web := templateClient(rep, "tender-web")
	require.NotNil(t, web)
	assert.True(t, *web.PublicClient)
	assert.True(t, *web.StandardFlowEnabled)
	assert.False(t, *web.ImplicitFlowEnabled)
	assert.False(t, *web.DirectAccessGrantsEnabled)
	assert.False(t, *web.ServiceAccountsEnabled)
	assert.Empty(t, *web.RedirectURIs, "no wildcard redirect in the template")
	assert.Empty(t, *web.WebOrigins, `no "+" web origin in the template`)
	assert.Equal(t, "S256", (*web.Attributes)["pkce.code.challenge.method"])

	raw, err := readRealmTemplate()
	require.NoError(t, err)
	var s realmTokenSettings
	require.NoError(t, json.Unmarshal(raw, &s))
	assert.Equal(t, 300, *s.AccessTokenLifespan)        // 5 min
	assert.Equal(t, 43200, *s.SsoSessionIdleTimeout)    // SSO 12h idle
	assert.Equal(t, 86400, *s.SsoSessionMaxLifespan)    // SSO 24h max
	assert.Equal(t, 28800, *s.ClientSessionIdleTimeout) // refresh 8h idle
	assert.Equal(t, 43200, *s.ClientSessionMaxLifespan) // refresh 12h absolute
	assert.True(t, *s.RevokeRefreshToken)               // rotating
	assert.Equal(t, 0, *s.RefreshTokenMaxReuse)
}

// --- a stateful fake realm for the create/converge paths ---

type fakeRealm struct {
	mu            sync.Mutex
	exists        bool
	createStatus  int // POST /admin/realms/ status when set
	created       map[string]any
	tokenSettings map[string]any
	profile       map[string]any
	clients       map[string]*gocloak.Client // by internal id
	updated       []gocloak.Client
	mapperCreates map[string]int // "<internal client id>/<mapper name>" -> count
	mapperUpdates int
	scopeMoves    []string // "<internal client id>/<scope name>" moved from default to optional
}

// defaultScopes is a client's default scopes as Keycloak would list them:
// its own list when it was created with one, else Keycloak 26's built-in
// defaults (profile and email included). Moved scopes are gone.
func (f *fakeRealm) defaultScopes(id string) []gocloak.ClientScope {
	names := []string{"web-origins", "acr", "profile", "roles", "basic", "email"}
	if c := f.clients[id]; c != nil && c.DefaultClientScopes != nil {
		names = *c.DefaultClientScopes
	}
	out := []gocloak.ClientScope{}
	for _, n := range names {
		if !slices.Contains(f.scopeMoves, id+"/"+n) {
			out = append(out, gocloak.ClientScope{ID: gocloak.StringP("scope-" + n), Name: gocloak.StringP(n)})
		}
	}
	return out
}

func newFakeRealm(exists bool, clients ...gocloak.Client) *fakeRealm {
	f := &fakeRealm{exists: exists, clients: map[string]*gocloak.Client{}, mapperCreates: map[string]int{},
		profile: map[string]any{"attributes": []any{
			map[string]any{"name": "username"},
			map[string]any{"name": "tenant_id", "permissions": map[string]any{"view": []any{"admin", "user"}, "edit": []any{"admin", "user"}}},
		}}}
	for _, c := range clients {
		f.clients[*c.ID] = &c
	}
	return f
}

func (f *fakeRealm) register(t *testing.T, mux *http.ServeMux, realm string) {
	t.Helper()
	mux.HandleFunc("GET "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.exists {
			writeJSON(w, http.StatusNotFound, `{"error":"Realm not found."}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"realm":"`+realm+`"}`)
	})
	mux.HandleFunc("POST /admin/realms/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.createStatus != 0 {
			writeJSON(w, f.createStatus, `{"errorMessage":"Conflict detected"}`)
			f.exists = true
			return
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&f.created))
		f.exists = true
		// The created realm carries the template's clients.
		raw, _ := json.Marshal(f.created["clients"])
		var cs []gocloak.Client
		_ = json.Unmarshal(raw, &cs)
		for i := range cs {
			id := "id-" + *cs[i].ClientID
			cs[i].ID = &id
			f.clients[id] = &cs[i]
		}
		writeLocation(w, realm)
		writeJSON(w, http.StatusCreated, `{}`)
	})
	mux.HandleFunc("POST "+realmPath(realm, "roles"), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, `{"errorMessage":"exists"}`)
	})
	mux.HandleFunc("GET "+realmPath(realm, "users", "profile"), func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, _ := json.Marshal(f.profile)
		writeJSON(w, http.StatusOK, string(b))
	})
	mux.HandleFunc("PUT "+realmPath(realm, "users", "profile"), func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.profile = nil
		require.NoError(t, json.NewDecoder(r.Body).Decode(&f.profile))
		writeJSON(w, http.StatusOK, `{}`)
	})
	tokenSettingsHandler(mux, realm, &f.tokenSettings)
	mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []gocloak.Client{}
		for _, c := range f.clients {
			if q := r.URL.Query().Get("clientId"); q == "" || q == *c.ClientID {
				out = append(out, *c)
			}
		}
		b, _ := json.Marshal(out)
		writeJSON(w, http.StatusOK, string(b))
	})
	mux.HandleFunc("POST "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var c gocloak.Client
		require.NoError(t, json.NewDecoder(r.Body).Decode(&c))
		id := "id-" + *c.ClientID
		c.ID = &id
		f.clients[id] = &c
		writeLocation(w, id)
		writeJSON(w, http.StatusCreated, `{}`)
	})
	mux.HandleFunc("GET "+realmPath(realm, "clients", "{id}"), func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, _ := json.Marshal(f.clients[r.PathValue("id")])
		writeJSON(w, http.StatusOK, string(b))
	})
	mux.HandleFunc("PUT "+realmPath(realm, "clients", "{id}"), func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var c gocloak.Client
		require.NoError(t, json.NewDecoder(r.Body).Decode(&c))
		f.updated = append(f.updated, c)
		writeJSON(w, http.StatusNoContent, ``)
	})
	mux.HandleFunc("POST "+realmPath(realm, "clients", "{id}", "protocol-mappers", "models"), func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var m gocloak.ProtocolMapperRepresentation
		require.NoError(t, json.NewDecoder(r.Body).Decode(&m))
		switch *m.Name {
		case tenantIDMapperName:
			assert.Equal(t, tenantIDMapper(), m)
		case audienceMapperName:
			assert.Equal(t, audienceMapper(*f.clients[r.PathValue("id")].ClientID), m)
		case idTokenNameMapperName:
			assert.Equal(t, idTokenNameMapper(), m)
		case idTokenEmailMapperName:
			assert.Equal(t, idTokenEmailMapper(), m)
		default:
			t.Errorf("unexpected mapper %q", *m.Name)
		}
		f.mapperCreates[r.PathValue("id")+"/"+*m.Name]++
		writeLocation(w, "mapper-id")
		writeJSON(w, http.StatusCreated, `{}`)
	})
	mux.HandleFunc("PUT "+realmPath(realm, "clients", "{id}", "protocol-mappers", "models", "{mid}"), func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.mapperUpdates++
		writeJSON(w, http.StatusNoContent, ``)
	})
	mux.HandleFunc("GET "+realmPath(realm, "clients", "{id}", "default-client-scopes"), func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, _ := json.Marshal(f.defaultScopes(r.PathValue("id")))
		writeJSON(w, http.StatusOK, string(b))
	})
	mux.HandleFunc("DELETE "+realmPath(realm, "clients", "{id}", "default-client-scopes", "{sid}"), func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.scopeMoves = append(f.scopeMoves, r.PathValue("id")+"/"+strings.TrimPrefix(r.PathValue("sid"), "scope-"))
		writeJSON(w, http.StatusNoContent, ``)
	})
	mux.HandleFunc("PUT "+realmPath(realm, "clients", "{id}", "optional-client-scopes", "{sid}"), func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		assert.Contains(t, f.scopeMoves, r.PathValue("id")+"/"+strings.TrimPrefix(r.PathValue("sid"), "scope-"),
			"a scope becomes optional only after leaving the defaults")
		writeJSON(w, http.StatusNoContent, ``)
	})
}

func webCfg() WebClientConfig {
	return WebClientConfig{ClientID: "tender-web", BaseURLs: []string{"https://app.example"}}
}

func newSharedRealmAdmin(t *testing.T, realm string, f *fakeRealm) *Admin {
	t.Helper()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) { f.register(t, mux, realm) })
	a.cfg.Web = webCfg()
	return a
}

// tenantIDDecl returns the profile's tenant_id declaration(s).
func tenantIDDecl(profile map[string]any) []map[string]any {
	var out []map[string]any
	attrs, _ := profile["attributes"].([]any)
	for _, a := range attrs {
		if m, ok := a.(map[string]any); ok && m["name"] == "tenant_id" {
			out = append(out, m)
		}
	}
	return out
}

func assertAdminOnlyTenantID(t *testing.T, profile map[string]any) {
	t.Helper()
	assert.Equal(t, "ENABLED", profile["unmanagedAttributePolicy"])
	decl := tenantIDDecl(profile)
	require.Len(t, decl, 1, "exactly one tenant_id declaration, the earlier one replaced")
	assert.Equal(t, map[string]any{"view": []any{"admin"}, "edit": []any{"admin"}}, decl[0]["permissions"])
	attrs, _ := profile["attributes"].([]any)
	assert.Contains(t, attrs, map[string]any{"name": "username"}, "other attributes are kept")
}

func assertHardenedWebClient(t *testing.T, c gocloak.Client) {
	t.Helper()
	assert.Equal(t, []string{"https://app.example/*"}, *c.RedirectURIs)
	assert.Equal(t, []string{"https://app.example"}, *c.WebOrigins)
	assert.Equal(t, "S256", (*c.Attributes)["pkce.code.challenge.method"])
	assert.Equal(t, "https://app.example/*", (*c.Attributes)["post.logout.redirect.uris"])
	assert.True(t, *c.PublicClient)
	assert.False(t, *c.ImplicitFlowEnabled)
	assert.False(t, *c.DirectAccessGrantsEnabled)
}

// --- CreateRealmFromTemplate with a web config ---

func TestCreateRealmFromTemplate_WebClientFromConfigAndTokenSettings(t *testing.T) {
	f := newFakeRealm(false)
	a := newSharedRealmAdmin(t, "tenant-acme", f)

	require.NoError(t, a.CreateRealmFromTemplate(context.Background(), "tenant-acme", "v1"))

	require.Contains(t, f.clients, "id-tender-web")
	assertHardenedWebClient(t, *f.clients["id-tender-web"])
	assert.Equal(t, map[string]any{
		"accessTokenLifespan": 300.0, "ssoSessionIdleTimeout": 43200.0, "ssoSessionMaxLifespan": 86400.0,
		"clientSessionIdleTimeout": 28800.0, "clientSessionMaxLifespan": 43200.0,
		"revokeRefreshToken": true, "refreshTokenMaxReuse": 0.0,
	}, f.tokenSettings)
	assertAdminOnlyTenantID(t, f.profile)
	assert.Empty(t, f.mapperCreates, "a dedicated realm's tenant is its iss — no tenant_id mapper")
	assertCreatedWithPublicClientClaims(t, f)
}

// assertCreatedWithPublicClientClaims checks that the realm was created with
// each public client naming itself in aud, name and email in the ID token
// only, and profile/email optional; tender-gateway gets none of it.
func assertCreatedWithPublicClientClaims(t *testing.T, f *fakeRealm) {
	t.Helper()
	for _, id := range []string{"tender-web", "tender-mobile"} {
		c := f.clients["id-"+id]
		require.NotNil(t, c, id)
		require.NotNil(t, c.ProtocolMappers, id)
		assert.Contains(t, *c.ProtocolMappers, audienceMapper(id), "%s puts itself in aud", id)
		assert.Contains(t, *c.ProtocolMappers, idTokenNameMapper(), id)
		assert.Contains(t, *c.ProtocolMappers, idTokenEmailMapper(), id)
		require.NotNil(t, c.DefaultClientScopes, id)
		assert.NotContains(t, *c.DefaultClientScopes, "profile", id)
		assert.NotContains(t, *c.DefaultClientScopes, "email", id)
		assert.Contains(t, *c.OptionalClientScopes, "profile", id)
		assert.Contains(t, *c.OptionalClientScopes, "email", id)
	}
	gw := f.clients["id-tender-gateway"]
	assert.Nil(t, gw.ProtocolMappers, "the confidential client gets no public-client mapper")
	assert.Nil(t, gw.DefaultClientScopes, "the confidential client keeps the realm's default scopes")
}

func TestApplyRealmTokenSettings_TemplateWithoutSettingsIsNoop(t *testing.T) {
	p := t.TempDir() + "/tpl.json"
	require.NoError(t, os.WriteFile(p, []byte(`{"sslRequired":"external"}`), 0o600))
	t.Setenv("REALM_TEMPLATE_PATH", p)
	a := newFakeAdmin(t, nil, nil) // any realm PUT would 404
	require.NoError(t, a.applyRealmTokenSettings(context.Background(), "tok", "acme"))
}

func TestApplyRealmTokenSettings_PutError(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("PUT "+realmPath("acme"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusForbidden, `{}`)
		})
	})
	err := a.applyRealmTokenSettings(context.Background(), "tok", "acme")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "update realm")
}

// --- EnsureSharedRealm (shared trial realm, RP-INV-2) ---

func TestEnsureSharedRealm_CreatesWhenAbsent(t *testing.T) {
	f := newFakeRealm(false)
	a := newSharedRealmAdmin(t, "trial", f)

	require.NoError(t, a.EnsureSharedRealm(context.Background(), "trial"))

	require.NotNil(t, f.created, "the realm was created from the template")
	assert.Equal(t, "trial", f.created["realm"])
	require.NotEmpty(t, f.updated)
	assertHardenedWebClient(t, f.updated[0])
	assert.Equal(t, map[string]int{"id-tender-web/tenant_id": 1, "id-tender-mobile/tenant_id": 1}, f.mapperCreates,
		"tenant_id mapper on every public client, not on tender-gateway; the others came with the create")
	assert.Empty(t, f.scopeMoves, "a realm created with the public-client scope lists needs no move")
	assertCreatedWithPublicClientClaims(t, f)
	assertAdminOnlyTenantID(t, f.profile)
	assert.Equal(t, 300.0, f.tokenSettings["accessTokenLifespan"])
}

// An existing realm with the old wildcard client and a drifted mapper is
// converged in place: URIs replaced, unrelated attributes kept, mapper
// rewritten, nothing re-created.
func TestEnsureSharedRealm_ConvergesExisting(t *testing.T) {
	webID, mobileID := "id-tender-web", "id-tender-mobile"
	f := newFakeRealm(true,
		gocloak.Client{ID: &webID, ClientID: gocloak.StringP("tender-web"), PublicClient: gocloak.BoolP(true),
			DirectAccessGrantsEnabled: gocloak.BoolP(true),
			RedirectURIs:              &[]string{"https://*.bcbpsolutions.com/*"}, WebOrigins: &[]string{"+"},
			Attributes: &map[string]string{"custom": "kept"},
			ProtocolMappers: &[]gocloak.ProtocolMapperRepresentation{{
				ID: gocloak.StringP("m1"), Name: gocloak.StringP("tenant_id"),
				ProtocolMapper: gocloak.StringP("oidc-usermodel-attribute-mapper"),
				Config:         &map[string]string{"user.attribute": "tenant_id", "claim.name": "tenant_id"},
			}}},
		gocloak.Client{ID: &mobileID, ClientID: gocloak.StringP("tender-mobile"), PublicClient: gocloak.BoolP(true),
			ProtocolMappers: &[]gocloak.ProtocolMapperRepresentation{func() gocloak.ProtocolMapperRepresentation {
				m := tenantIDMapper()
				m.ID = gocloak.StringP("m2")
				return m
			}()}},
	)
	a := newSharedRealmAdmin(t, "trial", f)

	require.NoError(t, a.EnsureSharedRealm(context.Background(), "trial"))

	assert.Nil(t, f.created, "an existing realm is never re-created")
	require.Len(t, f.updated, 1)
	assertHardenedWebClient(t, f.updated[0])
	assert.Equal(t, "kept", (*f.updated[0].Attributes)["custom"])
	assert.Equal(t, 1, f.mapperUpdates, "the drifted tender-web mapper is rewritten")
	assert.Equal(t, map[string]int{
		"id-tender-web/audience": 1, "id-tender-mobile/audience": 1,
		"id-tender-web/id_token_name": 1, "id-tender-mobile/id_token_name": 1,
		"id-tender-web/id_token_email": 1, "id-tender-mobile/id_token_email": 1,
	}, f.mapperCreates, "the missing mappers are added; the correct tender-mobile tenant_id mapper is left alone")
	assert.ElementsMatch(t, []string{
		"id-tender-web/profile", "id-tender-web/email", "id-tender-mobile/profile", "id-tender-mobile/email",
	}, f.scopeMoves, "profile and email move from default to optional; other scopes stay")
}

// Two replicas booting together: the loser's create gets 409 and it
// converges the winner's realm instead of failing.
func TestEnsureSharedRealm_CreateRaceConverges(t *testing.T) {
	f := newFakeRealm(false)
	f.createStatus = http.StatusConflict
	a := newSharedRealmAdmin(t, "trial", f)

	require.NoError(t, a.EnsureSharedRealm(context.Background(), "trial"))
	require.Contains(t, f.clients, "id-tender-web", "the missing web client is created")
	assertHardenedWebClient(t, *f.clients["id-tender-web"])
}

func TestEnsureSharedRealm_RequiresWebConfig(t *testing.T) {
	a := newFakeAdmin(t, nil, nil)
	err := a.EnsureSharedRealm(context.Background(), "trial")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "web client config")
}

func TestEnsureSharedRealm_GetRealmError(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("trial"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusForbidden, `{}`)
		})
	})
	a.cfg.Web = webCfg()
	err := a.EnsureSharedRealm(context.Background(), "trial")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get_realm")
}

func TestEnsureSharedRealm_UpdateClientError(t *testing.T) {
	webID := "id-tender-web"
	f := newFakeRealm(true, gocloak.Client{ID: &webID, ClientID: gocloak.StringP("tender-web")})
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("PUT "+realmPath("trial", "clients", webID), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusBadRequest, `{"errorMessage":"invalid redirect"}`)
		})
		f.register(t, mux, "trial")
	})
	a.cfg.Web = webCfg()
	err := a.EnsureSharedRealm(context.Background(), "trial")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "web_client")
}

// --- ensureOptionalOnlyScopes failure paths ---

func TestEnsureOptionalOnlyScopes_ClientAbsentIsSkipped(t *testing.T) {
	f := newFakeRealm(true)
	a := newSharedRealmAdmin(t, "trial", f)
	require.NoError(t, a.ensureOptionalOnlyScopes(context.Background(), "tok", "trial", "tender-mobile"))
	assert.Empty(t, f.scopeMoves)
}

func TestEnsureOptionalOnlyScopes_Errors(t *testing.T) {
	webID := "id-tender-web"
	for _, tc := range []struct{ name, method, path, want string }{
		{"list clients", "GET", realmPath("trial", "clients"), "get_clients"},
		{"list default scopes", "GET", realmPath("trial", "clients", webID, "default-client-scopes"), "get_default_scopes"},
		{"remove default", "DELETE", realmPath("trial", "clients", webID, "default-client-scopes", "scope-profile"), "remove_default_scope profile"},
		{"add optional", "PUT", realmPath("trial", "clients", webID, "optional-client-scopes", "scope-profile"), "add_optional_scope profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRealm(true, gocloak.Client{ID: &webID, ClientID: gocloak.StringP("tender-web"), PublicClient: gocloak.BoolP(true)})
			a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
				mux.HandleFunc(tc.method+" "+tc.path, func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, http.StatusForbidden, `{}`)
				})
				if tc.want != "get_clients" { // the same pattern cannot be registered twice
					f.register(t, mux, "trial")
				}
			})
			err := a.ensureOptionalOnlyScopes(context.Background(), "tok", "trial", "tender-web")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
