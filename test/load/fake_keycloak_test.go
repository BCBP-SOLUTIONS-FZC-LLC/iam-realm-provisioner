//go:build load

package load_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeKeycloakServer stands in for Keycloak's Admin REST API (the
// "loopback-network/mock-Keycloak caveat" the LLD §14.6 requires — a real
// Keycloak testcontainer is deliberately NOT used here, both because it is
// too slow/flaky at N=500 and because the point of this suite is to
// measure THIS SERVICE's rate-limiter/conversion-loop behavior, not
// Keycloak's own performance).
//
// It implements just enough of the Admin API surface that
// internal/adapter/outbound/keycloak.Admin's Convert-path methods
// (CreateRealmFromTemplate, RegisterOIDCClients, ListUsersByTenant,
// MigrateUser, ForceLogoutRealm) exercise real gocloak HTTP calls against
// it, following the same httptest.Server-behind-gocloak.NewClient pattern
// already proven in internal/adapter/outbound/keycloak/admin_token_test.go
// and internal/adapter/outbound/keycloak/fake_admin_test.go's newFakeAdmin
// helper.
//
// Every handler sleeps callLatency before responding so the rate limiter's
// throttling is actually observable (an instant fake would let the
// sequential migration loop run at whatever speed the Go runtime allows,
// which tells us nothing about KEYCLOAK_RATE_LIMIT_RPS).
type fakeKeycloakServer struct {
	t           *testing.T
	srv         *httptest.Server
	callLatency time.Duration

	mu               sync.Mutex
	realmUsers       map[string][]string // realm -> seeded source-user ids (ListUsersByTenant)
	migrationStarts  map[string]time.Time
	migrationLatency []time.Duration // one entry per completed MigrateUser (GetUserByID -> partialImport)
	completions      []time.Time     // wall-clock time each MigrateUser completed (partialImport response)
}

func newFakeKeycloakServer(t *testing.T, callLatency time.Duration) *fakeKeycloakServer {
	t.Helper()
	f := &fakeKeycloakServer{
		t:               t,
		callLatency:     callLatency,
		realmUsers:      map[string][]string{},
		migrationStarts: map[string]time.Time{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handle)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// seedTrialUsers registers ids as the tenant's users in realm, returned by
// the fake server's ListUsersByTenant (GET .../users?q=tenant_id:...) path.
func (f *fakeKeycloakServer) seedTrialUsers(realm string, ids []uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	f.realmUsers[realm] = strs
}

// latencySnapshot returns a copy of the per-user migration latencies
// recorded so far.
func (f *fakeKeycloakServer) latencySnapshot() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Duration, len(f.migrationLatency))
	copy(out, f.migrationLatency)
	return out
}

// completionSnapshot returns a copy of the completion timestamps recorded
// so far (one per fully-migrated user), used to measure achieved
// throughput against the configured rate-limiter budget.
func (f *fakeKeycloakServer) completionSnapshot() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Time, len(f.completions))
	copy(out, f.completions)
	return out
}

func (f *fakeKeycloakServer) baseURL() string { return f.srv.URL }

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// handle dispatches every request this test drives against the fake
// Keycloak Admin API. Ordering matters: more specific path suffixes are
// checked before the generic ones they'd otherwise be swallowed by.
func (f *fakeKeycloakServer) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// The client-credentials token endpoint is NOT part of the Admin API
	// call budget the rate limiter governs — respond immediately, with a
	// long expiry so the token cache amortizes it to (at most) a couple of
	// mints for the whole run, matching real-world behavior (§21.4).
	if path == "/realms/master/protocol/openid-connect/token" {
		writeJSON(w, http.StatusOK, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
		return
	}

	// Every other route simulates real network/Keycloak-processing latency
	// so the rate limiter's pacing is observable against wall-clock time.
	time.Sleep(f.callLatency)

	switch {
	case strings.HasSuffix(path, "/users/profile"):
		// enableUnmanagedAttributes: GET then PUT the User Profile config.
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, `{"attributes":[]}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"attributes":[]}`)
		return

	case strings.HasSuffix(path, "/credentials"):
		// GetCredentials — no TOTP/WebAuthn credential to copy.
		writeJSON(w, http.StatusOK, `[]`)
		return

	case strings.HasSuffix(path, "/logout"):
		// LogoutAllSessions (ForceLogoutRealm, per user).
		writeJSON(w, http.StatusNoContent, "")
		return

	case strings.HasSuffix(path, "/partialImport"):
		f.handlePartialImport(w, r)
		return

	case strings.HasSuffix(path, "/service-account-user"):
		// GetClientServiceAccount (CreateServiceAccountClient's sub lookup,
		// §2.5/RP-17).
		writeJSON(w, http.StatusOK, `{"id":"`+uuid.NewString()+`"}`)
		return

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/clients"):
		// CreateClient (CreateServiceAccountClient's mint call, §2.5/RP-17).
		w.Header().Set("Location", strings.TrimSuffix(r.URL.String(), "/")+"/"+uuid.NewString())
		writeJSON(w, http.StatusCreated, `{}`)
		return

	case r.Method == http.MethodPut && strings.Contains(path, "/clients/"):
		// UpdateClient — unused by the mint path since EXT-6 (client-jwt/
		// jwks.url needs no apply-a-secret step), kept for any other
		// UpdateClient caller this fake server may pick up.
		writeJSON(w, http.StatusOK, `{}`)
		return

	case strings.HasSuffix(path, "/clients"):
		// RegisterOIDCClients / CreateServiceAccountClient's existence-check
		// GET — no non-builtin/pre-existing clients on the template.
		writeJSON(w, http.StatusOK, `[]`)
		return

	case strings.HasSuffix(path, "/roles"):
		// CreateRealmRole (requires-mfa role on the new dedicated realm).
		writeJSON(w, http.StatusCreated, `{}`)
		return

	case strings.HasSuffix(path, "/users") && r.Method == http.MethodGet:
		f.handleGetUsers(w, r)
		return

	case r.Method == http.MethodGet && strings.Contains(path, "/users/"):
		// GetUserByID — start of one user's migration.
		f.handleGetUserByID(w, r)
		return

	case r.Method == http.MethodDelete && strings.Contains(path, "/users/"):
		// DeleteUser — source-realm delete inside MigrateUser.
		writeJSON(w, http.StatusNoContent, "")
		return

	case r.Method == http.MethodPost && strings.TrimSuffix(path, "/") == "/admin/realms":
		// CreateRealm.
		writeJSON(w, http.StatusCreated, `{}`)
		return

	case r.Method == http.MethodPut:
		// UpdateRealm (enable/disable), not exercised by Convert but
		// handled for safety.
		writeJSON(w, http.StatusOK, `{}`)
		return

	case r.Method == http.MethodGet:
		// GetRealm, not exercised by Convert but handled for safety.
		writeJSON(w, http.StatusOK, `{}`)
		return

	default:
		f.t.Logf("fakeKeycloakServer: unhandled %s %s", r.Method, path)
		writeJSON(w, http.StatusOK, `{}`)
	}
}

func (f *fakeKeycloakServer) handleGetUsers(w http.ResponseWriter, r *http.Request) {
	// ListUsersByTenant carries ?q=tenant_id:<id>; ForceLogoutRealm's
	// GetUsers(realm, GetUsersParams{}) carries no query at all — by the
	// time conversion calls ForceLogoutRealm every source user has already
	// been deleted (MigrateUser's delete-then-import step), so returning
	// an empty list here is the faithful real-Keycloak behavior, not a
	// simplification.
	if r.URL.RawQuery == "" {
		writeJSON(w, http.StatusOK, `[]`)
		return
	}
	realm := realmFromUsersPath(r.URL.Path)
	f.mu.Lock()
	ids := append([]string(nil), f.realmUsers[realm]...)
	f.mu.Unlock()
	var sb strings.Builder
	sb.WriteByte('[')
	for i, id := range ids {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"id":"`)
		sb.WriteString(id)
		sb.WriteString(`"}`)
	}
	sb.WriteByte(']')
	writeJSON(w, http.StatusOK, sb.String())
}

func (f *fakeKeycloakServer) handleGetUserByID(w http.ResponseWriter, r *http.Request) {
	id := lastPathSegment(r.URL.Path)
	f.mu.Lock()
	f.migrationStarts[id] = time.Now()
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, `{"username":"synthetic","enabled":true,"emailVerified":true}`)
}

func (f *fakeKeycloakServer) handlePartialImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Users []struct {
			ID string `json:"id"`
		} `json:"users"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	now := time.Now()
	if len(body.Users) == 1 {
		id := body.Users[0].ID
		f.mu.Lock()
		if start, ok := f.migrationStarts[id]; ok {
			f.migrationLatency = append(f.migrationLatency, now.Sub(start))
			delete(f.migrationStarts, id)
		}
		f.completions = append(f.completions, now)
		f.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, `{"added":1,"skipped":0,"overwritten":0}`)
}

// realmFromUsersPath extracts {realm} from /admin/realms/{realm}/users.
func realmFromUsersPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p == "realms" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func lastPathSegment(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}
