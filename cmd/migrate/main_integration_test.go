//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/test/dbseed"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// The migrate Job applies the outbox and domain schema against a fresh
// database, and a second run (every helm upgrade) is a no-op success.
func TestRun_FreshDatabase_MigratesAndIsRerunnable(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("provisioner"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("testpassword"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Terminate(ctx) })
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	t.Setenv("APP_ENV", "test")
	t.Setenv("MIGRATION_DATABASE_URL", dsn)
	t.Setenv("MIGRATE_TIMEOUT", "2m")

	require.Equal(t, 0, run())
	require.Equal(t, 0, run(), "re-running against an up-to-date schema succeeds")

	conn, err := dbseed.New(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close()
	for _, table := range []string{"outbox_events", "tenant_realms", "provisioning_operations", "pending_audit_entries"} {
		var exists bool
		require.NoError(t, conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&exists))
		assert.True(t, exists, table)
	}
}
