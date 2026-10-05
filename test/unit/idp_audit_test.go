package unit_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pkg/requestctx"
)

// TenantIdpConfigChanged (HLD §9.4, §10.6) — every successful RP-6/RP-7
// change enqueues exactly one AL-5 direct-write entry in the same
// transaction as its keycloak_idp_configs write; a rejected or failed
// change enqueues nothing.

func stubConfigureIdpHappyPath(kc *mockKeycloakAdmin, idpConfigs *mockIdpConfigRepository, openbao *mockOpenBaoClient) {
	openbao.WriteIdpSecretFn = func(context.Context, uuid.UUID, string, map[string]any) error { return nil }
	kc.CreateOrUpdateIdpFn = func(context.Context, string, port.IdpKeycloakConfig) error { return nil }
	idpConfigs.UpsertFn = func(context.Context, *domain.IdpConfig) error { return nil }
}

func TestConfigureIdp_NewConfig_EnqueuesCreateAuditEntry(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _, _, audit := newIdpTestServiceWithAudit()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	stubConfigureIdpHappyPath(kc, idpConfigs, openbao)

	_, err := svc.ConfigureIdp(context.Background(), "idem-audit-1", tenantID, validSAMLIdpRequest())
	require.NoError(t, err)

	require.Len(t, audit.Enqueued, 1)
	e := audit.Enqueued[0]
	assert.Equal(t, tenantID, e.TenantID)
	assert.Equal(t, "config.idp.changed", e.EntryType)
	assert.Equal(t, "TenantIdpConfigChanged", e.SourceEventType)
	assert.Equal(t, domain.Source, e.SourceService)
	assert.Equal(t, domain.AuditActionCreate, e.Action)
	assert.Equal(t, &domain.AuditTarget{Type: "idp", ID: "acme-sso"}, e.Target)
	assert.Equal(t, domain.AuditActor{Type: domain.AuditActorIAMSystem, ID: domain.SystemActorID}, e.Actor)
	assert.False(t, e.OccurredAt.IsZero())
	var meta map[string]any
	require.NoError(t, json.Unmarshal(e.Metadata, &meta))
	assert.Equal(t, string(domain.ValidationOutcomePass), meta["validation_outcome"])
	assert.Equal(t, true, meta["secret_rotated"])
	assert.NotEmpty(t, meta["checks"])
	for _, v := range meta {
		assert.NotEqual(t, "super-secret", v, "the IdP secret must never reach an audit entry")
	}
}

func TestConfigureIdp_ExistingConfig_EnqueuesUpdateAuditEntry(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _, _, audit := newIdpTestServiceWithAudit()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	stubConfigureIdpHappyPath(kc, idpConfigs, openbao)
	idpConfigs.FindByAliasFn = func(_ context.Context, tid uuid.UUID, alias string) (*domain.IdpConfig, error) {
		return &domain.IdpConfig{TenantID: tid, IdpAlias: alias}, nil
	}

	_, err := svc.ConfigureIdp(context.Background(), "idem-audit-2", tenantID, validSAMLIdpRequest())
	require.NoError(t, err)
	require.Len(t, audit.Enqueued, 1)
	assert.Equal(t, domain.AuditActionUpdate, audit.Enqueued[0].Action)
}

func TestConfigureIdp_NonSystemCaller_RecordedAsUserActor(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _, _, audit := newIdpTestServiceWithAudit()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	stubConfigureIdpHappyPath(kc, idpConfigs, openbao)
	userID := uuid.New()
	ctx := requestctx.WithContext(context.Background(), &requestctx.RequestContext{
		UserID: userID, TenantID: tenantID, ClientIP: "203.0.113.7", UserAgent: "om/1.0",
	})

	_, err := svc.ConfigureIdp(ctx, "idem-audit-3", tenantID, validSAMLIdpRequest())
	require.NoError(t, err)
	require.Len(t, audit.Enqueued, 1)
	e := audit.Enqueued[0]
	assert.Equal(t, domain.AuditActor{Type: domain.AuditActorUser, ID: userID.String()}, e.Actor)
	assert.Equal(t, "203.0.113.7", e.IPAddress)
	assert.Equal(t, "om/1.0", e.UserAgent)
}

func TestConfigureIdp_CriticalFailure_EnqueuesNoAuditEntry(t *testing.T) {
	svc, _, realms, _, _, _, _, _, audit := newIdpTestServiceWithAudit()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	req := validSAMLIdpRequest()
	req.Config["sso_url"] = "https://idp.ap-south-1.example.com/sso"

	_, err := svc.ConfigureIdp(context.Background(), "idem-audit-4", tenantID, req)
	require.Error(t, err)
	assert.Empty(t, audit.Enqueued)
}

func TestConfigureIdp_KeycloakFailure_EnqueuesNoAuditEntry(t *testing.T) {
	svc, kc, realms, _, _, openbao, _, _, audit := newIdpTestServiceWithAudit()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	openbao.WriteIdpSecretFn = func(context.Context, uuid.UUID, string, map[string]any) error { return nil }
	openbao.DeleteIdpSecretFn = func(context.Context, uuid.UUID, string) error { return nil }
	kc.CreateOrUpdateIdpFn = func(context.Context, string, port.IdpKeycloakConfig) error { return errors.New("kc down") }

	_, err := svc.ConfigureIdp(context.Background(), "idem-audit-5", tenantID, validSAMLIdpRequest())
	require.Error(t, err)
	assert.Empty(t, audit.Enqueued)
}

// The enqueue shares the Upsert's transaction: its failure fails the
// whole commit (and the idempotency row), so a committed IdP config
// always carries its audit entry.
func TestConfigureIdp_AuditEnqueueFailure_FailsTheCommit(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, idem, _, audit := newIdpTestServiceWithAudit()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	stubConfigureIdpHappyPath(kc, idpConfigs, openbao)
	enqueueErr := errors.New("insert pending_audit_entries failed")
	audit.EnqueueErr = enqueueErr

	_, err := svc.ConfigureIdp(context.Background(), "idem-audit-6", tenantID, validSAMLIdpRequest())
	require.ErrorIs(t, err, enqueueErr)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["idem-audit-6"].Status)
}

func TestConfigureIdp_TxRunnerFailure_Propagates(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _, tx, audit := newIdpTestServiceWithAudit()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	stubConfigureIdpHappyPath(kc, idpConfigs, openbao)
	txErr := domain.NewError(domain.ErrDBUnavailable, "db down")
	tx.FailFn = func() error { return txErr }

	_, err := svc.ConfigureIdp(context.Background(), "idem-audit-7", tenantID, validSAMLIdpRequest())
	require.ErrorIs(t, err, txErr)
	assert.Empty(t, audit.Enqueued)
}

func TestRemoveIdp_EnqueuesDeleteAuditEntry(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _, _, audit := newIdpTestServiceWithAudit()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	idpConfigs.FindByAliasFn = func(_ context.Context, tid uuid.UUID, alias string) (*domain.IdpConfig, error) {
		return &domain.IdpConfig{TenantID: tid, IdpAlias: alias, Protocol: domain.IdpProtocolOIDC}, nil
	}
	kc.DeleteIdpFn = func(context.Context, string, string) error { return nil }
	openbao.DeleteIdpSecretFn = func(context.Context, uuid.UUID, string) error { return nil }
	idpConfigs.DeleteFn = func(context.Context, uuid.UUID, string) error { return nil }

	require.NoError(t, svc.RemoveIdp(context.Background(), tenantID, "acme-sso"))
	require.Len(t, audit.Enqueued, 1)
	e := audit.Enqueued[0]
	assert.Equal(t, domain.AuditActionDelete, e.Action)
	assert.Equal(t, "acme-sso", e.Target.ID)
	var meta map[string]any
	require.NoError(t, json.Unmarshal(e.Metadata, &meta))
	assert.Equal(t, string(domain.IdpProtocolOIDC), meta["protocol"])
}

func TestRemoveIdp_LocalDeleteFailure_EnqueuesNoAuditEntry(t *testing.T) {
	svc, kc, realms, idpConfigs, _, openbao, _, _, audit := newIdpTestServiceWithAudit()
	tenantID := uuid.New()
	seedDedicatedRealm(realms, tenantID, nil)
	idpConfigs.FindByAliasFn = func(_ context.Context, tid uuid.UUID, alias string) (*domain.IdpConfig, error) {
		return &domain.IdpConfig{TenantID: tid, IdpAlias: alias}, nil
	}
	kc.DeleteIdpFn = func(context.Context, string, string) error { return nil }
	openbao.DeleteIdpSecretFn = func(context.Context, uuid.UUID, string) error { return nil }
	delErr := errors.New("db down")
	idpConfigs.DeleteFn = func(context.Context, uuid.UUID, string) error { return delErr }

	require.ErrorIs(t, svc.RemoveIdp(context.Background(), tenantID, "acme-sso"), delErr)
	assert.Empty(t, audit.Enqueued)
}
