//go:build integration

// newAuditOutbox against a real migrated database: the startup access
// checks platform-audit runs, and the Runner's start / liveness / stop
// lifecycle main() drives.
package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	auditconfig "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

func TestNewAuditOutbox_RealSchema(t *testing.T) {
	ctx := context.Background()
	pgContainer, err := tcpostgres.Run(ctx,
		"postgres:16.16-alpine",
		tcpostgres.WithDatabase("provisioner"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("testpassword"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err, "start postgres container")
	t.Cleanup(func() { _ = pgContainer.Terminate(ctx) })
	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, pgadapter.MigrateSchema(ctx, dsn, nil), "outbox, domain and platform-audit migrations")

	superPool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: dsn})
	require.NoError(t, err)
	t.Cleanup(superPool.Close)
	ensureMetricsRegistered(t)
	t.Setenv(auditconfig.EnvBaseURL, "http://audit-log.invalid:8080")

	// A NOBYPASSRLS role holding every grant still cannot see other
	// tenants' rows under FORCE RLS: NewRunner's catalog check refuses it.
	t.Run("runner pool without cross-tenant access is fatal", func(t *testing.T) {
		raw, err := dbseed.New(ctx, dsn)
		require.NoError(t, err)
		t.Cleanup(raw.Close)
		_, err = raw.Exec(ctx, `CREATE ROLE audit_nobypass LOGIN PASSWORD 'pw' NOBYPASSRLS;
			GRANT SELECT, INSERT, UPDATE, DELETE ON pending_audit_entries TO audit_nobypass`)
		require.NoError(t, err)
		noBypass, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: strings.Replace(dsn, "postgres:testpassword@", "audit_nobypass:pw@", 1)})
		require.NoError(t, err)
		t.Cleanup(noBypass.Close)

		_, err = newAuditOutbox(ctx, superPool, noBypass, &fakeLogger{})
		require.ErrorIs(t, err, auditoutbox.ErrNoCrossTenantAccess)
	})

	t.Run("start, alive, stop", func(t *testing.T) {
		ob, err := newAuditOutbox(ctx, superPool, superPool, &fakeLogger{})
		require.NoError(t, err)
		assert.Equal(t, 8192, ob.maxMetadata)
		assert.Error(t, ob.runner.Alive(), "not alive before Start")

		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		ob.start(runCtx, &fakeLogger{})
		require.Eventually(t, func() bool { return ob.runner.Alive() == nil }, 10*time.Second, 20*time.Millisecond)

		ob.stop(5*time.Second, &fakeLogger{}) //nolint:contextcheck // stop uses its own fresh shutdown budget, exactly as main() calls it
		assert.Error(t, ob.runner.Alive(), "not alive after Stop")
	})
}
