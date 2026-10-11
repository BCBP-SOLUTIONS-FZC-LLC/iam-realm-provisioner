package jobs

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// defaultPruneBatchLimit is used by ProcessedEventsPrune when
// jctx.BatchLimit is unset (<=0) — matches RECONCILER_BATCH_LIMIT's own
// production default so a caller that never set BatchLimit still gets a
// bounded batch.
const defaultPruneBatchLimit = 500

// pruneConsumers is every processed_events.consumer value (§25) — one
// platform-events inbox.Store per inbound queue.
var pruneConsumers = []domain.ProcessedEventsConsumer{
	domain.ConsumerMembership, domain.ConsumerTenant, domain.ConsumerBilling,
}

// ProcessedEventsPrune deletes processed_events rows older than
// PROCESSED_EVENTS_TTL_DAYS (strictly greater than the 7-day SQS message
// lifetime, §4.5) for every consumer, through platform-events'
// inbox.Store.Prune — matching iam-org-membership. Prune loops
// jctx.BatchLimit-sized deletes until no expired row is left, so a backlog
// never holds one long transaction.
func ProcessedEventsPrune(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	if jctx.ProcessedEvents == nil {
		jctx.Logger.WarnContext(ctx, "processed-events-prune: no inbox pruner wired — skipping", nil)
		return res, nil
	}
	batchLimit := jctx.BatchLimit
	if batchLimit <= 0 {
		batchLimit = defaultPruneBatchLimit
	}
	retention := time.Duration(jctx.ProcessedEventsTTLDays) * 24 * time.Hour
	for _, consumer := range pruneConsumers {
		n, err := jctx.ProcessedEvents.Prune(ctx, consumer, retention, batchLimit)
		res.Attempted += int(n)
		res.Succeeded += int(n)
		if err != nil {
			return res, err
		}
	}
	jctx.Logger.InfoContext(ctx, "processed-events-prune complete", "pruned", res.Succeeded)
	return res, nil
}
