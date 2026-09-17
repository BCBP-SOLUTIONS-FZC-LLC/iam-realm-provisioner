package keycloak

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// --- CreateServiceAccountClient ---

func TestCreateServiceAccountClient_Success(t *testing.T) {
	realm := "acme"
	clientID := "platform-automation"
	sub := "22222222-2222-2222-2222-222222222222"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("POST "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeLocation(w, "internal-client-id")
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id", "service-account-user"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+sub+`"}`)
		})
	})

	got, err := a.CreateServiceAccountClient(context.Background(), realm, clientID)
	require.NoError(t, err)
	require.Equal(t, uuid.MustParse(sub), got)
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
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id", "service-account-user"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+sub+`"}`)
		})
	})

	got, err := a.CreateServiceAccountClient(context.Background(), realm, clientID)
	require.NoError(t, err)
	require.Equal(t, uuid.MustParse(sub), got)
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

	_, err := a.CreateServiceAccountClient(context.Background(), realm, clientID)
	require.NoError(t, err, "a non-exact clientId match must not short-circuit the create")
}

func TestCreateServiceAccountClient_GetClientsError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, err := a.CreateServiceAccountClient(context.Background(), realm, "platform-automation")
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
	_, err := a.CreateServiceAccountClient(context.Background(), realm, "platform-automation")
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	require.Equal(t, domain.ErrKeycloakUnavailable, derr.Code)
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
		mux.HandleFunc("GET "+realmPath(realm, "clients", "internal-client-id", "service-account-user"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+sub+`"}`)
		})
	})

	got, err := a.CreateServiceAccountClient(context.Background(), realm, clientID)
	require.NoError(t, err)
	require.Equal(t, uuid.MustParse(sub), got)
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
	_, err := a.CreateServiceAccountClient(context.Background(), realm, "platform-automation")
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
	_, err := a.CreateServiceAccountClient(context.Background(), realm, "platform-automation")
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
	_, err := a.CreateServiceAccountClient(context.Background(), realm, "platform-automation")
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse_sub")
}

// --- SetClientSecret ---

func TestSetClientSecret_Success(t *testing.T) {
	realm := "acme"
	clientID := "platform-automation"
	var capturedBody map[string]any
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"internal-client-id","clientId":"`+clientID+`"}]`)
		})
		mux.HandleFunc("PUT "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&capturedBody))
			writeJSON(w, http.StatusOK, `{}`)
		})
	})

	err := a.SetClientSecret(context.Background(), realm, clientID, "s3cr3t")
	require.NoError(t, err)
	require.Equal(t, "s3cr3t", capturedBody["secret"])
}

func TestSetClientSecret_GetClientsError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.SetClientSecret(context.Background(), realm, "platform-automation", "s3cr3t")
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_clients")
}

func TestSetClientSecret_ClientNotFound(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
	})
	err := a.SetClientSecret(context.Background(), realm, "platform-automation", "s3cr3t")
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	require.Equal(t, domain.ErrKeycloakUnavailable, derr.Code)
}

func TestSetClientSecret_UpdateClientError(t *testing.T) {
	realm := "acme"
	clientID := "platform-automation"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath(realm, "clients"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"internal-client-id","clientId":"`+clientID+`"}]`)
		})
		mux.HandleFunc("PUT "+realmPath(realm, "clients", "internal-client-id"), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.SetClientSecret(context.Background(), realm, clientID, "s3cr3t")
	require.Error(t, err)
}
