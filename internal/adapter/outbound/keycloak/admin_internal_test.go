package keycloak

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

func TestApiErrStatus(t *testing.T) {
	require.Equal(t, 409, apiErrStatus(gocloak.APIError{Code: 409}))
	require.Equal(t, 404, apiErrStatus(&gocloak.APIError{Code: 404}))
	require.Equal(t, 0, apiErrStatus(errors.New("not an api error")))
	require.Equal(t, 0, apiErrStatus(nil))
}

func TestIsConflictIsNotFound(t *testing.T) {
	require.True(t, isConflict(gocloak.APIError{Code: 409}))
	require.False(t, isConflict(gocloak.APIError{Code: 404}))
	require.True(t, isNotFound(gocloak.APIError{Code: 404}))
	require.False(t, isNotFound(gocloak.APIError{Code: 409}))
	require.False(t, isConflict(errors.New("boom")))
	require.False(t, isNotFound(errors.New("boom")))
}

func TestWrapErr(t *testing.T) {
	require.NoError(t, wrapErr("op", nil))
	err := wrapErr("op", errors.New("boom"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "op")
}

func TestBoolStr(t *testing.T) {
	require.Equal(t, "true", boolStr(true))
	require.Equal(t, "false", boolStr(false))
}

func TestIdpAliasFromSettingKey(t *testing.T) {
	alias, ok := idpAliasFromSettingKey("idp:acme-saml")
	require.True(t, ok)
	require.Equal(t, "acme-saml", alias)

	_, ok = idpAliasFromSettingKey("local_accounts_enabled")
	require.False(t, ok)

	_, ok = idpAliasFromSettingKey("idp:")
	require.False(t, ok, "an empty alias after the prefix is not a valid idp: key")
}

func TestErrUnsupportedSetting(t *testing.T) {
	err := errUnsupportedSetting("bogus_key")
	require.Error(t, err)
	require.Contains(t, err.Error(), "bogus_key")
}

func TestSplitName(t *testing.T) {
	first, last := splitName("Dana Owner")
	require.Equal(t, "Dana", first)
	require.Equal(t, "Owner", last)

	first, last = splitName("Cher")
	require.Equal(t, "Cher", first)
	require.Empty(t, last)

	first, last = splitName("Mary Jane Watson")
	require.Equal(t, "Mary Jane", first)
	require.Equal(t, "Watson", last)
}

func TestBuildUser(t *testing.T) {
	tenantID := uuid.New()
	req := port.CreateUserRequest{
		Email: "owner@example.com", FullName: "Dana Owner", TenantID: tenantID,
		RequiredActions: []string{"VERIFY_EMAIL"}, EmailVerified: true,
	}
	u, err := buildUser(req)
	require.NoError(t, err)
	require.Equal(t, "owner@example.com", *u.Username)
	require.Equal(t, "owner@example.com", *u.Email)
	require.Equal(t, "Dana", *u.FirstName)
	require.Equal(t, "Owner", *u.LastName)
	require.True(t, *u.Enabled)
	require.True(t, *u.EmailVerified)
	require.Equal(t, []string{tenantID.String()}, (*u.Attributes)["tenant_id"])
	require.Equal(t, []string{"VERIFY_EMAIL"}, *u.RequiredActions)
	require.Nil(t, u.Credentials, "no password seed given — no credential should be set")
}

func TestBuildUser_NoRequiredActions(t *testing.T) {
	u, err := buildUser(port.CreateUserRequest{Email: "a@example.com", FullName: "A", TenantID: uuid.New()})
	require.NoError(t, err)
	require.Nil(t, u.RequiredActions)
}

func TestBuildUser_WithHashedPasswordSeed(t *testing.T) {
	seed := `{"algorithm":"pbkdf2-sha512","iterations":210000,"salt":"c2FsdA==","hash":"aGFzaA=="}`
	u, err := buildUser(port.CreateUserRequest{
		Email: "a@example.com", FullName: "A", TenantID: uuid.New(), HashedPasswordSeed: seed,
	})
	require.NoError(t, err)
	require.NotNil(t, u.Credentials)
	require.Len(t, *u.Credentials, 1)
	cred := (*u.Credentials)[0]
	require.Equal(t, "password", *cred.Type)
	require.False(t, *cred.Temporary)
	require.Nil(t, cred.Value, "Value must be nil — Keycloak must not re-hash the credential")
	require.NotNil(t, cred.SecretData)
	require.NotNil(t, cred.CredentialData)

	var secret struct {
		Value string `json:"value"`
		Salt  string `json:"salt"`
	}
	require.NoError(t, json.Unmarshal([]byte(*cred.SecretData), &secret))
	require.Equal(t, "aGFzaA==", secret.Value)
	require.Equal(t, "c2FsdA==", secret.Salt)

	var credData struct {
		HashIterations int    `json:"hashIterations"`
		Algorithm      string `json:"algorithm"`
	}
	require.NoError(t, json.Unmarshal([]byte(*cred.CredentialData), &credData))
	require.Equal(t, 210000, credData.HashIterations)
	require.Equal(t, "pbkdf2-sha512", credData.Algorithm)
}

func TestBuildUser_InvalidHashedPasswordSeed(t *testing.T) {
	_, err := buildUser(port.CreateUserRequest{
		Email: "a@example.com", FullName: "A", TenantID: uuid.New(), HashedPasswordSeed: "not-json",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "hashed_password_seed is not valid JSON")
}
