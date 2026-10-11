package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// OutboxPrune wraps platform-events' outbox.Runner.PrunePublished, a
// concrete type this repo does not own — only the nil-OutboxRunner
// skip-and-WARN branch is this repo's own code to test.
func TestOutboxPrune_NilRunner_SkipsAndWarns(t *testing.T) {
	jctx, _, _, _, _, logger := newTestContext()
	jctx.OutboxRunner = nil

	res, err := OutboxPrune(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{}, res)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
	require.NotEmpty(t, logger.warnMsgs)
	assert.Contains(t, logger.warnMsgs[0], "no outbox.Runner wired")
}

func TestProcessedEventsPrune_NilStore_SkipsAndWarns(t *testing.T) {
	jctx, _, _, _, _, logger := newTestContext()
	jctx.ProcessedEvents = nil

	res, err := ProcessedEventsPrune(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{}, res)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
	require.NotEmpty(t, logger.warnMsgs)
	assert.Contains(t, logger.warnMsgs[0], "no inbox pruner wired")
}

// The repository-level Prune failure propagates unwrapped — this job adds
// no compensating behavior of its own on that path.
func TestProcessedEventsPrune_StorePruneFails_PropagatesError(t *testing.T) {
	jctx, _, _, _, _, _ := newTestContext()
	pruneErr := errors.New("db unavailable")
	jctx.ProcessedEvents = &mockInboxPruner{
		PruneFn: func(context.Context, domain.ProcessedEventsConsumer, time.Duration, int) (int64, error) {
			return 0, pruneErr
		},
	}

	_, err := ProcessedEventsPrune(context.Background(), jctx)
	require.Same(t, pruneErr, err)
}

// BatchLimit <= 0 falls back to defaultPruneBatchLimit rather than passing
// a non-positive limit straight through to the repository (which would
// delete 0 rows for LIMIT 0, or reject a negative one).
func TestProcessedEventsPrune_BatchLimitUnset_UsesDefault(t *testing.T) {
	jctx, _, _, _, _, _ := newTestContext()
	jctx.BatchLimit = 0
	jctx.ProcessedEventsTTLDays = 8
	var gotLimit int
	var gotRetention time.Duration
	var consumers []domain.ProcessedEventsConsumer
	jctx.ProcessedEvents = &mockInboxPruner{
		PruneFn: func(_ context.Context, consumer domain.ProcessedEventsConsumer, retention time.Duration, batch int) (int64, error) {
			gotLimit, gotRetention = batch, retention
			consumers = append(consumers, consumer)
			return 1, nil
		},
	}

	res, err := ProcessedEventsPrune(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, defaultPruneBatchLimit, gotLimit)
	assert.Equal(t, 8*24*time.Hour, gotRetention)
	assert.ElementsMatch(t, []domain.ProcessedEventsConsumer{domain.ConsumerMembership, domain.ConsumerTenant, domain.ConsumerBilling}, consumers,
		"every consumer's inbox store is pruned")
	assert.Equal(t, 3, res.Attempted)
	assert.Equal(t, 3, res.Succeeded)
}
