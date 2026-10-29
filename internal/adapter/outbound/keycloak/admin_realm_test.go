package keycloak

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// --- loadRealmTemplate ---

func TestLoadRealmTemplate_EmbeddedFallback(t *testing.T) {
	rep, err := loadRealmTemplate()
	require.NoError(t, err)
	require.NotNil(t, rep)
}

func TestLoadRealmTemplate_FromDiskPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "custom-template.json")
	require.NoError(t, os.WriteFile(p, []byte(`{"realm":"placeholder","sslRequired":"external"}`), 0o600))
	t.Setenv("REALM_TEMPLATE_PATH", p)

	rep, err := loadRealmTemplate()
	require.NoError(t, err)
	require.NotNil(t, rep.SslRequired)
	require.Equal(t, "external", *rep.SslRequired)
}

func TestLoadRealmTemplate_DiskPathUnreadable_FallsBackToEmbedded(t *testing.T) {
	t.Setenv("REALM_TEMPLATE_PATH", filepath.Join(t.TempDir(), "does-not-exist.json"))
	rep, err := loadRealmTemplate()
	require.NoError(t, err, "an unreadable disk path must silently fall back to the embedded template")
	require.NotNil(t, rep)
}

func TestLoadRealmTemplate_DiskPathInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(p, []byte(`{not valid json`), 0o600))
	t.Setenv("REALM_TEMPLATE_PATH", p)

	_, err := loadRealmTemplate()
	require.Error(t, err)
}

// --- CreateRealmFromTemplate ---

func realmPath(realm string, parts ...string) string {
	p := "/admin/realms/" + realm
	for _, part := range parts {
		p += "/" + part
	}
	return p
}

func userProfileHandlers(mux *http.ServeMux, realm string, getFail, putFail bool) {
	mux.HandleFunc("GET "+realmPath(realm, "users", "profile"), func(w http.ResponseWriter, _ *http.Request) {
		if getFail {
			writeJSON(w, http.StatusInternalServerError, `{}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"unmanagedAttributePolicy":"DISABLED"}`)
	})
	mux.HandleFunc("PUT "+realmPath(realm, "users", "profile"), func(w http.ResponseWriter, _ *http.Request) {
		if putFail {
			writeJSON(w, http.StatusInternalServerError, `{}`)
			return
		}
		writeJSON(w, http.StatusOK, `{}`)
	})
}

// tokenSettingsHandler serves the partial realm PUT that applies the
// template's token settings, decoding its body into got when non-nil.
func tokenSettingsHandler(mux *http.ServeMux, realm string, got *map[string]any) {
	mux.HandleFunc("PUT "+realmPath(realm), func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			_ = json.NewDecoder(r.Body).Decode(got)
		}
		writeJSON(w, http.StatusNoContent, ``)
	})
}

func TestCreateRealmFromTemplate_Success(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, []tokenResponse{okTokenResponse(), okTokenResponse()}, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /admin/realms/", func(w http.ResponseWriter, r *http.Request) {
			writeLocation(w, realm)
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "roles"), func(w http.ResponseWriter, r *http.Request) {
			writeLocation(w, "role-id")
			writeJSON(w, http.StatusCreated, `{}`)
		})
		userProfileHandlers(mux, realm, false, false)
		tokenSettingsHandler(mux, realm, nil)
	})

	err := a.CreateRealmFromTemplate(context.Background(), realm, "tpl")
	require.NoError(t, err)
}

// TestCreateRealmFromTemplate_NeverSetsRealmID guards against Keycloak's
// REALM.ID column (VARCHAR(36), sized for a bare UUID) overflowing on a
// long realm name: RP-3's "tenant-"+<36-char tenant UUID> is 43 characters
// and once overflowed that column on every real Keycloak, surfaced as an
// opaque 409 "Conflict detected" the existing conflict-handling branch
// misread as "realm already exists". The fix was to stop setting rep.ID at
// all (Keycloak assigns its own PK); this test fails if that regresses.
func TestCreateRealmFromTemplate_NeverSetsRealmID(t *testing.T) {
	realm := "tenant-82e241d8-f134-4470-9be1-ec5e061f299a" // 43 chars, matches RP-3's dedicatedRealmName shape
	require.Greater(t, len(realm), 36, "test realm name must exceed Keycloak's REALM.ID column length to be a meaningful guard")

	var capturedBody map[string]any
	a := newFakeAdmin(t, []tokenResponse{okTokenResponse(), okTokenResponse()}, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /admin/realms/", func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&capturedBody))
			writeLocation(w, realm)
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "roles"), func(w http.ResponseWriter, r *http.Request) {
			writeLocation(w, "role-id")
			writeJSON(w, http.StatusCreated, `{}`)
		})
		userProfileHandlers(mux, realm, false, false)
		tokenSettingsHandler(mux, realm, nil)
	})

	err := a.CreateRealmFromTemplate(context.Background(), realm, "tpl")
	require.NoError(t, err)

	id, present := capturedBody["id"]
	require.False(t, present && id != nil, "CreateRealm request body must never set id — Keycloak's REALM.ID column overflows on a realm name this long")
	require.Equal(t, realm, capturedBody["realm"], "the logical realm name must still be set")
}

func TestCreateRealmFromTemplate_LoadTemplateError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(p, []byte(`{not json`), 0o600))
	t.Setenv("REALM_TEMPLATE_PATH", p)

	a := newFakeAdmin(t, nil, nil)
	err := a.CreateRealmFromTemplate(context.Background(), "acme", "tpl")
	require.Error(t, err)
	require.Contains(t, err.Error(), "load realm template")
}

func TestCreateRealmFromTemplate_Conflict(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /admin/realms/", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusConflict, `{"errorMessage":"already exists"}`)
		})
	})
	err := a.CreateRealmFromTemplate(context.Background(), "acme", "tpl")
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	require.Equal(t, domain.ErrRealmAlreadyDedicated, derr.Code)
}

func TestCreateRealmFromTemplate_CreateRealmGenericError(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /admin/realms/", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.CreateRealmFromTemplate(context.Background(), "acme", "tpl")
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	require.Equal(t, domain.ErrKeycloakUnavailable, derr.Code)
}

func TestCreateRealmFromTemplate_RefreshTokenAfterCreateFails(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, []tokenResponse{okTokenResponse(), failTokenResponse()}, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /admin/realms/", func(w http.ResponseWriter, r *http.Request) {
			writeLocation(w, realm)
			writeJSON(w, http.StatusCreated, `{}`)
		})
	})
	err := a.CreateRealmFromTemplate(context.Background(), realm, "tpl")
	require.Error(t, err)
	require.Contains(t, err.Error(), "refresh token post-create")
}

func TestCreateRealmFromTemplate_CreateRealmRoleGenericError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, []tokenResponse{okTokenResponse(), okTokenResponse()}, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /admin/realms/", func(w http.ResponseWriter, r *http.Request) {
			writeLocation(w, realm)
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "roles"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.CreateRealmFromTemplate(context.Background(), realm, "tpl")
	require.Error(t, err)
	require.Contains(t, err.Error(), "create_realm_role")
}

func TestCreateRealmFromTemplate_RoleAlreadyExistsIsIgnored(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, []tokenResponse{okTokenResponse(), okTokenResponse()}, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /admin/realms/", func(w http.ResponseWriter, r *http.Request) {
			writeLocation(w, realm)
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "roles"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusConflict, `{"errorMessage":"role exists"}`)
		})
		userProfileHandlers(mux, realm, false, false)
		tokenSettingsHandler(mux, realm, nil)
	})
	err := a.CreateRealmFromTemplate(context.Background(), realm, "tpl")
	require.NoError(t, err)
}

func TestCreateRealmFromTemplate_UserProfileError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, []tokenResponse{okTokenResponse(), okTokenResponse()}, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /admin/realms/", func(w http.ResponseWriter, r *http.Request) {
			writeLocation(w, realm)
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "roles"), func(w http.ResponseWriter, r *http.Request) {
			writeLocation(w, "role-id")
			writeJSON(w, http.StatusCreated, `{}`)
		})
		userProfileHandlers(mux, realm, true, false)
	})
	err := a.CreateRealmFromTemplate(context.Background(), realm, "tpl")
	require.Error(t, err)
	require.Contains(t, err.Error(), "user_profile")
}

// --- applyUserProfile (deeper branches beyond the one exercised
// above) ---

func TestApplyUserProfile_GetTransportError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, []tokenResponse{okTokenResponse()}, nil) // no route registered -> connection works but 404s, not a transport error
	tok, err := a.token(context.Background())
	require.NoError(t, err)
	err = a.applyUserProfile(context.Background(), tok, realm)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get user profile")
}

func TestApplyUserProfile_DecodeError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "users", "profile"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `not json`)
		})
	})
	tok, err := a.token(context.Background())
	require.NoError(t, err)
	err = a.applyUserProfile(context.Background(), tok, realm)
	require.Error(t, err)
	require.Contains(t, err.Error(), "decode user profile")
}

func TestApplyUserProfile_PutError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		userProfileHandlers(mux, realm, false, true)
	})
	tok, err := a.token(context.Background())
	require.NoError(t, err)
	err = a.applyUserProfile(context.Background(), tok, realm)
	require.Error(t, err)
	require.Contains(t, err.Error(), "update user profile")
}

// hijackAndClose abruptly closes the raw TCP connection without writing an
// HTTP response, simulating a genuine transport-level failure (as opposed
// to a well-formed HTTP error status) so resty's Get/Put itself returns a
// non-nil error.
func hijackAndClose(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}
}

func TestApplyUserProfile_GetRealTransportError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "users", "profile"), hijackAndClose(t))
	})
	tok, err := a.token(context.Background())
	require.NoError(t, err)
	err = a.applyUserProfile(context.Background(), tok, realm)
	require.Error(t, err)
}

func TestApplyUserProfile_PutRealTransportError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "users", "profile"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"unmanagedAttributePolicy":"DISABLED"}`)
		})
		mux.HandleFunc("PUT "+realmPath(realm, "users", "profile"), hijackAndClose(t))
	})
	tok, err := a.token(context.Background())
	require.NoError(t, err)
	err = a.applyUserProfile(context.Background(), tok, realm)
	require.Error(t, err)
}

func TestApplyUserProfile_Success(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		userProfileHandlers(mux, realm, false, false)
		tokenSettingsHandler(mux, realm, nil)
	})
	tok, err := a.token(context.Background())
	require.NoError(t, err)
	require.NoError(t, a.applyUserProfile(context.Background(), tok, realm))
}

// --- RegisterOIDCClients ---

func TestRegisterOIDCClients_FiltersBuiltins(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[
				{"clientId":"realm-management"},
				{"clientId":"account"},
				{"clientId":"account-console"},
				{"clientId":"broker"},
				{"clientId":"security-admin-console"},
				{"clientId":"admin-cli"},
				{"clientId":"tenant-web"},
				{"clientId":"tenant-mobile"}
			]`)
		})
	})
	ids, err := a.RegisterOIDCClients(context.Background(), realm, "acme-slug")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"tenant-web", "tenant-mobile"}, ids)
}

func TestRegisterOIDCClients_GetClientsError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, err := a.RegisterOIDCClients(context.Background(), realm, "acme-slug")
	require.Error(t, err)
}

func TestRegisterOIDCClients_NilClientIDSkipped(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"no-client-id-field"}]`)
		})
	})
	ids, err := a.RegisterOIDCClients(context.Background(), realm, "acme-slug")
	require.NoError(t, err)
	require.Empty(t, ids)
}

// --- DisableRealm / EnableRealm ---

func TestDisableRealm_Success(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("PUT "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{}`)
		})
	})
	require.NoError(t, a.DisableRealm(context.Background(), realm))
}

func TestDisableRealm_Error(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("PUT "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.DisableRealm(context.Background(), realm)
	require.Error(t, err)
}

func TestEnableRealm_Success(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("PUT "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{}`)
		})
	})
	require.NoError(t, a.EnableRealm(context.Background(), realm))
}

func TestEnableRealm_Error(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("PUT "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.EnableRealm(context.Background(), realm)
	require.Error(t, err)
}

// --- ExportRealm ---

func TestExportRealm_Success(t *testing.T) {
	realm := "acme"
	userID := "11111111-1111-1111-1111-111111111111"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"realm":"acme","enabled":true}`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"`+userID+`","username":"a@example.com"}]`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "users", userID, "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"cred-1","type":"otp"}]`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "identity-provider", "instances"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"alias":"idp1"}]`)
		})
	})
	doc, err := a.ExportRealm(context.Background(), realm)
	require.NoError(t, err)
	require.Contains(t, string(doc), `"acme"`)
	require.Contains(t, string(doc), `"idp1"`)
	require.Contains(t, string(doc), `"cred-1"`)
}

func TestExportRealm_GetRealmError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, err := a.ExportRealm(context.Background(), realm)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_realm")
}

func TestExportRealm_GetUsersError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"realm":"acme"}`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, err := a.ExportRealm(context.Background(), realm)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_users")
}

func TestExportRealm_GetCredentialsError(t *testing.T) {
	realm := "acme"
	userID := "11111111-1111-1111-1111-111111111111"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"realm":"acme"}`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"`+userID+`"}]`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "users", userID, "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, err := a.ExportRealm(context.Background(), realm)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_credentials")
}

func TestExportRealm_UserWithNilIDSkipsCredentialsLookup(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"realm":"acme"}`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"username":"no-id"}]`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "identity-provider", "instances"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
	})
	doc, err := a.ExportRealm(context.Background(), realm)
	require.NoError(t, err)
	require.Contains(t, string(doc), `"no-id"`)
}

func TestExportRealm_GetIdpsError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"realm":"acme"}`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "identity-provider", "instances"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, err := a.ExportRealm(context.Background(), realm)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_idps")
}

// --- DeleteRealm / DeleteRealmIfExists ---

func TestDeleteRealm_Success(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("DELETE "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	})
	require.NoError(t, a.DeleteRealm(context.Background(), realm))
}

func TestDeleteRealm_Error(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("DELETE "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.DeleteRealm(context.Background(), realm)
	require.Error(t, err)
}

func TestDeleteRealmIfExists_NotFoundIsSuccess(t *testing.T) {
	realm := "ghost"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("DELETE "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, `{"errorMessage":"realm not found"}`)
		})
	})
	require.NoError(t, a.DeleteRealmIfExists(context.Background(), realm))
}

func TestDeleteRealmIfExists_GenericErrorPropagates(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("DELETE "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.DeleteRealmIfExists(context.Background(), realm)
	require.Error(t, err)
}

func TestDeleteRealmIfExists_Success(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("DELETE "+realmPath(realm), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	})
	require.NoError(t, a.DeleteRealmIfExists(context.Background(), realm))
}
