package jobs

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// sysWriteRetryOpts matches postgres.writeRetryOpts — deadlock (40P01) and
// serialization failure (40001) retry with exponential backoff + jitter.
var sysWriteRetryOpts = pgcommon.RetryOptions{
	MaxAttempts:    3,
	InitialWait:    10 * time.Millisecond,
	MaxWait:        500 * time.Millisecond,
	Multiplier:     2.0,
	JitterFraction: 0.25,
}

// runInTxWithSysPool runs fn inside a pgx.Tx against the BYPASSRLS sysPool
// via pgcommon.RunInTxWithRetryOpts. Reconcilers span tenants so they must
// not go through the RLS-scoped app pool.
func runInTxWithSysPool(ctx context.Context, pool *pgcommon.Pool, fn func(ctx context.Context, tx pgcommon.Tx) error) error {
	return pgcommon.RunInTxWithRetryOpts(ctx, pool, pgcommon.TxOptions{}, sysWriteRetryOpts, fn)
}
