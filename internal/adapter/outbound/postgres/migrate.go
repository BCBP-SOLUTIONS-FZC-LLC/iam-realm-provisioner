package postgres

import (
	"context"
	"embed"
	"io/fs"
	"os"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
)

// migrationsFS embeds every .sql file under migrations/. Files are numbered
// NNNNNN_<name>.up.sql / .down.sql (golang-migrate convention, §19).
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations applies all pending domain migrations against dsn. Uses the
// direct Postgres DSN (bypassing PgBouncer) because the runner acquires a
// pg_advisory_lock which is session-scoped. MUST run AFTER
// outbox.ApplySchema (§19 — the domain migration ALTERs outbox_events,
// which must already exist).
//
// pgmigrate.Runner.LockTimeout defaults to DefaultLockTimeout (30 s) when
// zero, which already protects against indefinite DDL-lock waits. PG_LOCK_TIMEOUT
// lets the operator tighten it further — the same env var pgcommon reads for
// transaction-level lock_timeout, re-used here for the migration connection.
func RunMigrations(ctx context.Context, dsn string, log ...port.Logger) error {
	// fs.Sub on an embedded FS with a known directory path is infallible.
	sub, _ := fs.Sub(migrationsFS, "migrations") //nolint:errcheck // infallible, see comment above
	runner := &pgmigrate.Runner{FS: sub, DSN: dsn}
	if len(log) > 0 && log[0] != nil {
		runner.Logger = NewLoggerAdapter(log[0])
	}
	if lt := os.Getenv("PG_LOCK_TIMEOUT"); lt != "" {
		if d, err := time.ParseDuration(lt); err == nil && d > 0 {
			runner.LockTimeout = d
		}
	}
	return runner.Up(ctx)
}
