// Command migrate brings the provisioner database to this build's schema and
// exits: platform-events' outbox schema, then the domain migrations
// (postgres.MigrateSchema, §19). The Helm chart runs it once per release as
// a pre-install/pre-upgrade hook Job, so the server replicas
// (RUN_MIGRATIONS=false) never race for the migration lock at startup —
// matching iam-org-membership.
//
// It needs only the direct, non-PgBouncer DSN: MIGRATION_DATABASE_URL, or the
// application DSN (DATABASE_URL / PG_*) when that is unset. Exit status 0 on
// success (including "already up to date"), 1 on any failure.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
)

// migrateTimeout bounds the whole run, lock wait included
// (MIGRATE_TIMEOUT overrides it). Keep it below the Job's
// activeDeadlineSeconds.
const migrateTimeout = 10 * time.Minute

func main() {
	os.Exit(run())
}

func run() int {
	log, err := logger.NewLogger(loggerEnv())
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: init logger: %v\n", err)
		return 1
	}
	defer func() {
		if err := gincommon.Shutdown(log); err != nil { // flushes the logger
			fmt.Fprintf(os.Stderr, "migrate: flush logger: %v\n", err)
		}
	}()

	dsn := pgadapter.MigrationDSNFromEnv()
	if dsn == "" {
		log.Error("migrate: no database DSN — set MIGRATION_DATABASE_URL (direct to Postgres, not PgBouncer)", nil)
		return 1
	}

	timeout := migrateTimeout
	if v := os.Getenv("MIGRATE_TIMEOUT"); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil && d > 0 {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	if err := pgadapter.MigrateSchema(ctx, dsn, log); err != nil {
		log.Error("migrate failed", map[string]interface{}{"error": err.Error()})
		return 1
	}
	log.Info("migrate complete", map[string]interface{}{"duration_ms": time.Since(start).Milliseconds()})
	return 0
}

// loggerEnv maps APP_ENV (else ENVIRONMENT, default dev) onto the value
// gincommon's logger understands: development output for local/dev/test,
// production JSON otherwise — the same mapping as cmd/server.
func loggerEnv() string {
	env := "dev"
	for _, key := range []string{"APP_ENV", "ENVIRONMENT"} {
		if v := strings.ToLower(strings.TrimSpace(os.Getenv(key))); v != "" {
			env = v
			break
		}
	}
	switch env {
	case "local", "dev", "test":
		return "dev"
	}
	return env
}
