package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/domain"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
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
	if err := AdoptAuditOutbox(ctx, dsn, logger); err != nil {
		return explainMigrationError("platform-audit outbox", err)
	}
	return nil
}

// AdoptAuditOutbox brings pending_audit_entries to platform-audit's outbox
// shape and applies the library's own migrations (platform-audit iam-adopters
// §5.1, LLD §19.3). Release N+1, the contract:
//
//  1. AdoptAsBuilt converts the as-built table that 000000_initial_schema
//     creates. Still required: a fresh database (CI, dev, a new environment)
//     gets the as-built shape from 000000, and ApplySchema refuses it
//     (auditoutbox.ErrAsBuiltNotAdopted). On a converted table it is a no-op.
//  2. FinishAdoption drops release N's compatibility trigger: no pod running
//     the pre-library adapter remains once release N has run in production
//     without rollback. Idempotent. To roll back to pre-library code after
//     this, run auditoutbox.ReinstallCompat first.
//  3. ApplySchema applies the library's migrations, which no longer hold at
//     version 1.
//
// Every step is idempotent, so every startup and every migrate Job run calls
// them. Each takes a brief ACCESS EXCLUSIVE lock on the table (bounded by
// lock_timeout), once per migrate run.
//
// AdoptAsBuilt and FinishAdoption run on their own single-connection pool
// over dsn, which must be the migrator role: it owns the table and holds
// BYPASSRLS (without it the backfill under FORCE RLS would update 0 rows,
// auditoutbox.ErrNotBypassRLS).
func AdoptAuditOutbox(ctx context.Context, dsn string, logger pgdomain.Logger) error {
	pool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: dsn, MaxConns: 1, PoolName: "platform_audit_adopt"})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	if err := auditoutbox.AdoptAsBuilt(ctx, pool, auditoutbox.AsBuiltRealmProvisioner, auditoutbox.AdoptOptions{}); err != nil {
		return fmt.Errorf("adopt as-built pending_audit_entries: %w", err)
	}
	if err := auditoutbox.FinishAdoption(ctx, pool); err != nil {
		return fmt.Errorf("finish adoption of pending_audit_entries: %w", err)
	}
	return auditoutbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: dsn, Logger: logger})
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
