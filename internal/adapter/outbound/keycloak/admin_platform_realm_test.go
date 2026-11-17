package keycloak

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePlatformRealm scripts the Keycloak routes EnsurePlatformRealm calls.
// Each status field is the answer of one route; zero means success.
type fakePlatformRealm struct {
	mu sync.Mutex

	getStatus      int    // GET  /admin/realms/{realm}: 0 → 200, else that status
	createStatus   int    // POST /admin/realms: 0 → 201
	roleStatus     int    // POST /admin/realms/{realm}/roles: 0 → 201
	execsStatus    int    // GET  …/authentication/flows/browser/executions: 0 → 200
	execsBody      string // its body; "" → one Conditional OTP execution at CONDITIONAL
	execPutStatus  int    // PUT  …/authentication/flows/browser/executions: 0 → 204
	settingsStatus int    // PUT  /admin/realms/{realm}: 0 → 204

	created     bool
	execUpdates []map[string]any
	settings    map[string]any
}

const platformRealmName = "platform"

func (f *fakePlatformRealm) register(mux *http.ServeMux) {
	execs := realmPath(platformRealmName, "authentication", "flows", "browser", "executions")
	mux.HandleFunc("GET "+realmPath(platformRealmName), func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case f.getStatus != 0:
			writeJSON(w, f.getStatus, `{}`)
		default:
			writeJSON(w, http.StatusOK, fmt.Sprintf(`{"realm":%q}`, platformRealmName))
		}
	})
	mux.HandleFunc("POST /admin/realms/", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.createStatus != 0 {
			writeJSON(w, f.createStatus, `{"errorMessage":"create"}`)
			return
		}
		f.created = true
		writeLocation(w, platformRealmName)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST "+realmPath(platformRealmName, "roles"), func(w http.ResponseWriter, _ *http.Request) {
		if f.roleStatus != 0 {
			writeJSON(w, f.roleStatus, `{"errorMessage":"role"}`)
			return
		}
		writeLocation(w, "platform_operator")
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("GET "+execs, func(w http.ResponseWriter, _ *http.Request) {
		if f.execsStatus != 0 {
			writeJSON(w, f.execsStatus, `{}`)
			return
		}
		body := f.execsBody
		if body == "" {
			body = `[{"id":"e0","displayName":"Cookie","requirement":"ALTERNATIVE"},null,
				{"id":"e1","displayName":"Browser - Conditional OTP","requirement":"CONDITIONAL"}]`
		}
		writeJSON(w, http.StatusOK, body)
	})
	mux.HandleFunc("PUT "+execs, func(w http.ResponseWriter, r *http.Request) {
		if f.execPutStatus != 0 {
			writeJSON(w, f.execPutStatus, `{}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.execUpdates = append(f.execUpdates, body)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT "+realmPath(platformRealmName), func(w http.ResponseWriter, r *http.Request) {
		if f.settingsStatus != 0 {
			writeJSON(w, f.settingsStatus, `{"errorMessage":"settings"}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.settings = body
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
}

func newPlatformRealmAdmin(t *testing.T, f *fakePlatformRealm, tokens ...tokenResponse) *Admin {
	t.Helper()
	return newFakeAdmin(t, tokens, f.register)
}

// An existing realm is converged: role ensured, the Conditional OTP subflow
// raised to REQUIRED, and the short-session settings written.
func TestEnsurePlatformRealm_ConvergesExisting(t *testing.T) {
	f := &fakePlatformRealm{}
	a := newPlatformRealmAdmin(t, f)

	require.NoError(t, a.EnsurePlatformRealm(context.Background(), platformRealmName))

	assert.False(t, f.created, "an existing realm is never re-created")
	require.Len(t, f.execUpdates, 1)
	assert.Equal(t, "REQUIRED", f.execUpdates[0]["requirement"])
	assert.Equal(t, "e1", f.execUpdates[0]["id"])
	assert.InDelta(t, 300.0, f.settings["accessTokenLifespan"], 0)
	assert.InDelta(t, 28800.0, f.settings["ssoSessionMaxLifespan"], 0)
	assert.Equal(t, true, f.settings["revokeRefreshToken"])
}

func TestEnsurePlatformRealm_CreatesWhenAbsent(t *testing.T) {
	f := &fakePlatformRealm{getStatus: http.StatusNotFound}
	a := newPlatformRealmAdmin(t, f)

	require.NoError(t, a.EnsurePlatformRealm(context.Background(), platformRealmName))
	assert.True(t, f.created)
	require.Len(t, f.execUpdates, 1)
}

// A second replica losing the create race (409) converges the winner's
// realm; an existing role (409) is not an error.
func TestEnsurePlatformRealm_CreateRaceAndExistingRole(t *testing.T) {
	f := &fakePlatformRealm{getStatus: http.StatusNotFound, createStatus: http.StatusConflict, roleStatus: http.StatusConflict}
	a := newPlatformRealmAdmin(t, f)

	require.NoError(t, a.EnsurePlatformRealm(context.Background(), platformRealmName))
	assert.NotNil(t, f.settings, "the realm is still converged")
}

// Already REQUIRED: no execution update is sent.
func TestEnsurePlatformRealm_OTPAlreadyRequired_NoUpdate(t *testing.T) {
	f := &fakePlatformRealm{execsBody: `[{"id":"e1","displayName":"Browser - Conditional OTP","requirement":"REQUIRED"}]`}
	a := newPlatformRealmAdmin(t, f)

	require.NoError(t, a.EnsurePlatformRealm(context.Background(), platformRealmName))
	assert.Empty(t, f.execUpdates)
}

func TestEnsurePlatformRealm_Errors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fake   *fakePlatformRealm
		tokens []tokenResponse
		want   string
	}{
		{"token", &fakePlatformRealm{}, []tokenResponse{failTokenResponse()}, "ensure_platform_realm: token"},
		{"get realm", &fakePlatformRealm{getStatus: http.StatusForbidden}, nil, "get_realm"},
		{"create realm", &fakePlatformRealm{getStatus: http.StatusNotFound, createStatus: http.StatusBadRequest}, nil, "create_platform_realm"},
		{"token after create", &fakePlatformRealm{getStatus: http.StatusNotFound},
			[]tokenResponse{okTokenResponse(), failTokenResponse()}, "create_platform_realm: refresh token"},
		{"token after create race", &fakePlatformRealm{getStatus: http.StatusNotFound, createStatus: http.StatusConflict},
			[]tokenResponse{okTokenResponse(), failTokenResponse()}, "ensure_platform_realm: token"},
		{"create role", &fakePlatformRealm{roleStatus: http.StatusForbidden}, nil, "create_role"},
		{"get executions", &fakePlatformRealm{execsStatus: http.StatusForbidden}, nil, "get_auth_executions"},
		{"update execution", &fakePlatformRealm{execPutStatus: http.StatusBadRequest}, nil, "enforce_otp"},
		// The browser flow drifted: MFA cannot be enforced, so it is an
		// error, never a silent success.
		{"otp subflow missing", &fakePlatformRealm{execsBody: `[{"id":"e0","displayName":"Cookie"}]`}, nil, "Conditional OTP' execution not found"},
		{"token settings", &fakePlatformRealm{settingsStatus: http.StatusBadRequest}, nil, "token settings: unexpected status 400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newPlatformRealmAdmin(t, tc.fake, tc.tokens...)
			err := a.EnsurePlatformRealm(context.Background(), platformRealmName)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// A transport failure on the settings PUT (server gone) is wrapped, not a
// status error.
func TestApplyPlatformTokenSettings_TransportError(t *testing.T) {
	a := newFakeAdmin(t, nil, nil)
	a.cfg.BaseURL = "http://127.0.0.1:1"
	err := a.applyPlatformTokenSettings(context.Background(), "tok", platformRealmName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token_settings")
}
