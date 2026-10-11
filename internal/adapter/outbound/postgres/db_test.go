package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func clearPgEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"DATABASE_URL", "PG_HOST", "PG_PORT", "PG_USER", "PG_PASSWORD", "PG_DBNAME", "PG_SSLMODE",
		"PG_STATEMENT_TIMEOUT", "SYSTEM_DATABASE_URL", "MIGRATION_DATABASE_URL",
		"PG_BOUNCER_MODE", "PG_MAX_CONNS", "PG_MIN_CONNS", "PG_SLOW_QUERY_THRESHOLD",
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

// PG_STATEMENT_TIMEOUT is never baked into the DSN: pgcommon applies
// Config.StatementTimeout per transaction (SET LOCAL), which is safe behind
// PgBouncer transaction pooling.
func TestDSNFromEnv_BuildsFromDiscreteVarsWithoutStatementTimeout(t *testing.T) {
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
	assert.NotContains(t, got, "statement_timeout")
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

// Long DDL and the advisory-lock wait must never be cut short.
func TestMigrationDSNFromEnv_NeverAppliesStatementTimeout(t *testing.T) {
	clearPgEnv(t)
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://m:x@migrations.example:5432/db?sslmode=disable")
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	assert.Equal(t, "postgres://m:x@migrations.example:5432/db?sslmode=disable", MigrationDSNFromEnv())
}

// The system pool reports its platform_db_* series under its own pool
// label, inherits the per-transaction statement timeout, and — PgBouncer
// mode being forced — lets PgBouncer manage backends unless PG_MIN_CONNS is
// set deliberately.
func TestSystemPoolConfig_PoolNameStatementTimeoutAndMinConns(t *testing.T) {
	clearPgEnv(t)
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	cfg := SystemPoolConfig("postgres://m@h/db", nil)
	assert.Equal(t, SystemPoolName, cfg.PoolName)
	assert.Equal(t, 5*time.Second, cfg.StatementTimeout)
	assert.Zero(t, cfg.MinConns)

	t.Setenv("PG_MIN_CONNS", "3")
	assert.Equal(t, int32(3), SystemPoolConfig("postgres://m@h/db", nil).MinConns)
}

func TestMigrationDSNFromEnv_FallsBackToDSNFromEnv(t *testing.T) {
	clearPgEnv(t)
	t.Setenv("DATABASE_URL", "postgres://app@host/db")

	assert.Equal(t, "postgres://app@host/db", MigrationDSNFromEnv())
}

func TestSystemPoolConfig_ForcesPGBouncerMode(t *testing.T) {
	clearPgEnv(t)
	t.Setenv("PG_BOUNCER_MODE", "false")
	t.Setenv("PG_MAX_CONNS", "20")
	t.Setenv("PG_SLOW_QUERY_THRESHOLD", "200ms")

	cfg := SystemPoolConfig("postgres://sys@host/db", nil)
	assert.Equal(t, "postgres://sys@host/db", cfg.DSN)
	assert.True(t, cfg.PGBouncerMode, "sysPool must force PGBouncerMode:true — zero-value false breaks PgBouncer txn pooling")
	assert.Nil(t, cfg.GUCProvider, "sysPool must not inject tenant GUCs")
	assert.Nil(t, cfg.Tracer, "Tracer is wired by the call site, not SystemPoolConfig")
	assert.Nil(t, cfg.Logger, "nil log must leave Logger unset")
	assert.Equal(t, int32(20), cfg.MaxConns, "sysPool inherits pool sizing from ConfigFromEnv")
}

// noopPortLogger is a minimal port.Logger for exercising SystemPoolConfig's
// non-nil-logger branch — the nil case is already covered above.
type noopPortLogger struct{}

func (noopPortLogger) Debug(msg string, fields map[string]any) {}
func (noopPortLogger) Info(msg string, fields map[string]any)  {}
func (noopPortLogger) Warn(msg string, fields map[string]any)  {}
func (noopPortLogger) Error(msg string, fields map[string]any) {}

func TestSystemPoolConfig_NonNilLoggerIsWrapped(t *testing.T) {
	clearPgEnv(t)
	cfg := SystemPoolConfig("postgres://sys@host/db", noopPortLogger{})
	assert.Equal(t, NewLoggerAdapter(noopPortLogger{}), cfg.Logger, "a non-nil port.Logger must be wrapped via NewLoggerAdapter")
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

	assert.Same(t, pgErr, got, "a non-availability *pgconn.PgError must pass through so callers can inspect .Code/.ConstraintName")
}

func TestWrapConnErr_AvailabilitySQLStateMapsToDBUnavailable(t *testing.T) {
	for _, code := range []string{"08006", "53300", "57P01", "58030"} {
		t.Run(code, func(t *testing.T) {
			pgErr := &pgconn.PgError{Code: code}
			got := wrapConnErr(pgErr)
			var de *domain.Error
			assert.True(t, errors.As(got, &de))
			assert.Equal(t, domain.ErrDBUnavailable, de.Code)
		})
	}
}

// 57014 (query_canceled — statement_timeout fired) and 55P03
// (lock_not_available — lock_timeout fired) are slow-query/contention
// signals, not outages: request_timeout (503), never db_unavailable.
func TestWrapConnErr_StatementOrLockTimeoutMapsToRequestTimeout(t *testing.T) {
	for _, code := range []string{"57014", "55P03"} {
		got := wrapConnErr(&pgconn.PgError{Code: code})
		var de *domain.Error
		require.True(t, errors.As(got, &de), code)
		assert.Equal(t, domain.ErrRequestTimeout, de.Code, code)
		assert.Equal(t, 503, de.Status())
	}
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

// CRITICAL: an unrecognized plain Go error — the shape a caller's own
// RunInTx/withPool callback returns for its own business reasons — must
// pass through completely unchanged, not get silently reclassified as
// ErrDBUnavailable. wrapConnErr has no way to distinguish "the pool itself
// failed" from "fn's own business logic failed" for anything it can't
// positively identify as a connectivity/resource failure, so defaulting an
// unrecognized error to ErrDBUnavailable would discard the caller's real
// error under a misleading 503 — exactly the bug this test guards against.
func TestWrapConnErr_UnrecognizedGenericErrorPassesThroughUnchanged(t *testing.T) {
	businessErr := errors.New("boom: simulated business-rule failure")

	got := wrapConnErr(businessErr)

	assert.Same(t, businessErr, got)
	assert.True(t, errors.Is(got, businessErr))
}

// puddle.ErrClosedPool (surfaced by pgxpool.Pool.BeginTx/Acquire on a
// closed pool) is the one non-PgError shape wrapConnErr does positively
// recognize as a genuine connectivity failure — a closed pool is never a
// SQL-protocol response, but is unambiguously "the database is
// unavailable", not a caller's business error.
func TestWrapConnErr_ClosedPoolMapsToDBUnavailable(t *testing.T) {
	got := wrapConnErr(puddle.ErrClosedPool)

	var de *domain.Error
	require.True(t, errors.As(got, &de))
	assert.Equal(t, domain.ErrDBUnavailable, de.Code)
}

// wrapConnErr's own isNetworkError branch (a bare Go-level network error,
// not wrapped in *pgconn.PgError) maps to ErrDBUnavailable just like the
// SQLSTATE-classified branch above.
func TestWrapConnErr_NetworkErrorMapsToDBUnavailable(t *testing.T) {
	got := wrapConnErr(io.EOF)

	var de *domain.Error
	require.True(t, errors.As(got, &de))
	assert.Equal(t, domain.ErrDBUnavailable, de.Code)
}

// isNetworkError's branches are only reachable through wrapConnErr when pgx
// surfaces a Go-level network failure directly (connection dropped
// mid-flight) rather than a *pgconn.PgError — exercised directly here since
// synthesizing each underlying error shape (net.OpError, syscall.Errno,
// wrapped io.EOF, and the last-resort text match) is much simpler than
// forcing a real testcontainer connection to fail in each of these ways.
func TestIsNetworkError_IOEOFVariants(t *testing.T) {
	assert.True(t, isNetworkError(io.EOF))
	assert.True(t, isNetworkError(io.ErrUnexpectedEOF))
	assert.True(t, isNetworkError(fmt.Errorf("wrapped: %w", io.EOF)))
}

func TestIsNetworkError_NetOpError(t *testing.T) {
	opErr := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	assert.True(t, isNetworkError(opErr))
}

func TestIsNetworkError_SyscallErrno(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE, syscall.ETIMEDOUT} {
		assert.True(t, isNetworkError(errno), errno.Error())
	}
}

func TestIsNetworkError_SyscallErrno_NonRetryableCodeNotMatched(t *testing.T) {
	// A syscall.Errno that is NOT one of the four explicitly retried codes
	// must fall through to the text-match branch, not be misclassified by
	// the switch alone.
	assert.False(t, isNetworkError(syscall.EACCES))
}

func TestIsNetworkError_TextMatchFallback(t *testing.T) {
	for _, msg := range []string{
		"dial tcp: connection refused",
		"read: connection reset by peer",
		"write: broken pipe",
		"unexpected EOF while reading",
	} {
		assert.True(t, isNetworkError(errors.New(msg)), msg)
	}
}

func TestIsNetworkError_UnrelatedErrorNotNetwork(t *testing.T) {
	assert.False(t, isNetworkError(errors.New("unique constraint violated")))
}

// withPool's "join an already-active transaction" branch (TxFromContext
// returning ok=true) is exercised in test/postgres/tx_runner_test.go against
// a real transaction, since pgx.Tx has enough methods that a hand-rolled
// fake would be more fragile than just using the real thing.
