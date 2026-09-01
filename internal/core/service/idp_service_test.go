package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

func newIdpTestService() (*IdpService, *mockKeycloakAdmin, *mockTenantRealmRepository, *mockIdpConfigRepository, *mockIdpValidationRepository, *mockOpenBaoClient) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	idpConfigs := &mockIdpConfigRepository{}
	validations := &mockIdpValidationRepository{InsertFn: func(ctx context.Context, v *domain.IdpConfigValidation) error { return nil }}
	openbao := &mockOpenBaoClient{}
	svc := NewIdpService(realms, idpConfigs, validations, openbao, kc)
	return svc, kc, realms, idpConfigs, validations, openbao
}

func validSAMLIdpRequest() ConfigureIdpRequest {
	return ConfigureIdpRequest{
		IdpAlias: "acme-sso", Protocol: domain.IdpProtocolSAML, DisplayName: "Acme SSO",
		Config: map[string]any{
			"sso_url": "https://idp.example.com/sso/saml", "slo_url": "https://idp.example.com/slo/saml",
			"acs_url":   "https://auth.bcbpsolutions.com/realms/acme/broker/idp/endpoint",
			"entity_id": "urn:acme:sp", "signing_cert": "-----BEGIN CERTIFICATE-----...",
			"nameid_format": "persistent", "signature_algorithm": "RSA-SHA256",
		},
		Secret: "super-secret",
	}
}

// RP-6: IdP configuration requires a dedicated realm.
func TestConfigureIdp_SharedStrategy_RequiresDedicatedRealm(t *testing.T) {
	svc, _, realms, _, _, _ := newIdpTestService()
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)

	_, err := svc.ConfigureIdp(context.Background(), tenantID, validSAMLIdpRequest())
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrIdpRequiresDedicatedRealm, de.Code)
	assert.Equal(t, 409, de.Status())
}

func TestConfigureIdp_TenantNotFound(t *testing.T) {
	svc, _, _, _, _, _ := newIdpTestService()
	_, err := svc.ConfigureIdp(context.Background(), uuid.New(), validSAMLIdpRequest())
	require.Error(t, err)
}

// §11.6 CRITICAL failure blocks with 400, regardless of which check failed.
func TestConfigureIdp_CriticalCheckFails_Returns400(t *testing.T) {
	svc, _, realms, _, validations, _ := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	var recordedOutcome domain.ValidationOutcome
	validations.InsertFn = func(ctx context.Context, v *domain.IdpConfigValidation) error {
		recordedOutcome = v.Outcome
		assert.Equal(t, tenantID, v.TenantID)
		return nil
	}
	req := validSAMLIdpRequest()
	req.Config["signing_cert"] = "" // fails check_3_customer_signing_cert (CRITICAL)

	_, err := svc.ConfigureIdp(context.Background(), tenantID, req)
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrIdpRegionPortabilityFailed, de.Code)
	assert.Equal(t, 400, de.Status())
	assert.Equal(t, domain.ValidationOutcomeFailing, recordedOutcome)
	require.NotNil(t, de.Details)
	assert.Contains(t, de.Details["failed_checks"], domain.CheckCustomerSigningCert)
}

// §11.6 WARN failure without acknowledgement returns 422.
func TestConfigureIdp_WarnUnacknowledged_Returns422(t *testing.T) {
	svc, _, realms, _, _, _ := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	req := validSAMLIdpRequest()
	req.Config["nameid_format"] = "transient" // WARN check_7, unacknowledged

	_, err := svc.ConfigureIdp(context.Background(), tenantID, req)
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrIdpWarningsUnacknowledged, de.Code)
	assert.Equal(t, 422, de.Status())
}

// §11.6 WARN failure WITH the matching acknowledgement passes through, and
// the OpenBao write happens strictly before the Keycloak write.
func TestConfigureIdp_WarnAcknowledged_Success_OpenBaoBeforeKeycloak(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	var order []string
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error {
		order = append(order, "openbao")
		assert.Equal(t, tenantID, tid)
		assert.Equal(t, "acme-sso", alias)
		return nil
	}
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
		order = append(order, "keycloak")
		assert.Equal(t, "tenant-abcd1234", realmName)
		return nil
	}
	idpConfigs.UpsertFn = func(ctx context.Context, cfg *domain.IdpConfig) error {
		order = append(order, "postgres")
		return nil
	}
	req := validSAMLIdpRequest()
	req.Config["nameid_format"] = "transient"
	req.Acknowledgements = []string{domain.CheckNameIDFormat}

	result, err := svc.ConfigureIdp(context.Background(), tenantID, req)
	require.NoError(t, err)
	assert.Equal(t, "acme-sso", result.IdpAlias)
	assert.Equal(t, "active", result.Status)
	assert.Equal(t, []string{"openbao", "keycloak", "postgres"}, order)
}

// A pass (no warnings at all) skips OpenBao entirely when no secret is
// supplied — the write is conditional on Secret != "".
func TestConfigureIdp_NoSecret_SkipsOpenBaoWrite(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error {
		t.Fatal("must not write to OpenBao when no secret is supplied")
		return nil
	}
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error { return nil }
	idpConfigs.UpsertFn = func(ctx context.Context, cfg *domain.IdpConfig) error { return nil }
	req := validSAMLIdpRequest()
	req.Secret = ""

	_, err := svc.ConfigureIdp(context.Background(), tenantID, req)
	require.NoError(t, err)
}

// If the OpenBao write fails, nothing is written to Keycloak or Postgres.
func TestConfigureIdp_OpenBaoFailure_NothingWrittenDownstream(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "openbao down")
	}
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
		t.Fatal("must not write to Keycloak when the OpenBao write failed")
		return nil
	}
	idpConfigs.UpsertFn = func(ctx context.Context, cfg *domain.IdpConfig) error {
		t.Fatal("must not write to Postgres when the OpenBao write failed")
		return nil
	}

	_, err := svc.ConfigureIdp(context.Background(), tenantID, validSAMLIdpRequest())
	require.Error(t, err)
}

func TestConfigureIdp_KeycloakFailure_DoesNotWriteConfig(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error { return nil }
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	idpConfigs.UpsertFn = func(ctx context.Context, cfg *domain.IdpConfig) error {
		t.Fatal("must not persist config when the Keycloak write failed")
		return nil
	}

	_, err := svc.ConfigureIdp(context.Background(), tenantID, validSAMLIdpRequest())
	require.Error(t, err)
}

// The local idpConfigs.Upsert failing (after OpenBao and Keycloak both
// already succeeded) propagates the repository error verbatim.
func TestConfigureIdp_IdpConfigUpsertFailure_Propagates(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error { return nil }
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error { return nil }
	upsertErr := errors.New("db down")
	idpConfigs.UpsertFn = func(ctx context.Context, cfg *domain.IdpConfig) error { return upsertErr }

	_, err := svc.ConfigureIdp(context.Background(), tenantID, validSAMLIdpRequest())
	require.Error(t, err)
	assert.Same(t, upsertErr, err)
}

// Recording the validation run is best-effort — a failure there is logged,
// not propagated, and the configure flow continues normally.
func TestConfigureIdp_ValidationRecordFailure_LoggedNotPropagated(t *testing.T) {
	svc, kc, realms, idpConfigs, validations, openbao := newIdpTestService()
	logger := &mockLogger{}
	svc.WithLogger(logger)
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	validations.InsertFn = func(ctx context.Context, v *domain.IdpConfigValidation) error { return errors.New("db down") }
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error { return nil }
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error { return nil }
	idpConfigs.UpsertFn = func(ctx context.Context, cfg *domain.IdpConfig) error { return nil }

	_, err := svc.ConfigureIdp(context.Background(), tenantID, validSAMLIdpRequest())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// ── RP-7: RemoveIdp ───────────────────────────────────────────────────────

func TestRemoveIdp_Success(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	var order []string
	kc.DeleteIdpFn = func(ctx context.Context, realmName, alias string) error {
		order = append(order, "keycloak")
		assert.Equal(t, "tenant-abcd1234", realmName)
		assert.Equal(t, "acme-sso", alias)
		return nil
	}
	openbao.DeleteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string) error {
		order = append(order, "openbao")
		return nil
	}
	idpConfigs.DeleteFn = func(ctx context.Context, tid uuid.UUID, alias string) error {
		order = append(order, "postgres")
		return nil
	}

	require.NoError(t, svc.RemoveIdp(context.Background(), tenantID, "acme-sso"))
	assert.Equal(t, []string{"keycloak", "openbao", "postgres"}, order)
}

func TestRemoveIdp_TenantNotFound(t *testing.T) {
	svc, _, _, _, _, _ := newIdpTestService()
	err := svc.RemoveIdp(context.Background(), uuid.New(), "acme-sso")
	require.Error(t, err)
}

func TestRemoveIdp_KeycloakFailure_StopsBeforeOpenBao(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	kc.DeleteIdpFn = func(ctx context.Context, realmName, alias string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	openbao.DeleteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string) error {
		t.Fatal("must not touch OpenBao when the Keycloak delete failed")
		return nil
	}
	idpConfigs.DeleteFn = func(ctx context.Context, tid uuid.UUID, alias string) error {
		t.Fatal("must not touch Postgres when the Keycloak delete failed")
		return nil
	}

	err := svc.RemoveIdp(context.Background(), tenantID, "acme-sso")
	require.Error(t, err)
}

func TestRemoveIdp_OpenBaoFailure_StopsBeforePostgres(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	kc.DeleteIdpFn = func(ctx context.Context, realmName, alias string) error { return nil }
	openbao.DeleteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	idpConfigs.DeleteFn = func(ctx context.Context, tid uuid.UUID, alias string) error {
		t.Fatal("must not touch Postgres when the OpenBao delete failed")
		return nil
	}

	err := svc.RemoveIdp(context.Background(), tenantID, "acme-sso")
	require.Error(t, err)
}

// ── RP-13: PortabilityAudit ───────────────────────────────────────────────

func TestPortabilityAudit_CountsAndFailingTenants(t *testing.T) {
	svc, _, _, idpConfigs, _, _ := newIdpTestService()
	passingTenant, warnTenant, failTenant := uuid.New(), uuid.New(), uuid.New()
	idpConfigs.ListAllFn = func(ctx context.Context) ([]*domain.IdpConfig, error) {
		return []*domain.IdpConfig{
			{TenantID: passingTenant, Protocol: domain.IdpProtocolSAML, Config: map[string]any{
				"sso_url": "https://idp.example.com/sso/saml", "slo_url": "https://idp.example.com/slo/saml",
				"acs_url":   "https://auth.bcbpsolutions.com/realms/acme/broker/idp/endpoint",
				"entity_id": "urn:acme:sp", "signing_cert": "cert", "nameid_format": "persistent",
				"signature_algorithm": "RSA-SHA256",
			}},
			{TenantID: warnTenant, Protocol: domain.IdpProtocolOIDC, Config: map[string]any{
				"issuer": "https://login.example.com/oauth2", "redirect_uri": "https://auth.bcbpsolutions.com/realms/acme/broker/idp/endpoint",
				"audience": "acme-client",
			}},
			{TenantID: failTenant, Protocol: domain.IdpProtocolOIDC, Config: map[string]any{
				"issuer": "https://login.example.com/us-east-1/oauth2", "redirect_uri": "https://auth.bcbpsolutions.com/realms/acme/broker/idp/endpoint",
				"audience": "acme-client", "pkce_method": "S256",
			}},
			// A second failing config for the same failTenant must not double-count the tenant.
			{TenantID: failTenant, Protocol: domain.IdpProtocolOIDC, Config: map[string]any{
				"issuer": "https://login.example.com/us-east-1/oauth2", "redirect_uri": "https://auth.bcbpsolutions.com/realms/acme/broker/idp/endpoint",
				"audience": "acme-client", "pkce_method": "S256",
			}},
		}, nil
	}

	result, err := svc.PortabilityAudit(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 4, result.TotalIdpConfigs)
	assert.Equal(t, 3, result.TotalTenants)
	assert.Equal(t, 1, result.Passing)
	assert.Equal(t, 1, result.WarningsOnly)
	assert.Equal(t, 2, result.Failing)
	require.Len(t, result.FailingTenants, 1)
	assert.Equal(t, failTenant.String(), result.FailingTenants[0])
}

func TestPortabilityAudit_ListAllFailure(t *testing.T) {
	svc, _, _, idpConfigs, _, _ := newIdpTestService()
	idpConfigs.ListAllFn = func(ctx context.Context) ([]*domain.IdpConfig, error) {
		return nil, errors.New("db down")
	}
	_, err := svc.PortabilityAudit(context.Background())
	require.Error(t, err)
}

func TestPortabilityAudit_Empty(t *testing.T) {
	svc, _, _, idpConfigs, _, _ := newIdpTestService()
	idpConfigs.ListAllFn = func(ctx context.Context) ([]*domain.IdpConfig, error) { return nil, nil }
	result, err := svc.PortabilityAudit(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, result.TotalTenants)
	assert.Equal(t, 0, result.TotalIdpConfigs)
	assert.Empty(t, result.FailingTenants)
}
