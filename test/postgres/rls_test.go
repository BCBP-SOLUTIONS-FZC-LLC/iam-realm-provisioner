//go:build integration

// Package postgres_test is the canonical Phase 1 test suite: spins up a
// fresh Postgres container via testcontainers-go, applies the outbox
// schema + every domain migration, and asserts the RLS invariants named in
// LLD §14.5 (the 5 canonical test cases).
//
// Requires Docker on the runner. Tag: integration.
package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	appRolePassword  = "apppassword-testonly"
	migratorPassword = "migratorpassword-testonly"
)

// setupTestDB spins up a Postgres container, applies outbox.ApplySchema
// then every domain migration (as superuser so DDL/CREATE ROLE succeed),
// creates the two runtime roles the LLD calls out (realm_provisioner_app
// without BYPASSRLS, realm_provisioner_migrator with BYPASSRLS), and
// returns:
//
//	appPool — pgcommon.Pool bound as realm_provisioner_app, RLS enforced,
//	          GUC-provider wired so `SET LOCAL app.tenant_id` fires on
//	          every checkout (mirrors production, RLS-6).
//	rawPool — dbseed.Pool (pgcommon-backed superuser) used only to seed
//	          rows (bypasses RLS naturally).
func setupTestDB(t testing.TB) (appPool *pgcommon.Pool, rawPool *dbseed.Pool) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping postgres integration test in short mode")
	}
	ctx := context.Background()

	const maxContainerAttempts = 3
	var pgContainer *tcpostgres.PostgresContainer
	var superDSN string
	var err error
	for attempt := 1; attempt <= maxContainerAttempts; attempt++ {
		pgContainer, err = tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("provisioner"),
			tcpostgres.WithUsername("postgres"),
			tcpostgres.WithPassword("testpassword"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
			),
		)
		if err == nil {
			superDSN, err = pgContainer.ConnectionString(ctx, "sslmode=disable")
			if err == nil {
				break
			}
			_ = pgContainer.Terminate(ctx)
		}
		t.Logf("setupTestDB: postgres testcontainer attempt %d/%d failed: %v", attempt, maxContainerAttempts, err)
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	require.NoError(t, err, "postgres testcontainer failed after %d attempts", maxContainerAttempts)
	t.Cleanup(func() { _ = pgContainer.Terminate(ctx) })

	rawPool, err = dbseed.New(ctx, superDSN)
	require.NoError(t, err)
	t.Cleanup(rawPool.Close)

	_, err = rawPool.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE realm_provisioner_app LOGIN PASSWORD '%s' NOBYPASSRLS`, appRolePassword))
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE realm_provisioner_migrator LOGIN PASSWORD '%s' BYPASSRLS`, migratorPassword))
	require.NoError(t, err)

	// outbox.ApplySchema FIRST (creates outbox_events with a JSONB payload
	// column) — the domain migration ALTERs that column to TEXT (§19).
	require.NoError(t, outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: superDSN}))
	require.NoError(t, pgadapter.RunMigrations(ctx, superDSN))

	appDSN := strings.Replace(superDSN, "postgres:testpassword@", "realm_provisioner_app:"+appRolePassword+"@", 1)
	appPool, err = pgcommon.NewPool(ctx, pgcommon.Config{
		DSN:           appDSN,
		PGBouncerMode: false, // testcontainer talks to Postgres directly
		GUCProvider:   pgcommon.GUCSetFromContext,
	})
	require.NoError(t, err)
	t.Cleanup(appPool.Close)

	return appPool, rawPool
}

// withTenant returns a context carrying a pgcommon GUCSet so the pool's
// GUCProvider emits `SET LOCAL app.tenant_id = <uuid>` on every checkout.
func withTenant(ctx context.Context, tenantID uuid.UUID) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID.String()
	return pgcommon.WithGUCSet(ctx, g)
}

// seedTenantRealm inserts a tenant_realms row via the superuser pool
// (bypasses RLS).
func seedTenantRealm(t testing.TB, ctx context.Context, rawPool *dbseed.Pool, tenantID uuid.UUID) {
	t.Helper()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO tenant_realms (tenant_id, realm_name, strategy, status)
		VALUES ($1, 'trial', 'shared', 'active')`, tenantID)
	require.NoError(t, err)
}

// ─────────────────────────────────────────────────────────────────────────
// Every tenant-scoped table has ENABLE + FORCE RLS; the app role never
// holds BYPASSRLS (§4.5, §10.1).
// ─────────────────────────────────────────────────────────────────────────
func TestRLS_EveryTenantScopedTableEnabled(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()

	expected := []string{
		"tenant_realms", "keycloak_idp_configs", "keycloak_idp_config_validations",
		"realm_config_reconciliations", "provisioning_operations",
	}
	rows, err := rawPool.Query(ctx, `
		SELECT c.relname
		FROM pg_class c JOIN pg_namespace n ON c.relnamespace = n.oid
		WHERE n.nspname = 'public' AND c.relkind = 'r'
		  AND c.relrowsecurity = true AND c.relforcerowsecurity = true`)
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		got[name] = true
	}
	for _, e := range expected {
		assert.True(t, got[e], "expected %s to have ENABLE + FORCE ROW LEVEL SECURITY", e)
	}
	assert.Len(t, got, len(expected), "unexpected extra/missing RLS tables: %v", got)

	var bypass bool
	err = rawPool.QueryRow(ctx, `SELECT rolbypassrls FROM pg_roles WHERE rolname = 'realm_provisioner_app'`).Scan(&bypass)
	require.NoError(t, err)
	assert.False(t, bypass, "realm_provisioner_app must NOT hold BYPASSRLS")
}

// outbox_events / processed_events / exporter_watermarks are RLS-exempt (§4.5).
func TestRLS_OperationalTablesExempt(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()

	for _, tbl := range []string{"outbox_events", "processed_events", "exporter_watermarks"} {
		var rls, force bool
		err := rawPool.QueryRow(ctx, `
			SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, tbl).Scan(&rls, &force)
		require.NoError(t, err)
		assert.False(t, rls, "%s must be RLS-exempt", tbl)
		assert.False(t, force, "%s must be RLS-exempt", tbl)
	}
}

// Case 1: missing GUC -> 0 rows (fail-closed).
func TestRLS_Case1_MissingGUCReturnsZeroRows(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	seedTenantRealm(t, ctx, rawPool, uuid.New())

	var count int
	err := pgcommon.RunInTx(ctx, appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenant_realms`).Scan(&count)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, count, "missing GUC must return 0 rows")
}

// Case 2: cross-tenant write rejected by WITH CHECK.
func TestRLS_Case2_CrossTenantWriteRejected(t *testing.T) {
	t.Parallel()
	appPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()

	ctxA := withTenant(ctx, tenantA)
	err := pgcommon.RunInTx(ctxA, appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO keycloak_idp_configs (tenant_id, idp_alias, protocol, display_name, config)
			VALUES ($1, 'acme', 'saml', 'Acme', '{}')`, tenantB)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "row-level security", "WITH CHECK must reject a cross-tenant insert")
}

// Case 3: cross-tenant read isolation.
func TestRLS_Case3_CrossTenantReadIsolation(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()
	seedTenantRealm(t, ctx, rawPool, tenantA)
	seedTenantRealm(t, ctx, rawPool, tenantB)

	ctxA := withTenant(ctx, tenantA)
	var crossCount, ownCount int
	err := pgcommon.RunInTx(ctxA, appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenant_realms WHERE tenant_id = $1`, tenantB).Scan(&crossCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenant_realms WHERE tenant_id = $1`, tenantA).Scan(&ownCount)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, crossCount, "must not see the other tenant's row")
	assert.Equal(t, 1, ownCount, "must see its own row")
}

// Case 4: cross-tenant update rejected (0 rows affected, not an error —
// the USING clause simply filters the target row out of the UPDATE's scan).
func TestRLS_Case4_CrossTenantUpdateAffectsZeroRows(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()
	seedTenantRealm(t, ctx, rawPool, tenantA)
	seedTenantRealm(t, ctx, rawPool, tenantB)

	ctxA := withTenant(ctx, tenantA)
	var rowsAffected int64
	err := pgcommon.RunInTx(ctxA, appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tenant_realms SET status = 'disabled' WHERE tenant_id = $1`, tenantB)
		if err != nil {
			return err
		}
		rowsAffected = tag.RowsAffected()
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, int64(0), rowsAffected, "cross-tenant UPDATE must affect 0 rows")
}

// Case 5 (RLS-6): no cross-tenant leakage across a pooled connection —
// pin one backend (MaxConns=1); tenant A writes under SET LOCAL; tenant B
// reads on the same backend afterward and must see only B's rows; a
// subsequent no-GUC checkout on that same backend must also fail closed.
func TestRLS_Case5_NoGUCLeakageAcrossPooledConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A fresh single-connection pool makes backend reuse deterministic.
	_, rawPool := setupTestDB(t)
	tenantA := uuid.New()
	tenantB := uuid.New()
	seedTenantRealm(t, ctx, rawPool, tenantA)
	seedTenantRealm(t, ctx, rawPool, tenantB)

	// PGBouncerMode: true exercises the transaction-scoped GUC path
	// (is_local=true, injectGUCsInTx) that production actually runs under
	// — the LLD mandates PgBouncer transaction pooling for `provisioner`.
	// A SET LOCAL value auto-resets at each transaction's commit/rollback,
	// which is what makes the final no-GUC checkout below fail closed;
	// the non-PgBouncer path uses a SESSION-scoped set_config instead
	// (pgcommon's InjectGUCs, is_local=false) that deliberately does NOT
	// clear a prior tenant's value on a no-GUC checkout — by design, that
	// mode is documented for direct-to-Postgres connections only, and
	// production never runs this database that way.
	appDSN := connStringForApp(t, rawPool)
	pinnedPool, err := pgcommon.NewPool(ctx, pgcommon.Config{
		DSN:           appDSN,
		MaxConns:      1,
		MinConns:      1,
		PGBouncerMode: true,
		GUCProvider:   pgcommon.GUCSetFromContext,
	})
	require.NoError(t, err)
	defer pinnedPool.Close()

	// Tenant A writes on the single backend.
	ctxA := withTenant(ctx, tenantA)
	err = pgcommon.RunInTx(ctxA, pinnedPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenant_realms SET realm_template_version = 'v-a' WHERE tenant_id = $1`, tenantA)
		return err
	})
	require.NoError(t, err)

	// Tenant B reuses the same (only) backend — SET LOCAL's scope ended at
	// A's COMMIT, so B must see only its own row.
	ctxB := withTenant(ctx, tenantB)
	var seenCount int
	err = pgcommon.RunInTx(ctxB, pinnedPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenant_realms WHERE tenant_id = $1`, tenantA).Scan(&seenCount)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, seenCount, "tenant B must not see tenant A's row on a reused backend")

	// A subsequent no-GUC checkout on that same backend must fail closed.
	var noGUCCount int
	err = pgcommon.RunInTx(ctx, pinnedPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenant_realms`).Scan(&noGUCCount)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, noGUCCount, "no-GUC checkout on a reused backend must still fail closed")
}

func connStringForApp(t testing.TB, rawPool *dbseed.Pool) string {
	t.Helper()
	cfg := rawPool.Config().ConnConfig
	return fmt.Sprintf("postgres://realm_provisioner_app:%s@%s:%d/%s?sslmode=disable",
		appRolePassword, cfg.Host, cfg.Port, cfg.Database)
}
