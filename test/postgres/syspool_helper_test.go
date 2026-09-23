//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/stretchr/testify/require"
)

// setupSysPool returns a pgcommon.Pool bound as realm_provisioner_migrator
// (BYPASSRLS) — the pool the reconciler binary uses for cross-tenant sweeps
// (CountsByShard, ListExpiredTrials, RP-C4's ListPending) per
// database-schema.md's role catalog. No GUCProvider is needed since
// BYPASSRLS makes the tenant GUC irrelevant to visibility.
//
// The domain migration only GRANTs table privileges to realm_provisioner_app
// (and SELECT to admin_readonly) — in production realm_provisioner_migrator
// gets its CRUD privileges from Terraform, which this test harness doesn't
// run, so they are granted here directly instead. BYPASSRLS alone bypasses
// row-security policies, not GRANT-based table permissions.
func setupSysPool(t testing.TB, rawPool *dbseed.Pool) *pgcommon.Pool {
	t.Helper()
	ctx := context.Background()

	_, err := rawPool.Exec(ctx, `
		GRANT SELECT, INSERT, UPDATE, DELETE ON
			tenant_realms, keycloak_idp_configs, keycloak_idp_config_validations,
			realm_config_reconciliations, provisioning_operations, processed_events,
			outbox_events
		TO realm_provisioner_migrator`)
	require.NoError(t, err)

	// GaugeRepository.RLSViolationCounts reads this table via sysPool —
	// SELECT-only, this role never writes rls_violation_log. Production's
	// equivalent grant is Terraform-owned (this repo has no rls_violation_log
	// GRANT of its own to realm_provisioner_migrator; see the comment on
	// RLSViolationCounts).
	_, err = rawPool.Exec(ctx, `GRANT SELECT ON rls_violation_log TO realm_provisioner_migrator`)
	require.NoError(t, err)

	// RLSViolationCounts's persisted high-water mark (§4.2) — read via
	// SELECT ... FOR UPDATE and written back after every scrape. Same
	// Terraform-is-the-real-grant-owner caveat as rls_violation_log above.
	_, err = rawPool.Exec(ctx, `GRANT SELECT, INSERT, UPDATE ON exporter_watermarks TO realm_provisioner_migrator`)
	require.NoError(t, err)

	cfg := rawPool.Config().ConnConfig
	dsn := fmt.Sprintf("postgres://realm_provisioner_migrator:%s@%s:%d/%s?sslmode=disable",
		migratorPassword, cfg.Host, cfg.Port, cfg.Database)
	pool, err := pgcommon.NewPool(ctx, pgcommon.Config{
		DSN:           dsn,
		PGBouncerMode: false, // testcontainer talks to Postgres directly
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}
