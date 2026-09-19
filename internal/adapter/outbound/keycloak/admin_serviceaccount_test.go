package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

const testJWKSURL = "http://token-service.iam.svc.cluster.local/api/v1/internal/tenants/11111111-1111-1111-1111-111111111111/service-accounts/platform-automation/jwks.json"

// --- CreateServiceAccountClient ---

func TestCreateServiceAccountClient_Success(t *testing.T) {
	realm := "acme"
	clientID := "platform-automation"
	sub := "22222222-2222-2222-2222-222222222222"
	var capturedBody map[string]any
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&capturedBody))
			writeLocation(w, "internal-client-id")
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id", "service-account-user"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+sub+`"}`)
		})
	})

	got, created, err := a.CreateServiceAccountClient(context.Background(), realm, clientID, testJWKSURL)
	require.NoError(t, err)
	require.Equal(t, uuid.MustParse(sub), got)
	require.True(t, created, "a freshly-minted client must report created=true")

	require.Equal(t, "client-jwt", capturedBody["clientAuthenticatorType"], "must mint a client-jwt client, not plain client-secret (EXT-6)")
	attrs, ok := capturedBody["attributes"].(map[string]any)
	require.True(t, ok, "attributes must be set")
	require.Equal(t, "true", attrs["use.jwks.url"])
	require.Equal(t, testJWKSURL, attrs["jwks.url"])
}

func TestCreateServiceAccountClient_ResumesIfAlreadyExists(t *testing.T) {
	realm := "acme"
	clientID := "platform-automation"
	sub := "22222222-2222-2222-2222-222222222222"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"internal-client-id","clientId":"`+clientID+`"}]`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("must not attempt to create a client that already exists")
		})
		// Already correctly configured — GetClient reports the up-to-date
		// auth config, so ensureClientJWTAuth must skip the PUT entirely.
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"internal-client-id","clientId":"`+clientID+`","clientAuthenticatorType":"client-jwt","attributes":{"use.jwks.url":"true","jwks.url":"`+testJWKSURL+`"}}`)
		})
		mux.HandleFunc("PUT "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("must not update a client that is already correctly configured")
		})
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id", "service-account-user"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+sub+`"}`)
		})
	})

	got, created, err := a.CreateServiceAccountClient(context.Background(), realm, clientID, testJWKSURL)
	require.NoError(t, err)
	require.Equal(t, uuid.MustParse(sub), got)
	require.False(t, created, "a resumed client must report created=false")
}

// A resumed client whose auth config had drifted (an out-of-band edit, or a
// stale pre-EXT-6 client) gets it re-applied via a full-representation
// GET-then-PUT, not silently left on the wrong mechanism.
func TestCreateServiceAccountClient_ResumedClient_ReappliesDriftedAuthConfig(t *testing.T) {
	realm := "acme"
	clientID := "platform-automation"
	sub := "22222222-2222-2222-2222-222222222222"
	var putBody map[string]any
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"internal-client-id","clientId":"`+clientID+`"}]`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			// Drifted: still on the default client-secret auth, no JWKS
			// attributes — must be corrected, not left alone.
			writeJSON(w, http.StatusOK, `{"id":"internal-client-id","clientId":"`+clientID+`","clientAuthenticatorType":"client-secret","name":"kept-field"}`)
		})
		mux.HandleFunc("PUT "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&putBody))
			writeJSON(w, http.StatusNoContent, ``)
		})
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id", "service-account-user"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+sub+`"}`)
		})
	})

	_, created, err := a.CreateServiceAccountClient(context.Background(), realm, clientID, testJWKSURL)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, "client-jwt", putBody["clientAuthenticatorType"])
	attrs, ok := putBody["attributes"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "true", attrs["use.jwks.url"])
	require.Equal(t, testJWKSURL, attrs["jwks.url"])
	require.Equal(t, "kept-field", putBody["name"], "the full representation is preserved, not clobbered")
}

func TestCreateServiceAccountClient_FiltersOutNonExactClientIDMatch(t *testing.T) {
	realm := "acme"
	clientID := "platform-automation"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			// Keycloak's clientId query param may substring-match; a client
			// named "platform-automation-<tenant>" must not be mistaken for
			// the exact base-name client this call is looking for.
			writeJSON(w, http.StatusOK, `[{"id":"other-id","clientId":"platform-automation-`+uuid.New().String()+`"}]`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeLocation(w, "internal-client-id")
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id", "service-account-user"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"22222222-2222-2222-2222-222222222222"}`)
		})
	})

	_, created, err := a.CreateServiceAccountClient(context.Background(), realm, clientID, testJWKSURL)
	require.NoError(t, err, "a non-exact clientId match must not short-circuit the create")
	require.True(t, created)
}

func TestCreateServiceAccountClient_GetClientsError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, _, err := a.CreateServiceAccountClient(context.Background(), realm, "platform-automation", testJWKSURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_clients")
}

func TestCreateServiceAccountClient_CreateClientGenericError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, _, err := a.CreateServiceAccountClient(context.Background(), realm, "platform-automation", testJWKSURL)
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	require.Equal(t, domain.ErrKeycloakUnavailable, derr.Code)
}

// A permanently-inconsistent resource shape (not a transient Keycloak call
// failure) must not be classified as ErrKeycloakUnavailable (502, implies
// retrying will help) — a plain error falls through to 500 internal_error.
func TestCreateServiceAccountClient_ExistingClientHasNoInternalID_NotClassifiedAsKeycloakUnavailable(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[{"clientId":"platform-automation"}]`)
		})
	})
	_, _, err := a.CreateServiceAccountClient(context.Background(), realm, "platform-automation", testJWKSURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no internal id")
	var derr *domain.Error
	require.False(t, errors.As(err, &derr), "must not be classified as a retryable domain.Error")
}

func TestCreateServiceAccountClient_ServiceAccountHasNoLinkedUser_NotClassifiedAsKeycloakUnavailable(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeLocation(w, "internal-client-id")
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id", "service-account-user"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{}`)
		})
	})
	_, _, err := a.CreateServiceAccountClient(context.Background(), realm, "platform-automation", testJWKSURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no linked user")
	var derr *domain.Error
	require.False(t, errors.As(err, &derr), "must not be classified as a retryable domain.Error")
}

func TestCreateServiceAccountClient_ConflictResumesByLookup(t *testing.T) {
	realm := "acme"
	clientID := "platform-automation"
	sub := "22222222-2222-2222-2222-222222222222"
	first := true
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			if first {
				first = false
				writeJSON(w, http.StatusOK, `[]`)
				return
			}
			writeJSON(w, http.StatusOK, `[{"id":"internal-client-id","clientId":"`+clientID+`"}]`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusConflict, `{"errorMessage":"already exists"}`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"internal-client-id","clientId":"`+clientID+`","clientAuthenticatorType":"client-jwt","attributes":{"use.jwks.url":"true","jwks.url":"`+testJWKSURL+`"}}`)
		})
		mux.HandleFunc("PUT "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("must not update a client that is already correctly configured")
		})
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id", "service-account-user"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+sub+`"}`)
		})
	})

	got, created, err := a.CreateServiceAccountClient(context.Background(), realm, clientID, testJWKSURL)
	require.NoError(t, err)
	require.Equal(t, uuid.MustParse(sub), got)
	require.False(t, created, "resuming after our own lost-response conflict is not a fresh create")
}

func TestCreateServiceAccountClient_ConflictResumeLookupFails(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusConflict, `{"errorMessage":"already exists"}`)
		})
	})
	_, _, err := a.CreateServiceAccountClient(context.Background(), realm, "platform-automation", testJWKSURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "resume_after_conflict")
}

func TestCreateServiceAccountClient_GetServiceAccountError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeLocation(w, "internal-client-id")
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id", "service-account-user"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, _, err := a.CreateServiceAccountClient(context.Background(), realm, "platform-automation", testJWKSURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_service_account")
}

func TestCreateServiceAccountClient_ServiceAccountSubNotUUID(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeLocation(w, "internal-client-id")
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id", "service-account-user"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"not-a-uuid"}`)
		})
	})
	_, _, err := a.CreateServiceAccountClient(context.Background(), realm, "platform-automation", testJWKSURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse_sub")
}

// --- ClearServiceAccountKeysCache ---

func TestClearServiceAccountKeysCache_Success(t *testing.T) {
	realm := "acme"
	called := false
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath(realm, "clear-keys-cache"), func(w http.ResponseWriter, r *http.Request) {
			called = true
			writeJSON(w, http.StatusNoContent, ``)
		})
	})

	err := a.ClearServiceAccountKeysCache(context.Background(), realm)
	require.NoError(t, err)
	require.True(t, called)
}

func TestClearServiceAccountKeysCache_Error(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath(realm, "clear-keys-cache"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})

	err := a.ClearServiceAccountKeysCache(context.Background(), realm)
	require.Error(t, err)
	require.Contains(t, err.Error(), "clear_service_account_keys_cache")
}

// --- DeleteServiceAccountClientIfExists ---

func TestDeleteServiceAccountClientIfExists_Success(t *testing.T) {
	realm := "trial"
	clientID := "platform-automation-11111111-1111-1111-1111-111111111111"
	deleted := false
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"internal-client-id","clientId":"`+clientID+`"}]`)
		})
		mux.HandleFunc("DELETE "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			deleted = true
			writeJSON(w, http.StatusNoContent, ``)
		})
	})

	err := a.DeleteServiceAccountClientIfExists(context.Background(), realm, clientID)
	require.NoError(t, err)
	require.True(t, deleted)
}

// Idempotent: a client that no longer exists (already cleaned up by a
// previous attempt) is a no-op success, mirroring DeleteRealmIfExists.
func TestDeleteServiceAccountClientIfExists_AlreadyGone_NoOp(t *testing.T) {
	realm := "trial"
	clientID := "platform-automation-11111111-1111-1111-1111-111111111111"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("DELETE "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("must not attempt to delete a client that was never found")
		})
	})

	err := a.DeleteServiceAccountClientIfExists(context.Background(), realm, clientID)
	require.NoError(t, err)
}

func TestDeleteServiceAccountClientIfExists_GetClientsError(t *testing.T) {
	realm := "trial"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})

	err := a.DeleteServiceAccountClientIfExists(context.Background(), realm, "platform-automation")
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_clients")
}

func TestDeleteServiceAccountClientIfExists_DeleteError(t *testing.T) {
	realm := "trial"
	clientID := "platform-automation"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"internal-client-id","clientId":"`+clientID+`"}]`)
		})
		mux.HandleFunc("DELETE "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})

	err := a.DeleteServiceAccountClientIfExists(context.Background(), realm, clientID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "delete_service_account_client_if_exists")
}

// A 404 from the DELETE itself (a race: found by our own GET moments ago,
// gone by the time DELETE lands) is idempotent success, not an error —
// distinct from TestDeleteServiceAccountClientIfExists_AlreadyGone_NoOp,
// which never finds the client via GET in the first place.
func TestDeleteServiceAccountClientIfExists_DeleteReturns404_TreatedAsAlreadyGone(t *testing.T) {
	realm := "trial"
	clientID := "platform-automation"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"internal-client-id","clientId":"`+clientID+`"}]`)
		})
		mux.HandleFunc("DELETE "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, `{}`)
		})
	})

	err := a.DeleteServiceAccountClientIfExists(context.Background(), realm, clientID)
	require.NoError(t, err)
}

func TestDeleteServiceAccountClientIfExists_TokenError(t *testing.T) {
	a := newFakeAdmin(t, []tokenResponse{failTokenResponse()}, nil)
	err := a.DeleteServiceAccountClientIfExists(context.Background(), "trial", "platform-automation")
	require.Error(t, err)
	require.Contains(t, err.Error(), "delete_service_account_client_if_exists: token")
}

func TestClearServiceAccountKeysCache_TokenError(t *testing.T) {
	a := newFakeAdmin(t, []tokenResponse{failTokenResponse()}, nil)
	err := a.ClearServiceAccountKeysCache(context.Background(), "trial")
	require.Error(t, err)
	require.Contains(t, err.Error(), "clear_service_account_keys_cache: token")
}

func TestCreateServiceAccountClient_TokenError(t *testing.T) {
	a := newFakeAdmin(t, []tokenResponse{failTokenResponse()}, nil)
	_, _, err := a.CreateServiceAccountClient(context.Background(), "acme", "platform-automation", testJWKSURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "create_service_account_client: token")
}

// --- ensureClientJWTAuth (exercised via CreateServiceAccountClient's resume path) ---

func TestCreateServiceAccountClient_ResumedClient_GetClientFails(t *testing.T) {
	realm := "acme"
	clientID := "platform-automation"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"internal-client-id","clientId":"`+clientID+`"}]`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})

	_, _, err := a.CreateServiceAccountClient(context.Background(), realm, clientID, testJWKSURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reapply_auth_config")
}

func TestCreateServiceAccountClient_ResumedClient_UpdateClientFails(t *testing.T) {
	realm := "acme"
	clientID := "platform-automation"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"internal-client-id","clientId":"`+clientID+`"}]`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			// Drifted, so ensureClientJWTAuth attempts the PUT.
			writeJSON(w, http.StatusOK, `{"id":"internal-client-id","clientId":"`+clientID+`","clientAuthenticatorType":"client-secret"}`)
		})
		mux.HandleFunc("PUT "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})

	_, _, err := a.CreateServiceAccountClient(context.Background(), realm, clientID, testJWKSURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reapply_auth_config")
}
