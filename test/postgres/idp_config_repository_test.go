//go:build integration

// Package postgres_test covers IdpConfigRepository and
// IdpValidationRepository (§4.3): CRUD on `keycloak_idp_configs`, its
// (tenant_id, idp_alias) unique constraint and record_version optimistic
// bump, RP-13's cross-tenant ListAll, and the append-only validation
// history.
package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newIdpConfig(tenantID uuid.UUID, alias string) *domain.IdpConfig {
	return &domain.IdpConfig{
		TenantID:    tenantID,
		IdpAlias:    alias,
		Protocol:    domain.IdpProtocolSAML,
		DisplayName: "Acme SAML",
		Enabled:     true,
		Config:      map[string]any{"sso_url": "https://sso.acme.example/saml"},
		Status:      domain.IdpConfigStatusPendingValidation,
	}
}

func TestIdpConfigRepository(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()
	repo := pgadapter.NewIdpConfigRepository(appPool)
	sysRepo := pgadapter.NewIdpConfigRepository(sysPool)

	t.Run("UpsertInsertThenFindByAlias_HappyPath", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		require.NoError(t, repo.Upsert(tctx, newIdpConfig(tenantID, "acme-saml")))

		got, err := repo.FindByAlias(tctx, tenantID, "acme-saml")
		require.NoError(t, err)
		assert.Equal(t, tenantID, got.TenantID)
		assert.Equal(t, domain.IdpProtocolSAML, got.Protocol)
		assert.Equal(t, "Acme SAML", got.DisplayName)
		assert.True(t, got.Enabled)
		assert.Equal(t, "https://sso.acme.example/saml", got.Config["sso_url"])
		assert.Equal(t, int64(1), got.RecordVersion)
		assert.NotEqual(t, uuid.Nil, got.ID)
	})

	t.Run("Upsert_OnConflictUpdatesAndBumpsRecordVersion", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		require.NoError(t, repo.Upsert(tctx, newIdpConfig(tenantID, "acme-saml")))

		update := newIdpConfig(tenantID, "acme-saml")
		update.Enabled = false
		update.Status = domain.IdpConfigStatusDisabled
		require.NoError(t, repo.Upsert(tctx, update))

		got, err := repo.FindByAlias(tctx, tenantID, "acme-saml")
		require.NoError(t, err)
		assert.False(t, got.Enabled)
		assert.Equal(t, domain.IdpConfigStatusDisabled, got.Status)
		assert.Equal(t, int64(2), got.RecordVersion)
	})

	t.Run("FindByAlias_NotFound", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		_, err := repo.FindByAlias(tctx, tenantID, "no-such-alias")
		require.Error(t, err)
		var derr *domain.Error
		require.True(t, errors.As(err, &derr))
		assert.Equal(t, domain.ErrIdpConfigNotFound, derr.Code)
	})

	t.Run("ListByTenant_ScopedAndOrderedByAlias", func(t *testing.T) {
		t.Parallel()
		tenantA, tenantB := uuid.New(), uuid.New()
		ctxA := withTenant(ctx, tenantA)
		require.NoError(t, repo.Upsert(ctxA, newIdpConfig(tenantA, "zzz-alias")))
		require.NoError(t, repo.Upsert(ctxA, newIdpConfig(tenantA, "aaa-alias")))
		require.NoError(t, repo.Upsert(withTenant(ctx, tenantB), newIdpConfig(tenantB, "other-tenant")))

		list, err := repo.ListByTenant(ctxA, tenantA)
		require.NoError(t, err)
		require.Len(t, list, 2)
		assert.Equal(t, "aaa-alias", list[0].IdpAlias)
		assert.Equal(t, "zzz-alias", list[1].IdpAlias)
	})

	t.Run("ListByTenant_RLSSanity_CrossTenantQueryReturnsEmpty", func(t *testing.T) {
		t.Parallel()
		tenantA, tenantB := uuid.New(), uuid.New()
		require.NoError(t, repo.Upsert(withTenant(ctx, tenantA), newIdpConfig(tenantA, "acme-saml")))

		list, err := repo.ListByTenant(withTenant(ctx, tenantB), tenantA)
		require.NoError(t, err)
		assert.Empty(t, list, "tenant B's GUC must not surface tenant A's rows even when explicitly queried by tenant A's ID")
	})

	t.Run("Delete_RemovesRowAndIsIdempotent", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		require.NoError(t, repo.Upsert(tctx, newIdpConfig(tenantID, "acme-saml")))

		require.NoError(t, repo.Delete(tctx, tenantID, "acme-saml"))
		_, err := repo.FindByAlias(tctx, tenantID, "acme-saml")
		require.Error(t, err)
		var derr *domain.Error
		require.True(t, errors.As(err, &derr))
		assert.Equal(t, domain.ErrIdpConfigNotFound, derr.Code)

		assert.NoError(t, repo.Delete(tctx, tenantID, "acme-saml"), "deleting an already-absent row is a no-op, not an error")
	})

	t.Run("UniqueConstraint_TenantAliasPair", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		_, err := rawPool.Exec(ctx, `
			INSERT INTO keycloak_idp_configs (tenant_id, idp_alias, protocol, display_name, config)
			VALUES ($1, 'dup-alias', 'saml', 'First', '{}')`, tenantID)
		require.NoError(t, err)

		_, err = rawPool.Exec(ctx, `
			INSERT INTO keycloak_idp_configs (tenant_id, idp_alias, protocol, display_name, config)
			VALUES ($1, 'dup-alias', 'oidc', 'Second', '{}')`, tenantID)
		require.Error(t, err)
		var pgErr *pgconn.PgError
		require.True(t, errors.As(err, &pgErr))
		assert.Equal(t, "23505", pgErr.Code)
		assert.Equal(t, "uq_idp_tenant_alias", pgErr.ConstraintName, "Upsert's ON CONFLICT(tenant_id, idp_alias) relies on exactly this constraint")
	})

	t.Run("ListAll_CrossTenantForRP13Audit", func(t *testing.T) {
		t.Parallel()
		tenantA, tenantB := uuid.New(), uuid.New()
		require.NoError(t, sysRepo.Upsert(ctx, newIdpConfig(tenantA, "alpha")))
		require.NoError(t, sysRepo.Upsert(ctx, newIdpConfig(tenantA, "beta")))
		require.NoError(t, sysRepo.Upsert(ctx, newIdpConfig(tenantB, "gamma")))

		all, err := sysRepo.ListAll(ctx)
		require.NoError(t, err)
		seen := map[string]uuid.UUID{}
		for _, c := range all {
			seen[c.IdpAlias] = c.TenantID
		}
		assert.Equal(t, tenantA, seen["alpha"])
		assert.Equal(t, tenantA, seen["beta"])
		assert.Equal(t, tenantB, seen["gamma"])
	})

	t.Run("Upsert_ConfigMarshalFailure_ReturnsErrorWithoutTouchingTheDatabase", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		row := newIdpConfig(tenantID, "unmarshalable")
		row.Config = map[string]any{"bad": make(chan int)} // channels are never JSON-marshalable

		err := repo.Upsert(tctx, row)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported type")

		_, findErr := repo.FindByAlias(tctx, tenantID, "unmarshalable")
		require.Error(t, findErr, "the marshal failure must happen before any Exec, so no row is written")
		var derr *domain.Error
		require.True(t, errors.As(findErr, &derr))
		assert.Equal(t, domain.ErrIdpConfigNotFound, derr.Code)
	})

	t.Run("ListByTenant_CancelledContext_ReturnsErrorBeforeAnyQuery", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		cctx, cancel := context.WithCancel(withTenant(ctx, tenantID))
		cancel()

		_, err := repo.ListByTenant(cctx, tenantID)
		require.Error(t, err, "a context cancelled before BeginTx must surface as an error, not a silently empty list")
	})

	t.Run("ListAll_CancelledContext_ReturnsErrorBeforeAnyQuery", func(t *testing.T) {
		t.Parallel()
		cctx, cancel := context.WithCancel(ctx)
		cancel()

		_, err := sysRepo.ListAll(cctx)
		require.Error(t, err)
	})

	// ── CountActiveByTenant — CD-20 SSO-downgrade guard ──────────────────

	// A tenant with no IdPs returns 0.
	t.Run("CountActiveByTenant_NoIdps_ReturnsZero", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)

		n, err := repo.CountActiveByTenant(tctx, tenantID)
		require.NoError(t, err)
		assert.Equal(t, 0, n)
	})

	// One active SAML + one active OIDC → count is 2.
	t.Run("CountActiveByTenant_TwoActiveIdps_ReturnsTwo", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)

		saml := newIdpConfig(tenantID, "saml-corp")
		saml.Status = domain.IdpConfigStatusActive
		require.NoError(t, repo.Upsert(tctx, saml))

		oidc := newIdpConfig(tenantID, "oidc-google")
		oidc.Protocol = domain.IdpProtocolOIDC
		oidc.Status = domain.IdpConfigStatusActive
		require.NoError(t, repo.Upsert(tctx, oidc))

		n, err := repo.CountActiveByTenant(tctx, tenantID)
		require.NoError(t, err)
		assert.Equal(t, 2, n)
	})

	// One active + one disabled → count is 1 (disabled is excluded).
	// This is the key assertion proving the SQL WHERE status='active' filter
	// works correctly for the CD-20 SSO-downgrade guard.
	t.Run("CountActiveByTenant_DisabledIdpNotCounted", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)

		active := newIdpConfig(tenantID, "active-saml")
		active.Status = domain.IdpConfigStatusActive
		require.NoError(t, repo.Upsert(tctx, active))

		disabled := newIdpConfig(tenantID, "disabled-oidc")
		disabled.Protocol = domain.IdpProtocolOIDC
		disabled.Status = domain.IdpConfigStatusDisabled
		require.NoError(t, repo.Upsert(tctx, disabled))

		pending := newIdpConfig(tenantID, "pending-saml2")
		pending.Status = domain.IdpConfigStatusPendingValidation
		require.NoError(t, repo.Upsert(tctx, pending))

		n, err := repo.CountActiveByTenant(tctx, tenantID)
		require.NoError(t, err)
		assert.Equal(t, 1, n,
			"disabled and pending_validation configs must not appear in idp_count (CD-20 SSO guard correctness)")
	})

	// RLS scoping: counting tenant A's active configs while the GUC is set
	// to tenant B must return 0, not tenant A's count.
	t.Run("CountActiveByTenant_RLSSanity_CrossTenantReturnZero", func(t *testing.T) {
		t.Parallel()
		tenantA, tenantB := uuid.New(), uuid.New()
		ctxA := withTenant(ctx, tenantA)
		active := newIdpConfig(tenantA, "saml-active")
		active.Status = domain.IdpConfigStatusActive
		require.NoError(t, repo.Upsert(ctxA, active))

		n, err := repo.CountActiveByTenant(withTenant(ctx, tenantB), tenantA)
		require.NoError(t, err)
		assert.Equal(t, 0, n, "tenant B's GUC must not surface tenant A's IdPs even when tenant A's ID is queried")
	})
}

func TestIdpValidationRepository(t *testing.T) {
	t.Parallel()
	appPool, _ := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewIdpValidationRepository(appPool)

	newValidation := func(tenantID uuid.UUID, alias string, outcome domain.ValidationOutcome) *domain.IdpConfigValidation {
		return &domain.IdpConfigValidation{
			TenantID:         tenantID,
			IdpAlias:         alias,
			ValidatorVersion: 1,
			Outcome:          outcome,
			Checks:           map[string]string{"sso_region": string(outcome)},
			Acknowledgements: []domain.IdpValidationAck{{CheckID: "attr_urn_region_free", UserID: uuid.New(), At: time.Now().UTC()}},
		}
	}

	t.Run("Insert_ThenLatestForAlias_ReturnsMostRecent", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)

		require.NoError(t, repo.Insert(tctx, newValidation(tenantID, "acme-saml", domain.ValidationOutcomeFailing)))
		time.Sleep(5 * time.Millisecond)
		second := newValidation(tenantID, "acme-saml", domain.ValidationOutcomePass)
		require.NoError(t, repo.Insert(tctx, second))

		got, err := repo.LatestForAlias(tctx, tenantID, "acme-saml")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, domain.ValidationOutcomePass, got.Outcome)
		assert.Equal(t, "pass", got.Checks["sso_region"])
		require.Len(t, got.Acknowledgements, 1)
		assert.Equal(t, "attr_urn_region_free", got.Acknowledgements[0].CheckID)
		assert.Equal(t, second.Acknowledgements[0].UserID, got.Acknowledgements[0].UserID)
	})

	t.Run("LatestForAlias_NoneExists_ReturnsNilNoError", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		got, err := repo.LatestForAlias(tctx, tenantID, "never-configured")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("LatestForAlias_RLSSanity_CrossTenantReturnsNil", func(t *testing.T) {
		t.Parallel()
		tenantA, tenantB := uuid.New(), uuid.New()
		require.NoError(t, repo.Insert(withTenant(ctx, tenantA), newValidation(tenantA, "acme-saml", domain.ValidationOutcomePass)))

		got, err := repo.LatestForAlias(withTenant(ctx, tenantB), tenantA, "acme-saml")
		require.NoError(t, err)
		assert.Nil(t, got, "tenant B must not see tenant A's append-only validation history")
	})

	t.Run("LatestForAlias_CancelledContext_ReturnsErrorBeforeAnyQuery", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		cctx, cancel := context.WithCancel(withTenant(ctx, tenantID))
		cancel()

		_, err := repo.LatestForAlias(cctx, tenantID, "acme-saml")
		require.Error(t, err, "a context cancelled before BeginTx must surface as an error, not the none-exists nil,nil shape")
	})
}
