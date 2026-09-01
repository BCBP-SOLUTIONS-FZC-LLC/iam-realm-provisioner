//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/Nerzal/gocloak/v13"
	"github.com/stretchr/testify/require"
)

// rawAdminClient logs in as the same bootstrap service-account client
// setupKeycloak provisions, returning a raw gocloak client + token for
// fixture setup the KeycloakAdmin port deliberately doesn't expose (e.g.
// seeding a user with a pre-existing OTP/WebAuthn credential representation
// to exercise MigrateUser's copy logic) — never used to duplicate what the
// adapter under test (the *keycloakadapter.Admin returned by setupKeycloak)
// already does.
func rawAdminClient(t *testing.T, baseURL string) (*gocloak.GoCloak, string) {
	t.Helper()
	client := gocloak.NewClient(baseURL)
	jwt, err := client.LoginClient(context.Background(), kcClientID, kcClientSecret, "master")
	require.NoError(t, err)
	return client, jwt.AccessToken
}

// otpCredential is a real Keycloak OTP (TOTP) credential representation —
// the exact shape Keycloak's own realm-export/partialImport format uses,
// confirmed empirically against a live server (§8.3, MigrateUser's TOTP
// copy).
func otpCredential() gocloak.CredentialRepresentation {
	return gocloak.CredentialRepresentation{
		Type:           gocloak.StringP("otp"),
		CredentialData: gocloak.StringP(`{"subType":"totp","digits":6,"counter":0,"period":30,"algorithm":"HmacSHA1"}`),
		SecretData:     gocloak.StringP(`{"value":"MFRGGZDFMZTWQ2LK"}`),
	}
}

// webauthnCredential is a real Keycloak WebAuthn credential representation.
// Its field set (aaguid/credentialPublicKey/counter/attestationStatementFormat/
// credentialId) was determined empirically — Keycloak 26 rejects any other
// field (e.g. attestationConveyancePreference) with a 500 "Cannot parse the
// JSON" from WebAuthnCredentialData's strict Jackson deserializer.
func webauthnCredential() gocloak.CredentialRepresentation {
	return gocloak.CredentialRepresentation{
		Type:           gocloak.StringP("webauthn"),
		CredentialData: gocloak.StringP(`{"aaguid":"00000000-0000-0000-0000-000000000000","credentialPublicKey":"pQECAyYgASFYIA","counter":0,"attestationStatementFormat":"none","credentialId":"AQIDBAUGBwgJ"}`),
		SecretData:     gocloak.StringP(`{}`),
	}
}
