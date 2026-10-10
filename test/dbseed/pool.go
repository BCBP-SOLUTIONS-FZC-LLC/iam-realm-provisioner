// Package dbseed wraps pgcommon.Pool with pgxpool-like Exec/QueryRow/Query
// helpers for test seed/assert SQL. Construction goes through pgcommon so
// seed pools share the same connection, GUC, and drain path as production.
package dbseed

import (
	"context"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is the superuser seed pool used by postgres/e2e tests.
type Pool struct {
	inner *pgcommon.Pool
	dsn   string
}

// New opens a pgcommon pool against dsn (typically the testcontainer
// superuser URL). PGBouncerMode is left false — tests talk to Postgres
// directly, not through a pooler.
func New(ctx context.Context, dsn string) (*Pool, error) {
	inner, err := pgcommon.NewPool(ctx, pgcommon.Config{
		DSN:           dsn,
		PGBouncerMode: false,
	})
	if err != nil {
		return nil, err
	}
	return &Pool{inner: inner, dsn: dsn}, nil
}

// Close closes the underlying pgcommon pool.
func (p *Pool) Close() {
	if p != nil && p.inner != nil {
		p.inner.Close()
	}
}

// Exec runs sql on the superuser pool.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	var tag pgconn.CommandTag
	err := p.inner.WithConn(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		var execErr error
		tag, execErr = conn.Exec(ctx, sql, args...)
		return execErr
	})
	return tag, err
}

// QueryRow returns a row whose Scan runs inside WithConn so the connection
// is still held when dest is populated (pgxpool.QueryRow Scan is deferred).
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &lazyRow{pool: p.inner, ctx: ctx, sql: sql, args: args}
}

type lazyRow struct {
	pool *pgcommon.Pool
	ctx  context.Context
	sql  string
	args []any
}

func (r *lazyRow) Scan(dest ...any) error {
	return r.pool.WithConn(r.ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		return conn.QueryRow(ctx, r.sql, r.args...).Scan(dest...)
	})
}

// Query runs sql and buffers the result set so rows remain readable after
// the connection is returned to the pool.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	var out *bufferedRows
	err := p.inner.WithConn(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		rows, err := conn.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		fields := append([]pgconn.FieldDescription(nil), rows.FieldDescriptions()...)
		var collected [][][]byte
		for rows.Next() {
			raw := rows.RawValues()
			copied := make([][]byte, len(raw))
			for i, v := range raw {
				if v != nil {
					copied[i] = append([]byte(nil), v...)
				}
			}
			collected = append(collected, copied)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = &bufferedRows{
			fields: fields,
			data:   collected,
			idx:    -1,
			tag:    rows.CommandTag(),
			m:      pgtype.NewMap(),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = &bufferedRows{idx: -1, m: pgtype.NewMap()}
	}
	return out, nil
}

// WithTx runs fn inside a transaction on the superuser pool.
func (p *Pool) WithTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return p.inner.WithTx(ctx, pgx.TxOptions{}, fn)
}

// Config matches pgxpool.Pool.Config() for tests that extract the DSN
// (migrations round-trip).
func (p *Pool) Config() *pgxpool.Config {
	cfg, err := pgxpool.ParseConfig(p.dsn)
	if err != nil {
		return &pgxpool.Config{}
	}
	return cfg
}

type bufferedRows struct {
	fields []pgconn.FieldDescription
	data   [][][]byte
	idx    int
	err    error
	tag    pgconn.CommandTag
	closed bool
	m      *pgtype.Map
}

func (r *bufferedRows) Close()                                       { r.closed = true }
func (r *bufferedRows) Err() error                                   { return r.err }
func (r *bufferedRows) CommandTag() pgconn.CommandTag                { return r.tag }
func (r *bufferedRows) FieldDescriptions() []pgconn.FieldDescription { return r.fields }
func (r *bufferedRows) Conn() *pgx.Conn                              { return nil }
func (r *bufferedRows) TypeMap() *pgtype.Map                         { return r.m }

func (r *bufferedRows) Next() bool {
	if r.closed {
		return false
	}
	r.idx++
	return r.idx < len(r.data)
}

func (r *bufferedRows) Scan(dest ...any) error {
	if r.idx < 0 || r.idx >= len(r.data) {
		return fmt.Errorf("dbseed: Scan called without Next")
	}
	raw := r.data[r.idx]
	for i := range dest {
		var oid uint32
		var format int16
		if i < len(r.fields) {
			oid = r.fields[i].DataTypeOID
			format = r.fields[i].Format
		}
		var src []byte
		if i < len(raw) {
			src = raw[i]
		}
		if err := r.m.Scan(oid, format, src, dest[i]); err != nil {
			return err
		}
	}
	return nil
}

func (r *bufferedRows) Values() ([]any, error) {
	if r.idx < 0 || r.idx >= len(r.data) {
		return nil, fmt.Errorf("dbseed: Values called without Next")
	}
	raw := r.data[r.idx]
	out := make([]any, len(raw))
	for i := range raw {
		var oid uint32
		var format int16
		if i < len(r.fields) {
			oid = r.fields[i].DataTypeOID
			format = r.fields[i].Format
		}
		if err := r.m.Scan(oid, format, raw[i], &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *bufferedRows) RawValues() [][]byte {
	if r.idx < 0 || r.idx >= len(r.data) {
		return nil
	}
	return r.data[r.idx]
}
