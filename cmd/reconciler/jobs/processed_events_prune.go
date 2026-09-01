package jobs

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ProcessedEventsPrune deletes processed_events rows older than
// PROCESSED_EVENTS_TTL_DAYS (strictly greater than the 7-day SQS message
// lifetime, §4.5).
func ProcessedEventsPrune(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	var n int64
	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM processed_events WHERE processed_at < now() - ($1::int * interval '1 day')`,
			jctx.ProcessedEventsTTLDays)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return res, err
	}
	res.Attempted = int(n)
	res.Succeeded = int(n)
	jctx.Logger.InfoContext(ctx, "processed-events-prune complete", "pruned", n)
	return res, nil
}
