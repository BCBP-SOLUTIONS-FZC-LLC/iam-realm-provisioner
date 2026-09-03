package keycloak

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

func roleMappingsPath(realm, userID string) string {
	return realmPath(realm, "users", userID, "role-mappings", "realm")
}

// --- hasRealmRole / AssignRequiresMFA / RevokeRequiresMFA / HasRequiresMFA ---

func TestHasRealmRole_True(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"name":"requires-mfa"},{"name":"other-role"}]`)
		})
	})
	tok, err := a.token(context.Background())
	require.NoError(t, err)
	has, err := a.hasRealmRole(context.Background(), tok, "acme", userID)
	require.NoError(t, err)
	require.True(t, has)
}

func TestHasRealmRole_False(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"name":"other-role"},{}]`)
		})
	})
	tok, err := a.token(context.Background())
	require.NoError(t, err)
	has, err := a.hasRealmRole(context.Background(), tok, "acme", userID)
	require.NoError(t, err)
	require.False(t, has)
}

func TestHasRealmRole_NotFoundIsFalseNoError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, `{}`)
		})
	})
	tok, err := a.token(context.Background())
	require.NoError(t, err)
	has, err := a.hasRealmRole(context.Background(), tok, "acme", userID)
	require.NoError(t, err)
	require.False(t, has)
}

func TestHasRealmRole_GenericErrorPropagates(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	tok, err := a.token(context.Background())
	require.NoError(t, err)
	_, err = a.hasRealmRole(context.Background(), tok, "acme", userID)
	require.Error(t, err)
}

func TestAssignRequiresMFA_AlreadyHasRole_NoOp(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"name":"requires-mfa"}]`)
		})
	})
	require.NoError(t, a.AssignRequiresMFA(context.Background(), "acme", userID))
}

func TestAssignRequiresMFA_HasRealmRoleError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.AssignRequiresMFA(context.Background(), "acme", userID)
	require.Error(t, err)
}

func TestAssignRequiresMFA_GetRoleError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("GET "+realmPath("acme", "roles", RequiresMFARoleName), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.AssignRequiresMFA(context.Background(), "acme", userID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_role")
}

func TestAssignRequiresMFA_AddRoleError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("GET "+realmPath("acme", "roles", RequiresMFARoleName), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"role-id","name":"requires-mfa"}`)
		})
		mux.HandleFunc("POST "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.AssignRequiresMFA(context.Background(), "acme", userID)
	require.Error(t, err)
}

func TestAssignRequiresMFA_Success(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("GET "+realmPath("acme", "roles", RequiresMFARoleName), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"role-id","name":"requires-mfa"}`)
		})
		mux.HandleFunc("POST "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{}`)
		})
	})
	require.NoError(t, a.AssignRequiresMFA(context.Background(), "acme", userID))
}

func TestRevokeRequiresMFA_DoesNotHaveRole_NoOp(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
	})
	require.NoError(t, a.RevokeRequiresMFA(context.Background(), "acme", userID))
}

func TestRevokeRequiresMFA_HasRealmRoleError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.RevokeRequiresMFA(context.Background(), "acme", userID)
	require.Error(t, err)
}

func TestRevokeRequiresMFA_GetRoleError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"name":"requires-mfa"}]`)
		})
		mux.HandleFunc("GET "+realmPath("acme", "roles", RequiresMFARoleName), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.RevokeRequiresMFA(context.Background(), "acme", userID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_role")
}

func TestRevokeRequiresMFA_DeleteRoleError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"name":"requires-mfa"}]`)
		})
		mux.HandleFunc("GET "+realmPath("acme", "roles", RequiresMFARoleName), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"role-id","name":"requires-mfa"}`)
		})
		mux.HandleFunc("DELETE "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.RevokeRequiresMFA(context.Background(), "acme", userID)
	require.Error(t, err)
}

func TestRevokeRequiresMFA_Success(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"name":"requires-mfa"}]`)
		})
		mux.HandleFunc("GET "+realmPath("acme", "roles", RequiresMFARoleName), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"role-id","name":"requires-mfa"}`)
		})
		mux.HandleFunc("DELETE "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNoContent, ``)
		})
	})
	require.NoError(t, a.RevokeRequiresMFA(context.Background(), "acme", userID))
}

func TestHasRequiresMFA_Success(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+roleMappingsPath("acme", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"name":"requires-mfa"}]`)
		})
	})
	has, err := a.HasRequiresMFA(context.Background(), "acme", userID)
	require.NoError(t, err)
	require.True(t, has)
}

// --- ResetMFA ---

func TestResetMFA_NotFoundCredentialsIsNoOp(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, `{}`)
		})
	})
	require.NoError(t, a.ResetMFA(context.Background(), "acme", userID, false))
}

func TestResetMFA_GetCredentialsGenericError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.ResetMFA(context.Background(), "acme", userID, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_credentials")
}

func TestResetMFA_DeletesOtpAndWebauthn_SkipsPassword(t *testing.T) {
	userID := uuid.New()
	var deleted []string
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"c-otp","type":"otp"},{"id":"c-wa","type":"webauthn"},{"id":"c-wap","type":"webauthn-passwordless"},{"id":"c-pw","type":"password"}]`)
		})
		mux.HandleFunc("DELETE "+realmPath("acme", "users", userID.String(), "credentials", "c-otp"), func(w http.ResponseWriter, _ *http.Request) {
			deleted = append(deleted, "c-otp")
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("DELETE "+realmPath("acme", "users", userID.String(), "credentials", "c-wa"), func(w http.ResponseWriter, _ *http.Request) {
			deleted = append(deleted, "c-wa")
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("DELETE "+realmPath("acme", "users", userID.String(), "credentials", "c-wap"), func(w http.ResponseWriter, _ *http.Request) {
			deleted = append(deleted, "c-wap")
			w.WriteHeader(http.StatusNoContent)
		})
	})
	require.NoError(t, a.ResetMFA(context.Background(), "acme", userID, false))
	require.ElementsMatch(t, []string{"c-otp", "c-wa", "c-wap"}, deleted)
}

func TestResetMFA_DeleteCredentialNotFoundIsIgnored(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"c-otp","type":"otp"}]`)
		})
		mux.HandleFunc("DELETE "+realmPath("acme", "users", userID.String(), "credentials", "c-otp"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, `{}`)
		})
	})
	require.NoError(t, a.ResetMFA(context.Background(), "acme", userID, false))
}

func TestResetMFA_DeleteCredentialGenericErrorPropagates(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"id":"c-otp","type":"otp"}]`)
		})
		mux.HandleFunc("DELETE "+realmPath("acme", "users", userID.String(), "credentials", "c-otp"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.ResetMFA(context.Background(), "acme", userID, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "delete_credential")
}

func TestResetMFA_CredentialWithNilTypeOrIDSkipped(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[{"type":"otp"},{"id":"no-type"}]`)
		})
	})
	require.NoError(t, a.ResetMFA(context.Background(), "acme", userID, false))
}

func TestResetMFA_RequiresMFA_AddsConfigureTotp(t *testing.T) {
	userID := uuid.New()
	var putUserBody []byte
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("GET "+realmPath("acme", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+userID.String()+`","requiredActions":["VERIFY_EMAIL"]}`)
		})
		mux.HandleFunc("PUT "+realmPath("acme", "users", userID.String()), func(w http.ResponseWriter, r *http.Request) {
			buf, _ := io.ReadAll(r.Body)
			putUserBody = buf
			writeJSON(w, http.StatusOK, `{}`)
		})
	})
	require.NoError(t, a.ResetMFA(context.Background(), "acme", userID, true))
	require.Contains(t, string(putUserBody), "CONFIGURE_TOTP")
	require.Contains(t, string(putUserBody), "VERIFY_EMAIL")
}

// A repeated ResetMFA for the same user (no idempotency ledger backs
// RP-9 — fail-closed, no reconciler) must not accumulate duplicate
// CONFIGURE_TOTP entries in requiredActions.
func TestResetMFA_RequiresMFA_AlreadyPresent_NotDuplicated(t *testing.T) {
	userID := uuid.New()
	var putUserBody []byte
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("GET "+realmPath("acme", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+userID.String()+`","requiredActions":["VERIFY_EMAIL","CONFIGURE_TOTP"]}`)
		})
		mux.HandleFunc("PUT "+realmPath("acme", "users", userID.String()), func(w http.ResponseWriter, r *http.Request) {
			buf, _ := io.ReadAll(r.Body)
			putUserBody = buf
			writeJSON(w, http.StatusOK, `{}`)
		})
	})
	require.NoError(t, a.ResetMFA(context.Background(), "acme", userID, true))
	var body struct {
		RequiredActions []string `json:"requiredActions"`
	}
	require.NoError(t, json.Unmarshal(putUserBody, &body))
	count := 0
	for _, a := range body.RequiredActions {
		if a == "CONFIGURE_TOTP" {
			count++
		}
	}
	assert.Equal(t, 1, count, "CONFIGURE_TOTP must appear exactly once, not accumulate across repeated resets")
	assert.Contains(t, body.RequiredActions, "VERIFY_EMAIL")
}

func TestResetMFA_RequiresMFA_GetUserError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("GET "+realmPath("acme", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.ResetMFA(context.Background(), "acme", userID, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_user")
}

func TestResetMFA_RequiresMFA_UpdateUserError(t *testing.T) {
	userID := uuid.New()
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+realmPath("acme", "users", userID.String(), "credentials"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `[]`)
		})
		mux.HandleFunc("GET "+realmPath("acme", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":"`+userID.String()+`"}`)
		})
		mux.HandleFunc("PUT "+realmPath("acme", "users", userID.String()), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.ResetMFA(context.Background(), "acme", userID, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "update_user")
}

// --- CreateOrUpdateIdp ---

func idpPath(realm string, parts ...string) string {
	return realmPath(realm, append([]string{"identity-provider", "instances"}, parts...)...)
}

func TestCreateOrUpdateIdp_CreatesWhenAbsent(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, `{}`)
		})
		mux.HandleFunc("POST "+idpPath("acme"), func(w http.ResponseWriter, _ *http.Request) {
			writeLocation(w, "idp1")
			writeJSON(w, http.StatusCreated, `{}`)
		})
	})
	err := a.CreateOrUpdateIdp(context.Background(), "acme", port.IdpKeycloakConfig{
		Alias: "idp1", Protocol: "oidc", DisplayName: "IdP One", Enabled: true,
		Config: map[string]any{"clientId": "cid", "notAString": 5},
		Secret: "shh",
	})
	require.NoError(t, err)
}

func TestCreateOrUpdateIdp_GetError(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.CreateOrUpdateIdp(context.Background(), "acme", port.IdpKeycloakConfig{Alias: "idp1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "configure_idp: get")
}

func TestCreateOrUpdateIdp_CreateError(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, `{}`)
		})
		mux.HandleFunc("POST "+idpPath("acme"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.CreateOrUpdateIdp(context.Background(), "acme", port.IdpKeycloakConfig{Alias: "idp1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "configure_idp: create")
}

func TestCreateOrUpdateIdp_UpdatesWhenPresent(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"alias":"idp1"}`)
		})
		mux.HandleFunc("PUT "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{}`)
		})
	})
	err := a.CreateOrUpdateIdp(context.Background(), "acme", port.IdpKeycloakConfig{Alias: "idp1", Protocol: "saml"})
	require.NoError(t, err)
}

func TestCreateOrUpdateIdp_UpdateError(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"alias":"idp1"}`)
		})
		mux.HandleFunc("PUT "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.CreateOrUpdateIdp(context.Background(), "acme", port.IdpKeycloakConfig{Alias: "idp1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "configure_idp: update")
}

// --- DeleteIdp ---

func TestDeleteIdp_Success(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("DELETE "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	})
	require.NoError(t, a.DeleteIdp(context.Background(), "acme", "idp1"))
}

func TestDeleteIdp_NotFoundIsIdempotent(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("DELETE "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, `{}`)
		})
	})
	require.NoError(t, a.DeleteIdp(context.Background(), "acme", "idp1"))
}

func TestDeleteIdp_GenericErrorPropagates(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("DELETE "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.DeleteIdp(context.Background(), "acme", "idp1")
	require.Error(t, err)
}

// --- ApplyRealmConfig ---

func TestApplyRealmConfig_LocalAccountsEnabled_Success(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("PUT "+realmPath("acme"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{}`)
		})
	})
	require.NoError(t, a.ApplyRealmConfig(context.Background(), "acme", "local_accounts_enabled", true))
}

func TestApplyRealmConfig_LocalAccountsEnabled_UpdateError(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("PUT "+realmPath("acme"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.ApplyRealmConfig(context.Background(), "acme", "local_accounts_enabled", true)
	require.Error(t, err)
}

func TestApplyRealmConfig_LocalAccountsEnabled_NonBoolDefaultsFalse(t *testing.T) {
	var gotBody string
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("PUT "+realmPath("acme"), func(w http.ResponseWriter, r *http.Request) {
			buf, _ := io.ReadAll(r.Body)
			gotBody = string(buf)
			writeJSON(w, http.StatusOK, `{}`)
		})
	})
	require.NoError(t, a.ApplyRealmConfig(context.Background(), "acme", "local_accounts_enabled", "not-a-bool"))
	require.Contains(t, gotBody, `"local_accounts_enabled":"false"`)
}

func TestApplyRealmConfig_UnsupportedSetting(t *testing.T) {
	a := newFakeAdmin(t, nil, nil)
	err := a.ApplyRealmConfig(context.Background(), "acme", "bogus_setting", true)
	require.Error(t, err)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	require.Contains(t, derr.Details["error"].(string), "bogus_setting")
}

func TestApplyRealmConfig_IdpToggle_Success(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"alias":"idp1","enabled":false}`)
		})
		mux.HandleFunc("PUT "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{}`)
		})
	})
	require.NoError(t, a.ApplyRealmConfig(context.Background(), "acme", "idp:idp1", true))
}

func TestApplyRealmConfig_IdpToggle_GetIdpError(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.ApplyRealmConfig(context.Background(), "acme", "idp:idp1", true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get_idp")
}

func TestApplyRealmConfig_IdpToggle_UpdateIdpError(t *testing.T) {
	a := newFakeAdmin(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("GET "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, `{"alias":"idp1"}`)
		})
		mux.HandleFunc("PUT "+idpPath("acme", "idp1"), func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{}`)
		})
	})
	err := a.ApplyRealmConfig(context.Background(), "acme", "idp:idp1", true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "update_idp")
}
