package keycloak

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// --- CreateTrialUser ---

func TestCreateTrialUser_Success(t *testing.T) {
	newID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath("trial", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeLocation(w, newID.String())
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("PUT "+realmPath("trial", "users", newID.String(), "send-verify-email"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNoContent, ``)
		})
	})
	id, err := a.CreateTrialUser(context.Background(), port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: uuid.New()})
	require.NoError(t, err)
	require.Equal(t, newID, id)
}

func TestCreateTrialUser_CreateError(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath("trial", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, err := a.CreateTrialUser(context.Background(), port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: uuid.New()})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrKeycloakUnavailable, de.Code, "a non-409 failure keeps wrapErr's generic semantics")
}

// A 409 specifically must surface as the distinguishable ErrUserEmailConflict
// (§9.2) — not the generic ErrKeycloakUnavailable every other CreateUser
// failure produces — so provisioning_service.go can tell "email already
// exists" apart from "keycloak is down" and attempt lost-response-retry
// resolution via FindUserByEmail.
func TestCreateTrialUser_ConflictReturnsDistinguishableCode(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath("trial", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusConflict, `{"errorMessage":"User exists with same email"}`)
		})
	})
	_, err := a.CreateTrialUser(context.Background(), port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: uuid.New()})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrUserEmailConflict, de.Code)
}

func TestCreateTrialUser_SendVerifyEmailFailureIsWarnedNotFatal(t *testing.T) {
	newID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath("trial", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeLocation(w, newID.String())
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("PUT "+realmPath("trial", "users", newID.String(), "send-verify-email"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	id, err := a.CreateTrialUser(context.Background(), port.CreateUserRequest{
		Email: "a@example.com", FullName: "A", TenantID: uuid.New(), RequiredActions: []string{"VERIFY_EMAIL"},
	})
	require.NoError(t, err, "a failed verify-email send must not fail user creation")
	require.Equal(t, newID, id)
}

// --- CreateInvitedUser ---

func TestCreateInvitedUser_Success(t *testing.T) {
	newID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeLocation(w, newID.String())
			writeJSON(w, http.StatusCreated, `{}`)
		})
	})
	id, err := a.CreateInvitedUser(context.Background(), "acme", port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: uuid.New()})
	require.NoError(t, err)
	require.Equal(t, newID, id)
}

func TestCreateInvitedUser_Error(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, err := a.CreateInvitedUser(context.Background(), "acme", port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: uuid.New()})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrKeycloakUnavailable, de.Code, "a non-409 failure keeps wrapErr's generic semantics")
}

func TestCreateInvitedUser_ConflictReturnsDistinguishableCode(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusConflict, `{"errorMessage":"User exists with same email"}`)
		})
	})
	_, err := a.CreateInvitedUser(context.Background(), "acme", port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: uuid.New()})
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrUserEmailConflict, de.Code)
}

// --- FindUserByEmail ---

func TestFindUserByEmail_Found_ParsesTenantIDAttribute(t *testing.T) {
	userID := uuid.New()
	tenantID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "a@example.com", r.URL.Query().Get("email"))
			assert.Equal(t, "true", r.URL.Query().Get("exact"))
			writeJSON(w, http.StatusOK, `[{"id":"`+userID.String()+`","attributes":{"tenant_id":["`+tenantID.String()+`"]}}]`)
		})
	})
	existing, err := a.FindUserByEmail(context.Background(), "acme", "a@example.com")
	require.NoError(t, err)
	assert.Equal(t, userID, existing.ID)
	assert.Equal(t, tenantID, existing.TenantID)
}

func TestFindUserByEmail_Found_NoAttributes_TenantIDStaysNil(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"`+userID.String()+`"}]`)
		})
	})
	existing, err := a.FindUserByEmail(context.Background(), "acme", "a@example.com")
	require.NoError(t, err)
	assert.Equal(t, userID, existing.ID)
	assert.Equal(t, uuid.Nil, existing.TenantID, "a legacy/attribute-less user must never appear to match a real tenant id")
}

func TestFindUserByEmail_NotFound_ReturnsDistinguishableCode(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
	})
	_, err := a.FindUserByEmail(context.Background(), "acme", "nobody@example.com")
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrKeycloakUserNotFound, de.Code)
}

func TestFindUserByEmail_ListError(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, err := a.FindUserByEmail(context.Background(), "acme", "a@example.com")
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrKeycloakUnavailable, de.Code)
}

func TestFindUserByEmail_TokenError(t *testing.T) {
	a := newFakeAdmin(t, []tokenResponse{failTokenResponse()}, nil)
	_, err := a.FindUserByEmail(context.Background(), "acme", "a@example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "find_user_by_email: token")
}

func TestFindUserByEmail_NonUUIDID_ParseError(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"not-a-uuid"}]`)
		})
	})
	_, err := a.FindUserByEmail(context.Background(), "acme", "a@example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "find_user_by_email: parse id")
}

// --- MigrateUser / partialImportUser ---

func TestMigrateUser_Success_WithTotpAndWebauthn(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+userID.String()+`","username":"a@example.com"}`)
		})
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"c1","type":"otp"},{"id":"c2","type":"webauthn"}]`)
		})
		mux.HandleFunc("DELETE "+realmPath("trial", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("POST "+realmPath("acme", "partialImport"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"added":1,"skipped":0,"overwritten":0}`)
		})
	})
	reenroll, err := a.MigrateUser(context.Background(), "trial", "acme", userID)
	require.NoError(t, err)
	require.True(t, reenroll, "a webauthn credential on the source must flag re-enrollment")
}

func TestMigrateUser_Success_NoCredentials(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+userID.String()+`","username":"a@example.com","requiredActions":["VERIFY_EMAIL"]}`)
		})
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("DELETE "+realmPath("trial", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("POST "+realmPath("acme", "partialImport"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"added":1,"skipped":0,"overwritten":0}`)
		})
	})
	reenroll, err := a.MigrateUser(context.Background(), "trial", "acme", userID)
	require.NoError(t, err)
	require.False(t, reenroll)
}

func TestMigrateUser_Success_NilTypeCredentialSkipped_AndExistingRequiredActionsPreserved(t *testing.T) {
	userID := uuid.New()
	var importedBody []byte
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+userID.String()+`","username":"a@example.com","requiredActions":["VERIFY_EMAIL"]}`)
		})
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			// includes a credential with a nil/absent "type" field, which
			// MigrateUser must skip rather than dereference.
			writeJSON(w, http.StatusOK, `[{"id":"c-no-type"},{"id":"c-wa","type":"webauthn"}]`)
		})
		mux.HandleFunc("DELETE "+realmPath("trial", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("POST "+realmPath("acme", "partialImport"), func(w http.ResponseWriter, r *http.Request) {
			buf, _ := io.ReadAll(r.Body)
			importedBody = buf
			writeJSON(w, http.StatusOK, `{"added":1,"skipped":0,"overwritten":0}`)
		})
	})
	reenroll, err := a.MigrateUser(context.Background(), "trial", "acme", userID)
	require.NoError(t, err)
	require.True(t, reenroll)
	require.Contains(t, string(importedBody), "VERIFY_EMAIL")
	require.Contains(t, string(importedBody), "webauthn-register")
}

func TestMigrateUser_PartialImportRealTransportError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+userID.String()+`"}`)
		})
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("DELETE "+realmPath("trial", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("POST "+realmPath("acme", "partialImport"), hijackAndClose(t))
	})
	_, err := a.MigrateUser(context.Background(), "trial", "acme", userID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "partial_import")
}

func TestMigrateUser_GetUserError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, err := a.MigrateUser(context.Background(), "trial", "acme", userID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_user")
}

func TestMigrateUser_GetCredentialsError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+userID.String()+`"}`)
		})
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, err := a.MigrateUser(context.Background(), "trial", "acme", userID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_credentials")
}

func TestMigrateUser_DeleteSourceError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+userID.String()+`"}`)
		})
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("DELETE "+realmPath("trial", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, err := a.MigrateUser(context.Background(), "trial", "acme", userID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "delete_source")
}

func TestMigrateUser_PartialImportError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+userID.String()+`"}`)
		})
		mux.HandleFunc("GET "+realmPath("trial", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("DELETE "+realmPath("trial", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("POST "+realmPath("acme", "partialImport"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, err := a.MigrateUser(context.Background(), "trial", "acme", userID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "partial_import")
}

// --- partialImportUser (deeper decode/count branches, exercised directly) ---

func TestPartialImportUser_DecodeError(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath(realm, "partialImport"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `not json`)
		})
	})
	tok, err := a.token(context.Background())
	require.NoError(t, err)
	err = a.partialImportUser(context.Background(), tok, realm, buildUser(port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: uuid.New()}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "decode response")
}

func TestPartialImportUser_SkippedNotAdded(t *testing.T) {
	realm := "acme"
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath(realm, "partialImport"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"added":0,"skipped":1,"overwritten":0}`)
		})
	})
	tok, err := a.token(context.Background())
	require.NoError(t, err)
	err = a.partialImportUser(context.Background(), tok, realm, buildUser(port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: uuid.New()}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected 1 user added")
}

func TestPartialImportUser_TransportError(t *testing.T) {
	a := newFakeAdmin(t, nil, nil) // no route registered for partialImport -> gocloak's resty escape hatch still succeeds the round trip (404), IsError() true
	tok, err := a.token(context.Background())
	require.NoError(t, err)
	err = a.partialImportUser(context.Background(), tok, "acme", buildUser(port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: uuid.New()}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unexpected status")
}

// --- ListUsersByTenant ---

func TestListUsersByTenant_Success(t *testing.T) {
	tenantID := uuid.New()
	id1 := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"`+id1.String()+`"},{"username":"no-id"},{"id":"not-a-uuid"}]`)
		})
	})
	ids, err := a.ListUsersByTenant(context.Background(), "acme", tenantID)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{id1}, ids, "a nil-ID user is skipped and a non-UUID id is dropped")
}

func TestListUsersByTenant_ListError(t *testing.T) {
	tenantID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	_, err := a.ListUsersByTenant(context.Background(), "acme", tenantID)
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrKeycloakUnavailable, de.Code, "a generic 5xx must not be mistaken for realm-not-found")
}

// CRITICAL: a 404 on this endpoint specifically means the realm itself is
// absent (a realm that exists but matches zero users returns 200 with an
// empty list, never a 404) — RevertConversion relies on this exact
// distinction to detect "I'm resuming a retry whose earlier attempt
// already deleted this realm" rather than treating it as a generic
// failure that could permanently strand the caller.
func TestListUsersByTenant_RealmNotFound_ReturnsDistinguishableCode(t *testing.T) {
	tenantID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, `{"error":"Realm not found."}`)
		})
	})
	_, err := a.ListUsersByTenant(context.Background(), "acme", tenantID)
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrTenantRealmNotFound, de.Code)
}

// --- DisableUsersInRealm / EnableUsersInRealm ---

func TestDisableUsersInRealm_Success(t *testing.T) {
	tenantID := uuid.New()
	id1 := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"`+id1.String()+`","enabled":true}]`)
		})
		mux.HandleFunc("PUT "+realmPath("acme", "users", id1.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{}`)
		})
	})
	require.NoError(t, a.DisableUsersInRealm(context.Background(), "acme", tenantID))
}

func TestDisableUsersInRealm_ListError(t *testing.T) {
	tenantID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.DisableUsersInRealm(context.Background(), "acme", tenantID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "disable_users: list")
}

func TestDisableUsersInRealm_UpdateError(t *testing.T) {
	tenantID := uuid.New()
	id1 := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"`+id1.String()+`"}]`)
		})
		mux.HandleFunc("PUT "+realmPath("acme", "users", id1.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.DisableUsersInRealm(context.Background(), "acme", tenantID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "disable_users: update")
}

func TestEnableUsersInRealm_Success(t *testing.T) {
	tenantID := uuid.New()
	id1 := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"`+id1.String()+`","enabled":false}]`)
		})
		mux.HandleFunc("PUT "+realmPath("acme", "users", id1.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{}`)
		})
	})
	require.NoError(t, a.EnableUsersInRealm(context.Background(), "acme", tenantID))
}

func TestEnableUsersInRealm_ListError(t *testing.T) {
	tenantID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.EnableUsersInRealm(context.Background(), "acme", tenantID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "enable_users: list")
}

func TestEnableUsersInRealm_UpdateError(t *testing.T) {
	tenantID := uuid.New()
	id1 := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"`+id1.String()+`"}]`)
		})
		mux.HandleFunc("PUT "+realmPath("acme", "users", id1.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.EnableUsersInRealm(context.Background(), "acme", tenantID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "enable_users: update")
}

// --- HardDeleteUsersInRealm ---

func TestHardDeleteUsersInRealm_Success(t *testing.T) {
	tenantID := uuid.New()
	id1 := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"`+id1.String()+`"},{"username":"no-id"}]`)
		})
		mux.HandleFunc("DELETE "+realmPath("acme", "users", id1.String()), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	})
	require.NoError(t, a.HardDeleteUsersInRealm(context.Background(), "acme", tenantID))
}

func TestHardDeleteUsersInRealm_ListError(t *testing.T) {
	tenantID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.HardDeleteUsersInRealm(context.Background(), "acme", tenantID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "hard_delete_users: list")
}

func TestHardDeleteUsersInRealm_NotFoundIsIgnored(t *testing.T) {
	tenantID := uuid.New()
	id1 := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"`+id1.String()+`"}]`)
		})
		mux.HandleFunc("DELETE "+realmPath("acme", "users", id1.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, `{}`)
		})
	})
	require.NoError(t, a.HardDeleteUsersInRealm(context.Background(), "acme", tenantID))
}

func TestHardDeleteUsersInRealm_GenericErrorPropagates(t *testing.T) {
	tenantID := uuid.New()
	id1 := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"`+id1.String()+`"}]`)
		})
		mux.HandleFunc("DELETE "+realmPath("acme", "users", id1.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.HardDeleteUsersInRealm(context.Background(), "acme", tenantID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "hard_delete_users: delete")
}

// --- DeleteUser ---

func TestDeleteUser_Success(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("DELETE "+realmPath("acme", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	})
	require.NoError(t, a.DeleteUser(context.Background(), "acme", userID))
}

func TestDeleteUser_NotFoundIsIdempotent(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("DELETE "+realmPath("acme", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, `{}`)
		})
	})
	require.NoError(t, a.DeleteUser(context.Background(), "acme", userID))
}

func TestDeleteUser_GenericErrorPropagates(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("DELETE "+realmPath("acme", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.DeleteUser(context.Background(), "acme", userID)
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	require.Equal(t, domain.ErrKeycloakUnavailable, derr.Code)
}

// --- RevokeUserSessions ---

func TestRevokeUserSessions_Success(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath("acme", "users", userID.String(), "logout"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{}`)
		})
	})
	require.NoError(t, a.RevokeUserSessions(context.Background(), "acme", userID))
}

func TestRevokeUserSessions_NotFoundIsIdempotent(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath("acme", "users", userID.String(), "logout"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, `{}`)
		})
	})
	require.NoError(t, a.RevokeUserSessions(context.Background(), "acme", userID))
}

func TestRevokeUserSessions_GenericErrorPropagates(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath("acme", "users", userID.String(), "logout"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.RevokeUserSessions(context.Background(), "acme", userID)
	require.Error(t, err)
}

// --- ForceLogoutRealm ---

func TestForceLogoutRealm_Success(t *testing.T) {
	id1 := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"`+id1.String()+`"},{"username":"no-id"}]`)
		})
		mux.HandleFunc("POST "+realmPath("acme", "users", id1.String(), "logout"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{}`)
		})
	})
	require.NoError(t, a.ForceLogoutRealm(context.Background(), "acme"))
}

func TestForceLogoutRealm_ListUsersError(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.ForceLogoutRealm(context.Background(), "acme")
	require.Error(t, err)
	require.Contains(t, err.Error(), "list_users")
}

func TestForceLogoutRealm_LogoutNotFoundIsIgnored(t *testing.T) {
	id1 := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"`+id1.String()+`"}]`)
		})
		mux.HandleFunc("POST "+realmPath("acme", "users", id1.String(), "logout"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, `{}`)
		})
	})
	require.NoError(t, a.ForceLogoutRealm(context.Background(), "acme"))
}

func TestForceLogoutRealm_LogoutGenericErrorPropagates(t *testing.T) {
	id1 := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"`+id1.String()+`"}]`)
		})
		mux.HandleFunc("POST "+realmPath("acme", "users", id1.String(), "logout"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.ForceLogoutRealm(context.Background(), "acme")
	require.Error(t, err)
	require.Contains(t, err.Error(), "force_logout_realm: logout")
}

// --- Action-email links (HLD §8.2.7) ---

func TestCreateTrialUser_VerifyEmailCarriesLink(t *testing.T) {
	newID := uuid.New()
	var query url.Values
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath("trial", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeLocation(w, newID.String())
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("PUT "+realmPath("trial", "users", newID.String(), "send-verify-email"), func(w http.ResponseWriter, r *http.Request) {
			query = r.URL.Query()
			writeJSON(w, http.StatusNoContent, ``)
		})
	})
	_, err := a.CreateTrialUser(context.Background(), port.CreateUserRequest{
		Email: "a@example.com", FullName: "A", TenantID: uuid.New(),
		Link: &port.ActionLink{ClientID: "tender-web", RedirectURI: "https://app.example/t/acme/"},
	})
	require.NoError(t, err)
	assert.Equal(t, "tender-web", query.Get("client_id"))
	assert.Equal(t, "https://app.example/t/acme/", query.Get("redirect_uri"))
}

// A link Keycloak refuses (no tender-web client in the trial realm, or an
// unregistered redirect URI) must cost only the link, never the email.
func TestCreateTrialUser_RefusedLink_RetriesWithoutIt(t *testing.T) {
	newID := uuid.New()
	var calls []url.Values
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("POST "+realmPath("trial", "users"), func(w http.ResponseWriter, _ *http.Request) {
			writeLocation(w, newID.String())
			writeJSON(w, http.StatusCreated, `{}`)
		})
		mux.HandleFunc("PUT "+realmPath("trial", "users", newID.String(), "send-verify-email"), func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, r.URL.Query())
			if r.URL.Query().Get("client_id") != "" {
				writeJSON(w, http.StatusBadRequest, `{"errorMessage":"Client doesn't exist"}`)
				return
			}
			writeJSON(w, http.StatusNoContent, ``)
		})
	})
	_, err := a.CreateTrialUser(context.Background(), port.CreateUserRequest{
		Email: "a@example.com", FullName: "A", TenantID: uuid.New(),
		Link: &port.ActionLink{ClientID: "tender-web", RedirectURI: "https://app.example/t/acme/"},
	})
	require.NoError(t, err)
	require.Len(t, calls, 2)
	assert.Empty(t, calls[1].Get("client_id"), "the retry sends the plain verify email")
}

func TestSendActionsEmail_SendsActionsLifespanAndLink(t *testing.T) {
	userID := uuid.New()
	var query url.Values
	var actions []string
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("PUT "+realmPath("tenant-acme", "users", userID.String(), "execute-actions-email"), func(w http.ResponseWriter, r *http.Request) {
			query = r.URL.Query()
			body, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(body, &actions))
			writeJSON(w, http.StatusNoContent, ``)
		})
	})
	err := a.SendActionsEmail(context.Background(), "tenant-acme", userID, []string{"VERIFY_EMAIL", "UPDATE_PASSWORD"}, 7*24*time.Hour,
		&port.ActionLink{ClientID: "tender-web", RedirectURI: "https://app.example/t/acme/"})
	require.NoError(t, err)
	assert.Equal(t, []string{"VERIFY_EMAIL", "UPDATE_PASSWORD"}, actions)
	assert.Equal(t, "604800", query.Get("lifespan"))
	assert.Equal(t, "tender-web", query.Get("client_id"))
	assert.Equal(t, "https://app.example/t/acme/", query.Get("redirect_uri"))
}

func TestSendActionsEmail_Error(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("PUT "+realmPath("tenant-acme", "users", userID.String(), "execute-actions-email"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.SendActionsEmail(context.Background(), "tenant-acme", userID, []string{"VERIFY_EMAIL"}, time.Hour, nil)
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrKeycloakUnavailable, de.Code)
}
