//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// setupSysPool returns a pgcommon.Pool bound as realm_provisioner_migrator
// (BYPASSRLS) — the pool the reconciler binary uses for cross-tenant sweeps
// (CountsByShard, ListExpiredTrials, RP-C4's ListPending) per
// database-schema.md's role catalogue. No GUCProvider is needed since
// BYPASSRLS makes the tenant GUC irrelevant to visibility.
//
// The domain migration only GRANTs table privileges to realm_provisioner_app
// (and SELECT to admin_readonly) — in production realm_provisioner_migrator
// gets its CRUD privileges from Terraform, which this test harness doesn't
// run, so they are granted here directly instead. BYPASSRLS alone bypasses
// row-security policies, not GRANT-based table permissions.
func setupSysPool(t testing.TB, rawPool *pgxpool.Pool) *pgcommon.Pool {
	t.Helper()
	ctx := context.Background()

	_, err := rawPool.Exec(ctx, `
		GRANT SELECT, INSERT, UPDATE, DELETE ON
			tenant_realms, keycloak_idp_configs, keycloak_idp_config_validations,
			realm_config_reconciliations, provisioning_operations, processed_events
		TO realm_provisioner_migrator`)
	require.NoError(t, err)

	cfg := rawPool.Config().ConnConfig
	dsn := fmt.Sprintf("postgres://realm_provisioner_migrator:%s@%s:%d/%s?sslmode=disable",
		migratorPassword, cfg.Host, cfg.Port, cfg.Database)
	pool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: dsn})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}
