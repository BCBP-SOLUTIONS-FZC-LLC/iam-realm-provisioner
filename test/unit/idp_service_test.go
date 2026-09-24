package unit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

func newIdpTestService() (*service.IdpService, *mockKeycloakAdmin, *mockTenantRealmRepository, *mockIdpConfigRepository, *mockIdpValidationRepository, *mockOpenBaoClient, *mockIdempotencyStore) {
	svc, kc, realms, idpConfigs, validations, openbao, idem, _, _ := newIdpTestServiceWithAudit()
	return svc, kc, realms, idpConfigs, validations, openbao, idem
}

// newIdpTestServiceWithAudit additionally returns the tx runner and the
// pending_audit_entries mock, for the TenantIdpConfigChanged (§10.6) tests.
func newIdpTestServiceWithAudit() (*service.IdpService, *mockKeycloakAdmin, *mockTenantRealmRepository, *mockIdpConfigRepository, *mockIdpValidationRepository, *mockOpenBaoClient, *mockIdempotencyStore, *mockTxRunner, *mockPendingAuditEntryRepository) {
	kc := &mockKeycloakAdmin{}
	realms := newMockTenantRealmRepository()
	idpConfigs := &mockIdpConfigRepository{
		// Defaults every test to the "new config" case (compensating
		// OpenBao delete allowed on a later Keycloak failure) — tests
		// exercising the update case override this explicitly.
		FindByAliasFn: func(ctx context.Context, tenantID uuid.UUID, alias string) (*domain.IdpConfig, error) {
			return nil, domain.NewError(domain.ErrIdpConfigNotFound, "no such idp config")
		},
	}
	validations := &mockIdpValidationRepository{InsertFn: func(ctx context.Context, v *domain.IdpConfigValidation) error { return nil }}
	idem := newMockIdempotencyStore()
	openbao := &mockOpenBaoClient{}
	tx, _ := newMockTxRunner()
	audit := &mockPendingAuditEntryRepository{}
	svc := service.NewIdpService(realms, idpConfigs, validations, idem, openbao, kc, tx, audit)
	return svc, kc, realms, idpConfigs, validations, openbao, idem, tx, audit
}

func validSAMLIdpRequest() service.ConfigureIdpRequest {
	return service.ConfigureIdpRequest{
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
	svc, _, realms, _, _, _, _ := newIdpTestService()
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)

	_, err := svc.ConfigureIdp(context.Background(), "idem-1", tenantID, validSAMLIdpRequest())
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrIdpRequiresDedicatedRealm, de.Code)
	assert.Equal(t, 409, de.Status())
}

func TestConfigureIdp_TenantNotFound(t *testing.T) {
	svc, _, _, _, _, _, _ := newIdpTestService()
	_, err := svc.ConfigureIdp(context.Background(), "idem-2", uuid.New(), validSAMLIdpRequest())
	require.Error(t, err)
}

// RP-6: a terminal (offboarded) tenant must reject IdP configuration
// before any §11.6 validation or Keycloak/OpenBao call.
func TestConfigureIdp_Offboarded_Returns409(t *testing.T) {
	svc, _, realms, _, _, _, idem := newIdpTestService()
	tenantID := uuid.New()
	shard := "kc-shard-1"
	realms.rows[tenantID] = &domain.TenantRealm{
		TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated,
		KeycloakShard: &shard, Status: domain.RealmStatusDeleted,
	}

	_, err := svc.ConfigureIdp(context.Background(), "idem-offboarded", tenantID, validSAMLIdpRequest())
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrTenantOffboarded, de.Code)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["idem-offboarded"].Status)
}

// failIdempotency's own persistence failure is best-effort — the caller
// still sees the ORIGINAL error, not a ledger error.
func TestConfigureIdp_FailIdempotencyPersistFailure_LoggedNotEscalated(t *testing.T) {
	svc, _, realms, _, _, _, idem := newIdpTestService()
	logger := &mockLogger{}
	svc.WithLogger(logger)
	idem.FailErr = errors.New("ledger unavailable")
	tenantID := uuid.New()
	seedSharedRealm(realms, tenantID)

	_, err := svc.ConfigureIdp(context.Background(), "idem-fail-persist", tenantID, validSAMLIdpRequest())
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrIdpRequiresDedicatedRealm, de.Code, "the ORIGINAL error, not the ledger failure")
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// A hard ledger failure from idempotency.Begin propagates immediately — no
// tenant lookup, no Keycloak/OpenBao calls.
func TestConfigureIdp_BeginError_Propagates(t *testing.T) {
	svc, _, realms, _, _, _, idem := newIdpTestService()
	idem.BeginErr = errors.New("ledger unavailable")
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)

	_, err := svc.ConfigureIdp(context.Background(), "idem-begin-fail", tenantID, validSAMLIdpRequest())
	require.Error(t, err)
	assert.Same(t, idem.BeginErr, err)
}

// A FindByAlias failure other than the distinguishable ErrIdpConfigNotFound
// (a transient repo error, say) is treated as "might be an update" for
// compensating-delete safety — logged, not propagated, and the flow
// continues to a normal success.
func TestConfigureIdp_ExistingConfigLookupFails_TreatedAsUpdate_LoggedNotPropagated(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _ := newIdpTestService()
	logger := &mockLogger{}
	svc.WithLogger(logger)
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	idpConfigs.FindByAliasFn = func(ctx context.Context, tid uuid.UUID, alias string) (*domain.IdpConfig, error) {
		return nil, errors.New("db down")
	}
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error { return nil }
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error { return nil }
	openbao.DeleteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string) error {
		t.Fatal("an unresolved lookup must be treated as 'might be an update' — no compensating delete on later success")
		return nil
	}
	idpConfigs.UpsertFn = func(ctx context.Context, cfg *domain.IdpConfig) error { return nil }

	_, err := svc.ConfigureIdp(context.Background(), "idem-lookup-fail", tenantID, validSAMLIdpRequest())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// idempotency.Complete's own persistence failure is best-effort — logged,
// not propagated; the caller still sees a successful result.
func TestConfigureIdp_IdempotencyCompleteFailure_LoggedNotPropagated(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, idem := newIdpTestService()
	logger := &mockLogger{}
	svc.WithLogger(logger)
	idem.CompleteErr = errors.New("ledger unavailable")
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error { return nil }
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error { return nil }
	idpConfigs.UpsertFn = func(ctx context.Context, cfg *domain.IdpConfig) error { return nil }

	result, err := svc.ConfigureIdp(context.Background(), "idem-complete-fail", tenantID, validSAMLIdpRequest())
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// §11.6 CRITICAL failure blocks with 400, regardless of which check failed.
func TestConfigureIdp_CriticalCheckFails_Returns400(t *testing.T) {
	svc, _, realms, _, validations, _, _ := newIdpTestService()
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

	_, err := svc.ConfigureIdp(context.Background(), "idem-3", tenantID, req)
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
	svc, _, realms, _, _, _, _ := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	req := validSAMLIdpRequest()
	req.Config["nameid_format"] = "transient" // WARN check_7, unacknowledged

	_, err := svc.ConfigureIdp(context.Background(), "idem-4", tenantID, req)
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrIdpWarningsUnacknowledged, de.Code)
	assert.Equal(t, 422, de.Status())
}

// §11.6 WARN failure WITH the matching acknowledgement passes through, and
// the OpenBao write happens strictly before the Keycloak write.
func TestConfigureIdp_WarnAcknowledged_Success_OpenBaoBeforeKeycloak(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _ := newIdpTestService()
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

	result, err := svc.ConfigureIdp(context.Background(), "idem-5", tenantID, req)
	require.NoError(t, err)
	assert.Equal(t, "acme-sso", result.IdpAlias)
	assert.Equal(t, "active", result.Status)
	assert.Equal(t, []string{"openbao", "keycloak", "postgres"}, order)
}

// A pass (no warnings at all) skips OpenBao entirely when no secret is
// supplied — the write is conditional on Secret != "".
func TestConfigureIdp_NoSecret_SkipsOpenBaoWrite(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _ := newIdpTestService()
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

	_, err := svc.ConfigureIdp(context.Background(), "idem-6", tenantID, req)
	require.NoError(t, err)
}

// If the OpenBao write fails, nothing is written to Keycloak or Postgres.
func TestConfigureIdp_OpenBaoFailure_NothingWrittenDownstream(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, idem := newIdpTestService()
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

	_, err := svc.ConfigureIdp(context.Background(), "idem-7", tenantID, validSAMLIdpRequest())
	require.Error(t, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["idem-7"].Status)
}

func TestConfigureIdp_KeycloakFailure_DoesNotWriteConfig(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _ := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error { return nil }
	openbao.DeleteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string) error { return nil }
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	idpConfigs.UpsertFn = func(ctx context.Context, cfg *domain.IdpConfig) error {
		t.Fatal("must not persist config when the Keycloak write failed")
		return nil
	}

	_, err := svc.ConfigureIdp(context.Background(), "idem-8", tenantID, validSAMLIdpRequest())
	require.Error(t, err)
}

// CRITICAL: when this alias has no prior config, a Keycloak failure after
// the OpenBao secret was already written must trigger a compensating
// delete of that secret — it was never applied to any live IdP, so leaving
// it behind would orphan it in OpenBao forever.
func TestConfigureIdp_KeycloakFailure_NewConfig_CompensatingOpenBaoDelete(t *testing.T) {
	svc, kc, realms, _, _, openbao, idem := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	// newIdpTestService's default FindByAliasFn already reports
	// ErrIdpConfigNotFound — this is the "new config" case.
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error { return nil }
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	var deletedTenant uuid.UUID
	var deletedAlias string
	openbao.DeleteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string) error {
		deletedTenant, deletedAlias = tid, alias
		return nil
	}

	_, err := svc.ConfigureIdp(context.Background(), "idem-9", tenantID, validSAMLIdpRequest())
	require.Error(t, err)
	assert.Equal(t, tenantID, deletedTenant, "the orphaned secret must be compensating-deleted")
	assert.Equal(t, "acme-sso", deletedAlias)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["idem-9"].Status)
}

// The compensating delete itself failing is best-effort — logged, not
// propagated; the caller still sees the original Keycloak failure, not a
// confusing OpenBao one.
func TestConfigureIdp_KeycloakFailure_NewConfig_CompensatingOpenBaoDeleteFails_LoggedNotEscalated(t *testing.T) {
	svc, kc, realms, _, _, openbao, idem := newIdpTestService()
	logger := &mockLogger{}
	svc.WithLogger(logger)
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error { return nil }
	keycloakErr := domain.NewError(domain.ErrKeycloakUnavailable, "down")
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
		return keycloakErr
	}
	openbao.DeleteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string) error {
		return errors.New("openbao unreachable")
	}

	_, err := svc.ConfigureIdp(context.Background(), "idem-9b", tenantID, validSAMLIdpRequest())
	require.Same(t, keycloakErr, err, "the original Keycloak error must surface, not the compensating-delete failure")
	assert.Equal(t, domain.OpStatusFailed, idem.rows["idem-9b"].Status)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// CRITICAL: when this alias already had a prior config, a Keycloak failure
// must NOT delete the OpenBao secret — the pre-existing Keycloak IdP is
// still live, unaffected by this failed update, and deleting the secret
// (even though WriteIdpSecret already overwrote OpenBao's copy with the
// new, never-applied value) would make it impossible to ever recover the
// old value for a still-working integration.
func TestConfigureIdp_KeycloakFailure_ExistingConfig_NoCompensatingDelete(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _ := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	idpConfigs.FindByAliasFn = func(ctx context.Context, tid uuid.UUID, alias string) (*domain.IdpConfig, error) {
		return &domain.IdpConfig{TenantID: tid, IdpAlias: alias}, nil
	}
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error { return nil }
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	openbao.DeleteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string) error {
		t.Fatal("must not delete the OpenBao secret when this alias already had a prior config")
		return nil
	}

	_, err := svc.ConfigureIdp(context.Background(), "idem-10", tenantID, validSAMLIdpRequest())
	require.Error(t, err)
}

// The local idpConfigs.Upsert failing (after OpenBao and Keycloak both
// already succeeded) propagates the repository error verbatim.
func TestConfigureIdp_IdpConfigUpsertFailure_Propagates(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _ := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error { return nil }
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error { return nil }
	upsertErr := errors.New("db down")
	idpConfigs.UpsertFn = func(ctx context.Context, cfg *domain.IdpConfig) error { return upsertErr }

	_, err := svc.ConfigureIdp(context.Background(), "idem-11", tenantID, validSAMLIdpRequest())
	require.Error(t, err)
	assert.Same(t, upsertErr, err)
}

// Recording the validation run is best-effort — a failure there is logged,
// not propagated, and the configure flow continues normally.
func TestConfigureIdp_ValidationRecordFailure_LoggedNotPropagated(t *testing.T) {
	svc, kc, realms, idpConfigs, validations, openbao, _ := newIdpTestService()
	logger := &mockLogger{}
	svc.WithLogger(logger)
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	validations.InsertFn = func(ctx context.Context, v *domain.IdpConfigValidation) error { return errors.New("db down") }
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error { return nil }
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error { return nil }
	idpConfigs.UpsertFn = func(ctx context.Context, cfg *domain.IdpConfig) error { return nil }

	_, err := svc.ConfigureIdp(context.Background(), "idem-12", tenantID, validSAMLIdpRequest())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// CRITICAL (security): idp_alias flows unvalidated all the way into
// openbao.Client.idpPath's plain string interpolation
// ("<prefix>/<tenant_id>/<idp_alias>"), with no sanitization at that
// layer. A path-traversal sequence in the alias must be rejected before
// any OpenBao/Keycloak call is ever made — never merely relied upon to
// fail somewhere downstream.
func TestConfigureIdp_InvalidAliasFormat_RejectedBeforeAnyCall(t *testing.T) {
	for _, alias := range []string{
		"../other-tenant/evil", "..", "a/b", "a b", "alias.with.dots", "", "alias!", "über-idp",
	} {
		t.Run(alias, func(t *testing.T) {
			svc, kc, realms, _, _, openbao, _ := newIdpTestService()
			tenantID := uuid.New()
			seedDedicatedRealm(realms, tenantID, nil)
			kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
				t.Fatal("must not call Keycloak for a malformed idp_alias")
				return nil
			}
			openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, a string, secret map[string]any) error {
				t.Fatal("must not write to OpenBao for a malformed idp_alias")
				return nil
			}
			req := validSAMLIdpRequest()
			req.IdpAlias = alias

			_, err := svc.ConfigureIdp(context.Background(), "idem-alias-"+alias, tenantID, req)
			require.Error(t, err)
			de, ok := err.(*domain.Error)
			require.True(t, ok)
			assert.Equal(t, domain.ErrInvalidRequest, de.Code)
			assert.Equal(t, 400, de.Status())
		})
	}
}

func TestConfigureIdp_ValidAliasFormats_Accepted(t *testing.T) {
	for _, alias := range []string{"acme-sso", "acme_sso", "ACME123", "a"} {
		t.Run(alias, func(t *testing.T) {
			svc, kc, realms, idpConfigs, _, openbao, _ := newIdpTestService()
			tenantID := uuid.New()
			seedDedicatedRealm(realms, tenantID, nil)
			openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, a string, secret map[string]any) error { return nil }
			kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error { return nil }
			idpConfigs.UpsertFn = func(ctx context.Context, cfg *domain.IdpConfig) error { return nil }
			req := validSAMLIdpRequest()
			req.IdpAlias = alias

			_, err := svc.ConfigureIdp(context.Background(), "idem-valid-"+alias, tenantID, req)
			require.NoError(t, err)
		})
	}
}

func TestRemoveIdp_InvalidAliasFormat_RejectedBeforeAnyCall(t *testing.T) {
	svc, kc, realms, _, _, openbao, _ := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	kc.DeleteIdpFn = func(ctx context.Context, realmName, alias string) error {
		t.Fatal("must not call Keycloak for a malformed idp_alias")
		return nil
	}
	openbao.DeleteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string) error {
		t.Fatal("must not call OpenBao for a malformed idp_alias")
		return nil
	}

	err := svc.RemoveIdp(context.Background(), tenantID, "../other-tenant/evil")
	require.Error(t, err)
	de, ok := err.(*domain.Error)
	require.True(t, ok)
	assert.Equal(t, domain.ErrInvalidRequest, de.Code)
}

// A retry with the same Idempotency-Key after a success replays the stored
// result instead of re-running the flow — Keycloak/OpenBao/Postgres are
// each hit exactly once across both calls.
func TestConfigureIdp_IdempotentReplay(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _ := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	var openbaoCalls, keycloakCalls, upsertCalls int
	openbao.WriteIdpSecretFn = func(ctx context.Context, tid uuid.UUID, alias string, secret map[string]any) error {
		openbaoCalls++
		return nil
	}
	kc.CreateOrUpdateIdpFn = func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
		keycloakCalls++
		return nil
	}
	idpConfigs.UpsertFn = func(ctx context.Context, cfg *domain.IdpConfig) error {
		upsertCalls++
		return nil
	}
	req := validSAMLIdpRequest()

	first, err := svc.ConfigureIdp(context.Background(), "idem-13", tenantID, req)
	require.NoError(t, err)
	second, err := svc.ConfigureIdp(context.Background(), "idem-13", tenantID, req)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Equal(t, 1, openbaoCalls)
	assert.Equal(t, 1, keycloakCalls)
	assert.Equal(t, 1, upsertCalls)
}

// ── RP-7: RemoveIdp ───────────────────────────────────────────────────────

func TestRemoveIdp_Success(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _ := newIdpTestService()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	idpConfigs.FindByAliasFn = func(ctx context.Context, tid uuid.UUID, alias string) (*domain.IdpConfig, error) {
		return &domain.IdpConfig{TenantID: tid, IdpAlias: alias}, nil
	}
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
	svc, _, _, _, _, _, _ := newIdpTestService()
	err := svc.RemoveIdp(context.Background(), uuid.New(), "acme-sso")
	require.Error(t, err)
}

func TestRemoveIdp_KeycloakFailure_StopsBeforeOpenBao(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _ := newIdpTestService()
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
	svc, kc, realms, idpConfigs, _, openbao, _ := newIdpTestService()
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
	svc, _, _, idpConfigs, _, _, _ := newIdpTestService()
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
				"audience": "acme-client", "client_id": "acme-client-id",
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
	svc, _, _, idpConfigs, _, _, _ := newIdpTestService()
	idpConfigs.ListAllFn = func(ctx context.Context) ([]*domain.IdpConfig, error) {
		return nil, errors.New("db down")
	}
	_, err := svc.PortabilityAudit(context.Background())
	require.Error(t, err)
}

func TestPortabilityAudit_Empty(t *testing.T) {
	svc, _, _, idpConfigs, _, _, _ := newIdpTestService()
	idpConfigs.ListAllFn = func(ctx context.Context) ([]*domain.IdpConfig, error) { return nil, nil }
	result, err := svc.PortabilityAudit(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, result.TotalTenants)
	assert.Equal(t, 0, result.TotalIdpConfigs)
	assert.Empty(t, result.FailingTenants)
}
