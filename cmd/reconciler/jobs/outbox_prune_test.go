package jobs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.Contains(t, logger.warnMsgs[0], "no ProcessedEventsStore wired")
}
