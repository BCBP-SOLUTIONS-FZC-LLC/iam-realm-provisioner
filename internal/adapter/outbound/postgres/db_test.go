package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

func clearPgEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"DATABASE_URL", "PG_HOST", "PG_PORT", "PG_USER", "PG_PASSWORD", "PG_DBNAME", "PG_SSLMODE",
		"PG_STATEMENT_TIMEOUT", "SYSTEM_DATABASE_URL", "MIGRATION_DATABASE_URL",
	} {
		t.Setenv(k, "")
	}
}

func TestDSNFromEnv_UsesDatabaseURLVerbatim(t *testing.T) {
	clearPgEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@host:5432/db?sslmode=disable")

	got := DSNFromEnv()

	assert.Equal(t, "postgres://user:pass@host:5432/db?sslmode=disable", got,
		"DATABASE_URL must pass through unchanged, not get a statement_timeout option appended")
}

func TestDSNFromEnv_BuildsFromDiscreteVarsAndAppliesStatementTimeout(t *testing.T) {
	clearPgEnv(t)
	t.Setenv("PG_HOST", "localhost")
	t.Setenv("PG_PORT", "5544")
	t.Setenv("PG_USER", "realm_provisioner_app")
	t.Setenv("PG_PASSWORD", "secret")
	t.Setenv("PG_DBNAME", "provisioner")
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")

	got := DSNFromEnv()

	assert.Contains(t, got, "localhost")
	assert.Contains(t, got, "provisioner")
	assert.Contains(t, got, "options=-c%20statement_timeout%3D5000")
}

func TestApplyStatementTimeout_EmptyDSNPassesThrough(t *testing.T) {
	clearPgEnv(t)
	assert.Equal(t, "", ApplyStatementTimeout(""))
}

func TestApplyStatementTimeout_UnsetEnvLeavesDSNUnchanged(t *testing.T) {
	clearPgEnv(t)
	dsn := "postgres://host/db"
	assert.Equal(t, dsn, ApplyStatementTimeout(dsn))
}

func TestApplyStatementTimeout_InvalidDurationLeavesDSNUnchanged(t *testing.T) {
	clearPgEnv(t)
	t.Setenv("PG_STATEMENT_TIMEOUT", "not-a-duration")
	dsn := "postgres://host/db"
	assert.Equal(t, dsn, ApplyStatementTimeout(dsn))
}

func TestApplyStatementTimeout_ZeroOrNegativeDurationLeavesDSNUnchanged(t *testing.T) {
	clearPgEnv(t)
	t.Setenv("PG_STATEMENT_TIMEOUT", "0s")
	dsn := "postgres://host/db"
	assert.Equal(t, dsn, ApplyStatementTimeout(dsn))
}

func TestApplyStatementTimeout_ValidDurationAppendsMillisecondOption(t *testing.T) {
	clearPgEnv(t)
	t.Setenv("PG_STATEMENT_TIMEOUT", "250ms")
	dsn := "postgres://host/db"

	got := ApplyStatementTimeout(dsn)

	assert.True(t, strings.HasPrefix(got, dsn))
	assert.Contains(t, got, "options=-c%20statement_timeout%3D250")
}

func TestSystemDSNFromEnv_PrefersSystemDatabaseURL(t *testing.T) {
	clearPgEnv(t)
	t.Setenv("SYSTEM_DATABASE_URL", "postgres://migrator@host/db")
	t.Setenv("DATABASE_URL", "postgres://app@host/db")

	assert.Equal(t, "postgres://migrator@host/db", SystemDSNFromEnv())
}

func TestSystemDSNFromEnv_FallsBackToDSNFromEnv(t *testing.T) {
	clearPgEnv(t)
	t.Setenv("DATABASE_URL", "postgres://app@host/db")

	assert.Equal(t, "postgres://app@host/db", SystemDSNFromEnv())
}

func TestMigrationDSNFromEnv_PrefersMigrationDatabaseURL(t *testing.T) {
	clearPgEnv(t)
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://migrator@host/db")
	t.Setenv("DATABASE_URL", "postgres://app@host/db")

	assert.Equal(t, "postgres://migrator@host/db", MigrationDSNFromEnv())
}

func TestMigrationDSNFromEnv_FallsBackToDSNFromEnv(t *testing.T) {
	clearPgEnv(t)
	t.Setenv("DATABASE_URL", "postgres://app@host/db")

	assert.Equal(t, "postgres://app@host/db", MigrationDSNFromEnv())
}

// wrapConnErr is a pure function of its input error, so every branch can be
// exercised directly without a database — no repository method in this
// package actually produces a bare pgx.ErrNoRows or a bare context
// cancellation error by the time it reaches wrapConnErr (every repo either
// translates ErrNoRows into a *domain.Error or swallows it), so those two
// passthrough branches are only reachable this way.
func TestWrapConnErr_NilPassesThroughUnchanged(t *testing.T) {
	assert.NoError(t, wrapConnErr(nil))
}

func TestWrapConnErr_DomainErrorPassesThroughUnchanged(t *testing.T) {
	derr := domain.NewError(domain.ErrTenantRealmNotFound, "no such realm")

	got := wrapConnErr(derr)

	assert.Same(t, derr, got, "a *domain.Error must be returned completely unwrapped")
}

func TestWrapConnErr_PgConnErrorPassesThroughUnchanged(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505", ConstraintName: "uq_idp_tenant_alias"}

	got := wrapConnErr(pgErr)

	assert.Same(t, pgErr, got, "a *pgconn.PgError must be returned completely unwrapped so callers can inspect .Code/.ConstraintName")
}

func TestWrapConnErr_ErrNoRowsPassesThroughUnchanged(t *testing.T) {
	got := wrapConnErr(pgx.ErrNoRows)

	assert.Same(t, pgx.ErrNoRows, got)
	assert.True(t, errors.Is(got, pgx.ErrNoRows))
}

func TestWrapConnErr_ContextCanceledPassesThroughUnchanged(t *testing.T) {
	got := wrapConnErr(context.Canceled)

	assert.Same(t, context.Canceled, got)
	assert.True(t, errors.Is(got, context.Canceled))
}

func TestWrapConnErr_DeadlineExceededPassesThroughUnchanged(t *testing.T) {
	got := wrapConnErr(context.DeadlineExceeded)

	assert.Equal(t, context.DeadlineExceeded, got)
	assert.True(t, errors.Is(got, context.DeadlineExceeded))
}

func TestWrapConnErr_GenericErrorIsWrappedAsDatabaseUnavailable(t *testing.T) {
	boom := errors.New("connection refused")

	got := wrapConnErr(boom)

	assert.Error(t, got)
	assert.NotSame(t, boom, got)
	assert.Contains(t, got.Error(), "postgres: database unavailable")
	assert.True(t, errors.Is(got, boom), "the original error must remain unwrappable via errors.Is")
}

// withPool's "join an already-active transaction" branch (TxFromContext
// returning ok=true) is exercised in test/postgres/tx_runner_test.go against
// a real transaction, since pgx.Tx has enough methods that a hand-rolled
// fake would be more fragile than just using the real thing.
