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
	"github.com/jackc/pgx/v5"
)

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
// Pool sizing, lifetimes, SlowQueryThreshold, StatementTimeout, and
// LockTimeout are copied from ConfigFromEnv so sysPool and the app pool
// share one env-driven source of truth — pgcommon v2 applies
// PG_STATEMENT_TIMEOUT / PG_LOCK_TIMEOUT as SET LOCAL per transaction, not
// as a DSN parameter. Tracer is left unset — call sites wire NewOTelTracer
// so db.query spans export through gincommon's TracerProvider.
func SystemPoolConfig(dsn string, log port.Logger) pgcommon.Config {
	cfg, _ := pgcommon.ConfigFromEnv()
	cfg.DSN = dsn
	cfg.GUCProvider = nil
	cfg.PGBouncerMode = true
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
// realm_provisioner_migrator (BYPASSRLS). Falls back to the application DSN
// from pgcommon.ConfigFromEnv for local dev — cross-tenant queries will then
// be RLS-filtered to zero rows (safe no-op).
func SystemDSNFromEnv() string {
	if dsn := os.Getenv("SYSTEM_DATABASE_URL"); dsn != "" {
		return dsn
	}
	cfg, _ := pgcommon.ConfigFromEnv()
	return cfg.DSN
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
	return wrapConnErr(pgcommon.RunInTxWithRetryOpts(ctx, r.pool, pgx.TxOptions{}, writeRetryOpts, func(ctx context.Context, tx pgx.Tx) error {
		txCtx := WithTx(ctx, tx)
		if r.events != nil {
			txCtx = port.WithEventPublisher(txCtx, r.events)
		}
		return fn(txCtx)
	}))
}

// WithTx stores the active pgx.Tx in ctx so repository withPool joins and
// EventPublisher.Enqueue writes the outbox on the same transaction.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

type txKey struct{}

// TxFromContext retrieves the active pgx.Tx set by RunInTx, if any.
// Repository helpers call withPool which joins the tx when present.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}

// withPool runs fn inside a transaction, joining an existing one if present
// in ctx. Used by repository helpers so a read outside a service tx still
// binds RLS via pgcommon.RunInTx's checkout hook.
func withPool(ctx context.Context, pool *pgcommon.Pool, fn func(pgx.Tx) error) error {
	if tx, ok := TxFromContext(ctx); ok {
		return wrapConnErr(fn(tx))
	}
	return wrapConnErr(pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, func(_ context.Context, tx pgx.Tx) error {
		return fn(tx)
	}))
}

// wrapConnErr converts non-protocol database errors into
// ErrDBUnavailable. SQL-protocol errors (*pgconn.PgError) that are
// not connectivity/resource classes pass through so the service layer can
// distinguish an integrity violation from a network outage.
//
// SQLSTATE class 08 (connection exception), 53 (insufficient resources),
// 57 (operator intervention, excluding 57014 query_canceled which is a
// slow-query signal, not an outage), and 58 (system error) are remapped to
// domain.ErrDBUnavailable here so HTTP HandleError never needs to inspect
// a raw *pgconn.PgError — those classes are availability failures (503).
// pgcommon.IsPoolClosed covers both pgcommon's own closed-pool sentinel and
// puddle.ErrClosedPool: a closed pool is never a SQL-protocol response but
// is unambiguously "the database is unavailable", not a caller's business error.
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
	// IsQueryCanceled (57014) is included: statement_timeout expiry is transient.
	// Returning 503 preserves caller retry logic and avoids leaving idempotency
	// ledger rows stranded in 'in_progress' with no recoverable path.
	if pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) ||
		pgcommon.IsOperatorIntervention(err) || pgcommon.IsSystemError(err) ||
		pgcommon.IsPoolClosed(err) || pgcommon.IsQueryCanceled(err) {
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
