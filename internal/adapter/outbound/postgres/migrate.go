package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/domain"
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
func RunMigrations(ctx context.Context, dsn string, log ...port.Logger) error {
	// fs.Sub on an embedded FS with a known directory path is infallible.
	sub, _ := fs.Sub(migrationsFS, "migrations") //nolint:errcheck // infallible, see comment above
	runner := &pgmigrate.Runner{FS: sub, DSN: dsn}
	if len(log) > 0 && log[0] != nil {
		runner.Logger = NewLoggerAdapter(log[0])
	}
	return runner.Up(ctx)
}

// MigrateSchema applies the whole schema in its required order (§19):
// platform-events' outbox schema first (outbox.ApplySchema creates
// outbox_events), then this service's domain migrations (which ALTER that
// table). It is the single entry point for both the server's startup
// migration and the migrate Job (cmd/migrate), matching iam-org-membership.
func MigrateSchema(ctx context.Context, dsn string, log port.Logger) error {
	var logger pgdomain.Logger
	if log != nil {
		logger = NewLoggerAdapter(log)
	}
	if err := outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: dsn, Logger: logger}); err != nil {
		return explainMigrationError("outbox schema", err)
	}
	if err := RunMigrations(ctx, dsn, log); err != nil {
		return explainMigrationError("domain migrations", err)
	}
	return nil
}

// explainMigrationError turns platform-pgcommon's migration sentinels into
// an operator-actionable message, keeping the sentinel in the chain.
func explainMigrationError(stage string, err error) error {
	switch {
	case errors.Is(err, pgmigrate.ErrMigrationDirty):
		return fmt.Errorf("%s: a previous migration failed part-way and left the schema dirty — repair it by hand, then force the version (see docs/lld §19): %w", stage, err)
	case errors.Is(err, pgmigrate.ErrVersionNotInSource):
		return fmt.Errorf("%s: the database schema is newer than this image (rolled back to an older build?) — deploy a build that contains the applied migration, or run its down migration first: %w", stage, err)
	default:
		return fmt.Errorf("%s: %w", stage, err)
	}
}
