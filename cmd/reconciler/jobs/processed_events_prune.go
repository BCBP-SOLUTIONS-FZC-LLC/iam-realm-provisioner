package jobs

import (
	"context"
)

// defaultPruneBatchLimit is used by ProcessedEventsPrune when
// jctx.BatchLimit is unset (<=0) — matches RECONCILER_BATCH_LIMIT's own
// production default so a caller that never set BatchLimit still gets a
// bounded batch rather than deleting 0 rows (LIMIT 0).
const defaultPruneBatchLimit = 500

// ProcessedEventsPrune deletes processed_events rows older than
// PROCESSED_EVENTS_TTL_DAYS (strictly greater than the 7-day SQS message
// lifetime, §4.5). Batched by jctx.BatchLimit — matching
// iam-org-membership's PruneProcessedEvents — so a backlog larger than one
// tick converges over several ticks instead of one unbounded DELETE.
func ProcessedEventsPrune(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	if jctx.ProcessedEvents == nil {
		jctx.Logger.WarnContext(ctx, "processed-events-prune: no ProcessedEventsStore wired — skipping", nil)
		return res, nil
	}
	batchLimit := jctx.BatchLimit
	if batchLimit <= 0 {
		batchLimit = defaultPruneBatchLimit
	}
	n, err := jctx.ProcessedEvents.Prune(ctx, jctx.ProcessedEventsTTLDays, batchLimit)
	if err != nil {
		return res, err
	}
	res.Attempted = n
	res.Succeeded = n
	jctx.Logger.InfoContext(ctx, "processed-events-prune complete", "pruned", n)
	return res, nil
}
