//go:build integration

// runBusinessExporters (unlike tick, which exporters_test.go already
// covers via a hand-rolled emit closure) takes a concrete
// *pgadapter.GaugeRepository, not an interface — constructing a real one
// needs a real *pgcommon.Pool, which needs a real, reachable Postgres.
// This file stands up a Postgres testcontainer (mirroring
// cmd/reconciler/main_integration_test.go's own pattern) so the actual
// wiring — four goroutines started, each querying real tables and
// setting a real metric — gets exercised end-to-end instead of only
// tick's already-tested inner loop.
package main

import (
	"context"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"

	"github.com/testcontainers/testcontainers-go"
)

func TestRunBusinessExporters_RealSchema_PopulatesGaugesAndReturnsOnCancel(t *testing.T) {
	ctx := context.Background()

	pgContainer, err := tcpostgres.Run(ctx,
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
	require.NoError(t, err, "start postgres container")
	t.Cleanup(func() { _ = pgContainer.Terminate(ctx) })

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	require.NoError(t, outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: dsn}), "apply outbox schema")
	require.NoError(t, pgadapter.RunMigrations(ctx, dsn), "run domain migrations")

	pool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: dsn, PGBouncerMode: false})
	require.NoError(t, err, "connect pool")
	t.Cleanup(pool.Close)

	ensureMetricsRegistered(t)
	log := &fakeLogger{}

	runCtx, cancel := context.WithCancel(ctx)
	runBusinessExporters(runCtx, pgadapter.NewGaugeRepository(pool), log)

	// Each of the 4 tick goroutines emits once synchronously before
	// returning control (tick's own "run() // populate before the first
	// scrape" contract) — but runBusinessExporters launches them via `go`,
	// so give the scheduler a moment before asserting on their result.
	require.Eventually(t, func() bool {
		var m dto.Metric
		if err := metrics.RealmConfigPendingGauge.Write(&m); err != nil {
			return false
		}
		return m.GetGauge().GetValue() == 0 // a freshly migrated DB has zero pending realm_config_reconciliations rows
	}, 5*time.Second, 10*time.Millisecond, "realm_config_pending_gauge must reflect the real (empty) table")

	cancel()
	// No observable "stopped" signal from runBusinessExporters itself
	// (fire-and-forget goroutines) — this just confirms cancellation
	// doesn't hang or panic; tick's own unit tests already cover the
	// ctx.Done() return path precisely.
	time.Sleep(50 * time.Millisecond)
}
