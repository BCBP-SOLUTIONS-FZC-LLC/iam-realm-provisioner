// Package postgres implements the outbound repository ports backed by
// PostgreSQL through platform-pgcommon. TxRunner is the seam that binds
// transaction-local RLS GUC + tx-bound outbox publisher onto every write.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DSNFromEnv builds the application-pool DSN by delegating to
// pgcommon.ConfigFromEnv() — the same env vars (DATABASE_URL/PG_HOST/
// PG_PORT/PG_USER/PG_PASSWORD/PG_DBNAME/PG_SSLMODE) platform-pgcommon
// itself reads, so there is exactly one DSN-assembly implementation.
func DSNFromEnv() string {
	cfg, _ := pgcommon.ConfigFromEnv()
	if os.Getenv("DATABASE_URL") != "" {
		return cfg.DSN
	}
	return ApplyStatementTimeout(cfg.DSN)
}

// ApplyStatementTimeout appends a server-side statement_timeout option to
// dsn (PG_STATEMENT_TIMEOUT, a Go duration string) so hung queries release
// pool connections instead of holding them for the full HTTP deadline.
func ApplyStatementTimeout(dsn string) string {
	if dsn == "" {
		return dsn
	}
	if t := os.Getenv("PG_STATEMENT_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil && d > 0 {
			dsn += fmt.Sprintf("&options=-c%%20statement_timeout%%3D%d", d.Milliseconds())
		}
	}
	return dsn
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

// MigrationDSNFromEnv returns the DSN for schema migrations. Migrations
// MUST bypass PgBouncer because the migration runner uses pg_advisory_lock,
// which is session-scoped and breaks under transaction pooling.
// MIGRATION_DATABASE_URL must be set whenever PG_BOUNCER_MODE=true.
func MigrationDSNFromEnv() string {
	if dsn := os.Getenv("MIGRATION_DATABASE_URL"); dsn != "" {
		return dsn
	}
	return DSNFromEnv()
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

// RunInTx runs fn inside a transaction, injects a tx-bound publisher into
// the ctx, and maps low-level connection errors into a domain
// keycloak_unavailable/org_membership_unavailable-adjacent 503 shape
// (ErrObjectStoreUnavailable reuse would be wrong here — a generic wrapped
// error is returned; callers classify via errors.As on *domain.Error for
// anything already typed, or treat the fallback as a transient failure).
func (r *TxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return wrapConnErr(pgcommon.RunInTx(ctx, r.pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		txCtx := withTx(ctx, tx)
		if r.events != nil {
			txCtx = port.WithEventPublisher(txCtx, &txBoundPublisher{pub: r.events, tx: tx})
		}
		return fn(txCtx)
	}))
}

// txBoundPublisher adapts port.EventPublisher (requires pgx.Tx) to
// port.ContextEventPublisher (used via context lookup by the service layer).
type txBoundPublisher struct {
	pub port.EventPublisher
	tx  pgx.Tx
}

func (p *txBoundPublisher) EnqueueCtx(ctx context.Context, event *domain.Event) error {
	return p.pub.Enqueue(ctx, p.tx, event)
}

// txKey stores the active pgx.Tx in context so repository methods can join
// an in-flight transaction rather than opening a nested one.
type txKey struct{}

func withTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// TxFromContext retrieves the active pgx.Tx set by RunInTx, if any.
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

// wrapConnErr passes SQL-protocol errors, ErrNoRows, and context
// cancellations through unchanged (service layer classifies them); any
// other error is a connectivity failure surfaced as
// ErrOrgMembershipUnavailable's sibling for Postgres — kept generic so
// callers can errors.Is(err, pgx.ErrNoRows) etc. without a new domain code.
func wrapConnErr(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*domain.Error](err); ok { //nolint:errcheck // only the match bool is needed; err itself is returned unchanged below
		return err
	}
	if _, ok := errors.AsType[*pgconn.PgError](err); ok { //nolint:errcheck // only the match bool is needed; err itself is returned unchanged below
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("postgres: database unavailable: %w", err)
}
