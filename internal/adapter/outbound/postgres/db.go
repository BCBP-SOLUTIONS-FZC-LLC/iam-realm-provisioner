// Package postgres implements the outbound repository ports backed by
// PostgreSQL through platform-pgcommon. TxRunner is the seam that binds
// transaction-local RLS GUC + tx-bound outbox publisher onto every write.
package postgres

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// DSNFromEnv builds a PostgreSQL connection URL for the application pool by
// delegating host/port/user/password/dbname/sslmode parsing and DSN
// assembly to pgcommon.ConfigFromEnv() — the same env vars
// (DATABASE_URL/PG_HOST/PG_PORT/PG_USER/PG_PASSWORD/PG_DBNAME/PG_SSLMODE)
// platform-pgcommon itself reads to build the pool Config used by main.go,
// so there is exactly one DSN-assembly implementation instead of two drifting
// in parallel. Warnings from ConfigFromEnv (invalid/insecure settings
// replaced by defaults) are surfaced at the call site that owns a logger
// (see cmd/server/main.go); this helper only returns the DSN string.
//
// URL format is required because the migration runner (pgmigrate.Runner)
// calls url.Parse on the DSN after prepending "pgx5://"; a keyword/value DSN
// would produce invalid URL escapes (%20 for spaces) and fail at startup.
// pgcommon builds the DSN via net/url, which already produces this format.
//
// PG_STATEMENT_TIMEOUT is not part of the DSN: ConfigFromEnv reads it into
// Config.StatementTimeout and pgcommon applies it per transaction
// (SET LOCAL), which is safe behind PgBouncer transaction pooling.
func DSNFromEnv() string {
	cfg, _ := pgcommon.ConfigFromEnv()
	return cfg.DSN
}

// SystemPoolName is the pool metric label of the BYPASSRLS system pool, so
// its platform_db_* series stay separate from the app pool's.
const SystemPoolName = "system"

// SystemPoolConfig returns pgcommon.Config for the BYPASSRLS system pool
// (§10.1), matching iam-org-membership. The system pool deliberately has
// no GUCProvider — cross-tenant reconciler / exporter queries run under a
// BYPASSRLS role — but still connects through PgBouncer in production, so
// PGBouncerMode is forced true unconditionally (SimpleProtocol + MinConns:0).
// A bare pgcommon.Config{DSN, Logger} literal would leave PGBouncerMode at
// the Go zero-value false and drop ConfigFromEnv pool sizing, breaking
// transaction-pooling deployments even when the app pool correctly reads
// PG_BOUNCER_MODE.
//
// Pool sizing, lifetimes, SlowQueryThreshold and StatementTimeout are
// copied from ConfigFromEnv so sysPool and the app pool share one
// env-driven source of truth. PoolName is always SystemPoolName. Tracer is
// left unset — call sites wire gincommon.NewSpanTracer so db.query spans
// export through gincommon's TracerProvider.
func SystemPoolConfig(dsn string, log port.Logger) pgcommon.Config {
	cfg, _ := pgcommon.ConfigFromEnv()
	cfg.DSN = dsn
	cfg.PoolName = SystemPoolName
	cfg.GUCProvider = nil
	cfg.PGBouncerMode = true
	// ConfigFromEnv chose its MinConns default for the PG_BOUNCER_MODE it
	// read, before PgBouncer mode was forced on here. Unless PG_MIN_CONNS was
	// set deliberately (an empty value counts as unset, as ConfigFromEnv
	// treats it), zero it so NewPool applies the PgBouncer default (0 — let
	// PgBouncer manage backends).
	if strings.TrimSpace(os.Getenv("PG_MIN_CONNS")) == "" {
		cfg.MinConns = 0
	}
	cfg.Tracer = nil
	if log != nil {
		cfg.Logger = NewLoggerAdapter(log)
	} else {
		cfg.Logger = nil
	}
	return cfg
}

// SystemDSNFromEnv returns the DSN for the privileged cross-tenant pool used
// by the reconciler binary (RP-C1..C4). In production it MUST point to
// realm_provisioner_migrator (BYPASSRLS). Falls back to DSNFromEnv() for
// local dev — cross-tenant queries will then be RLS-filtered to zero rows
// (safe no-op).
func SystemDSNFromEnv() string {
	if dsn := os.Getenv("SYSTEM_DATABASE_URL"); dsn != "" {
		return dsn
	}
	return DSNFromEnv()
}

// MigrationDSNFromEnv returns the DSN for schema migrations:
// MIGRATION_DATABASE_URL when set, else the app DSN (pgcommon's own
// MigrationDSNFromEnv). Migrations MUST bypass PgBouncer because the
// migration runner uses pg_advisory_lock, which is session-scoped and
// breaks under transaction pooling, so MIGRATION_DATABASE_URL must be set
// whenever PG_BOUNCER_MODE is on. No statement timeout is applied: long DDL
// and the advisory-lock wait must not be cut short.
func MigrationDSNFromEnv() string {
	return pgcommon.MigrationDSNFromEnv()
}

// TxRunner wraps pgcommon.Pool to implement port.TxRunner. When a
// port.EventPublisher is provided it is injected into the tx context so
// services can enqueue events atomically via
// port.EventPublisherFromContext(txCtx) without importing this package.
type TxRunner struct {
	pool   *pgcommon.Pool
	events port.EventPublisher
}

// NewTxRunner constructs a TxRunner. Pass nil for events during bootstrap
// paths (e.g. migration-only reconciler runs) where no outbox writes occur.
func NewTxRunner(pool *pgcommon.Pool, events port.EventPublisher) *TxRunner {
	return &TxRunner{pool: pool, events: events}
}

// writeRetryOpts is pgcommon's documented high-throughput OLTP preset.
// Deadlock (40P01) and serialization failure (40001) retry with exponential
// backoff + jitter. Nested withPool joins (already inside a tx) do not
// retry — the outer TxRunner owns the attempt.
var writeRetryOpts = pgcommon.RetryOptions{
	MaxAttempts:    3,
	InitialWait:    10 * time.Millisecond,
	MaxWait:        500 * time.Millisecond,
	Multiplier:     2.0,
	JitterFraction: 0.25,
}

// RunInTx runs fn inside a transaction, injects a tx-bound publisher into
// the ctx, and maps low-level connection errors into
// domain.ErrDBUnavailable (503) so handlers get a consistent 5xx shape
// (§17). SQL-level errors (unique violation, FK, check) bubble up
// unchanged for service-layer classification. Contended writes retry via
// pgcommon.RunInTxWithRetryOpts on deadlock / serialization failure.
func (r *TxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return wrapConnErr(pgcommon.RunInTxWithRetryOpts(ctx, r.pool, pgcommon.TxOptions{}, writeRetryOpts, func(ctx context.Context, tx pgcommon.Tx) error {
		txCtx := WithTx(ctx, tx)
		if r.events != nil {
			txCtx = port.WithEventPublisher(txCtx, r.events)
		}
		return fn(txCtx)
	}))
}

// WithTx stores the active pgcommon.Tx in ctx so repository withPool joins and
// EventPublisher.Enqueue writes the outbox on the same transaction.
func WithTx(ctx context.Context, tx pgcommon.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

type txKey struct{}

// TxFromContext retrieves the active pgcommon.Tx set by RunInTx, if any.
// Repository helpers call withPool which joins the tx when present.
func TxFromContext(ctx context.Context) (pgcommon.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgcommon.Tx)
	return tx, ok
}

// withPool runs fn inside a transaction, joining an existing one if present
// in ctx. Used by repository helpers so a read outside a service tx still
// binds RLS via pgcommon.RunInTx's checkout hook.
func withPool(ctx context.Context, pool *pgcommon.Pool, fn func(pgcommon.Tx) error) error {
	if tx, ok := TxFromContext(ctx); ok {
		return wrapConnErr(fn(tx))
	}
	return wrapConnErr(pgcommon.RunInTx(ctx, pool, pgcommon.TxOptions{}, func(_ context.Context, tx pgcommon.Tx) error {
		return fn(tx)
	}))
}

// wrapConnErr converts non-protocol database errors into
// ErrDBUnavailable. SQL-protocol errors (*pgconn.PgError) that are
// not connectivity/resource classes pass through so the service layer can
// distinguish an integrity violation from a network outage.
//
// SQLSTATE class 08 (connection exception), 53 (insufficient resources),
// 57 (operator intervention, except 57014) and 58 (system error) are
// remapped to domain.ErrDBUnavailable here (IsDBUnavailable, pgcommon's
// classifiers) so HTTP HandleError never needs to inspect a raw
// *pgconn.PgError — those classes are availability failures (503). A closed
// pool (pgcommon.IsPoolClosed) is the other positively-identifiable
// connectivity failure: never a SQL-protocol response, but unambiguously
// "the database is unavailable", not a caller's business error.
//
// Everything else — including a plain Go error a caller's own RunInTx/
// withPool callback returns for its own business reasons — passes through
// completely unchanged. This function has no way to distinguish "the pool
// itself failed" from "fn's own business logic failed" for any error
// shape beyond the ones positively recognized above, since
// pgcommon.RunInTxWithRetryOpts returns both shapes identically; defaulting
// the unrecognized case to ErrDBUnavailable (as this used to) silently
// discarded the caller's real error under a misleading "database
// unavailable" 503 for every unrecognized failure — including deliberate
// business-rule errors a service intentionally returns from inside a
// transaction. HandleError (inbound/http/errors.go) already re-classifies
// a leaked raw PgError of these same connectivity/resource classes into
// 503 independently, so a genuine low-level connectivity failure that
// somehow isn't positively recognized here still degrades no worse than a
// generic 500, never a masked/wrong business error.
func wrapConnErr(err error) error {
	if err == nil {
		return nil
	}
	var de *domain.Error
	if errors.As(err, &de) {
		return err
	}
	if pgcommon.IsQueryCanceled(err) || pgcommon.IsLockNotAvailable(err) {
		return domain.NewError(domain.ErrRequestTimeout, "database statement or lock timeout")
	}
	if IsDBUnavailable(err) {
		return domain.NewError(domain.ErrDBUnavailable, "database unavailable")
	}
	// pgx returns Go-level network errors when the connection is dropped
	// mid-flight (e.g. Postgres container stopped while a query is in
	// flight). These are not *pgconn.PgError values so the SQLSTATE checks
	// above miss them — catch them here so callers always get 503, not 500.
	if isNetworkError(err) {
		return domain.NewError(domain.ErrDBUnavailable, "database unavailable")
	}
	return err
}

// isNetworkError reports whether err is a Go-level network/IO failure that
// pgx surfaces when the TCP connection to Postgres is lost mid-flight
// (container stop, network partition, ECONNRESET). These never reach the
// SQLSTATE classification path because pgx never received a protocol
// response — they are unambiguously availability failures (503).
func isNetworkError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	// syscall errors: ECONNRESET, ECONNREFUSED, EPIPE, ETIMEDOUT
	var sysErr syscall.Errno
	if errors.As(err, &sysErr) {
		switch sysErr { //nolint:exhaustive // only transient-connection codes are retryable
		case syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE, syscall.ETIMEDOUT:
			return true
		}
	}
	// pgx wraps some errors in a string — catch "connection refused" and
	// "connection reset" as a last-resort text match.
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "EOF")
}

// IsDBUnavailable reports whether err is a database availability failure
// pgcommon can classify: SQLSTATE class 08, 53, 57 (except 57014
// query_canceled) or 58, or a closed pool (pgcommon's own sentinel after
// Close/DrainAndClose, or pgxpool's).
func IsDBUnavailable(err error) bool {
	return pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) ||
		pgcommon.IsOperatorIntervention(err) || pgcommon.IsSystemError(err) || pgcommon.IsPoolClosed(err)
}
