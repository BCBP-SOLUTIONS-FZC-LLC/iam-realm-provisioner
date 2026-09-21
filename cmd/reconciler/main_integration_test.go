//go:build integration

// run() (cmd/reconciler's composition root) was previously untested
// end-to-end — testing its individual wiring steps in isolation would
// mean re-deriving most of main.go's own logic in test doubles. Since
// run() (unlike cmd/server's main()) returns an int and never blocks on a
// signal, it is directly callable from a test: this file stands up a
// real Postgres testcontainer (mirroring test/e2e/setup_test.go's own
// pattern), applies the real schema, points every required env var at it
// (or leaves optional ones — Keycloak, Org & Membership, Token Service —
// unset/fake, since the outbox-prune job this test dispatches never
// calls any of them), and calls run() for real.
package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"

	"github.com/testcontainers/testcontainers-go"
)

// setReconcilerTestEnv points every env var run() reads at either the
// real test Postgres (required) or a safe no-op default (everything
// else) — matching cmd/server/main.go's own "APP_ENV=dev/test relaxes
// required-secret fail-fast" posture (validateRequiredEnv/mustEnv), so
// Keycloak/Org-Membership/Token-Service/AWS never need to be real for a
// job (outbox-prune) that never calls any of them.
func setReconcilerTestEnv(t *testing.T, dsn string) {
	t.Helper()
	t.Setenv("APP_ENV", "test")
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("SYSTEM_DATABASE_URL", dsn)
	t.Setenv("MIGRATION_DATABASE_URL", dsn)
	t.Setenv("PG_BOUNCER_MODE", "false")
	t.Setenv("KEYCLOAK_BASE_URL", "")
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_SECRET", "")
	t.Setenv("ORG_MEMBERSHIP_BASE_URL", "")
	t.Setenv("TOKEN_SERVICE_BASE_URL", "")
	t.Setenv("AWS_REGION", "ap-south-1")
	t.Setenv("RECONCILER_BATCH_LIMIT", "10")
	t.Setenv("RECONCILER_TIMEOUT", "30s")
	t.Setenv("OUTBOX_RETENTION_DAYS", "8")
}

// withArgs temporarily replaces os.Args for the duration of the test —
// run()'s own flag.StringVar(&jobName, "job", os.Getenv("RECONCILER_JOB"), ...)
// reads flag.Parse()'s result from os.Args[1:], and flag.Parse() may only
// be called once against the global flag.CommandLine per process, so this
// file contains exactly one test that calls run().
func withArgs(t *testing.T, args ...string) {
	t.Helper()
	old := os.Args
	os.Args = append([]string{old[0]}, args...)
	t.Cleanup(func() { os.Args = old })
}

func TestRun_OutboxPruneJob_FullWiring_ReturnsZero(t *testing.T) {
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

	// outbox.ApplySchema FIRST — the domain migration ALTERs the JSONB
	// payload column it creates (§19) — matching every other composition
	// root's own migration order (cmd/server/main.go, test/e2e/setup_test.go).
	require.NoError(t, outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: dsn}), "apply outbox schema")
	require.NoError(t, pgadapter.RunMigrations(ctx, dsn), "run domain migrations")

	setReconcilerTestEnv(t, dsn)
	withArgs(t, fmt.Sprintf("--job=%s", "outbox-prune"))

	exitCode := run()

	require.Equal(t, 0, exitCode, "run() must succeed end-to-end against a freshly migrated database")
}
