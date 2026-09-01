package keycloak

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
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
			writeJSON(w, http.StatusConflict, `{"errorMessage":"user exists"}`)
		})
	})
	_, err := a.CreateTrialUser(context.Background(), port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: uuid.New()})
	require.Error(t, err)
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
