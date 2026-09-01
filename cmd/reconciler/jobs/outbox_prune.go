package jobs

import (
	"context"
	"time"
)

// OutboxPrune calls platform-events' outbox.Runner.PrunePublished on a
// retention window (OUTBOX_RETENTION_DAYS), removing already-published
// outbox_events rows so the table doesn't grow unbounded (§3.1).
func OutboxPrune(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	if jctx.OutboxRunner == nil {
		jctx.Logger.WarnContext(ctx, "outbox-prune: no outbox.Runner wired — skipping", nil)
		return res, nil
	}
	n, err := jctx.OutboxRunner.PrunePublished(ctx, time.Duration(jctx.OutboxRetentionDays)*24*time.Hour, jctx.BatchLimit)
	if err != nil {
		return res, err
	}
	res.Attempted = int(n)
	res.Succeeded = int(n)
	jctx.Logger.InfoContext(ctx, "outbox-prune complete", "pruned", n)
	return res, nil
}
