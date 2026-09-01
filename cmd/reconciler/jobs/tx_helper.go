package jobs

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
)

// runInTxWithSysPool runs fn inside a pgx.Tx against the BYPASSRLS sysPool
// via pgcommon.RunInTx. Reconcilers span tenants so they must not go
// through the RLS-scoped app pool.
func runInTxWithSysPool(ctx context.Context, pool *pgcommon.Pool, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, fn)
}
