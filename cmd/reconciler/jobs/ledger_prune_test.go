package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLedgerPrune_PrunesBothTablesWithTheirOwnRetention(t *testing.T) {
	jctx, _, _, _, _, _ := newTestContext()
	jctx.ProvisioningOperationsTTLDays = 30
	jctx.RLSViolationLogTTLDays = 90
	jctx.BatchLimit = 500
	var opsAge, rlsAge time.Duration
	jctx.Retention = &mockRetentionPruner{
		OpsFn: func(_ context.Context, olderThan time.Duration, batch int) (int64, error) {
			opsAge = olderThan
			assert.Equal(t, 500, batch)
			return 4, nil
		},
		RLSFn: func(_ context.Context, olderThan time.Duration, _ int) (int64, error) {
			rlsAge = olderThan
			return 2, nil
		},
	}

	res, err := LedgerPrune(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, 30*24*time.Hour, opsAge)
	assert.Equal(t, 90*24*time.Hour, rlsAge)
	assert.Equal(t, Result{Attempted: 6, Succeeded: 6}, res)
}

func TestLedgerPrune_FirstFailureStopsAndPropagates(t *testing.T) {
	jctx, _, _, _, _, _ := newTestContext()
	jctx.ProvisioningOperationsTTLDays, jctx.RLSViolationLogTTLDays = 30, 90
	boom := errors.New("db down")
	rlsCalled := false
	jctx.Retention = &mockRetentionPruner{
		OpsFn: func(context.Context, time.Duration, int) (int64, error) { return 0, boom },
		RLSFn: func(context.Context, time.Duration, int) (int64, error) { rlsCalled = true; return 0, nil },
	}
	_, err := LedgerPrune(context.Background(), jctx)
	require.ErrorIs(t, err, boom)
	assert.False(t, rlsCalled)
}

func TestLedgerPrune_NilPruner_SkipsAndWarns(t *testing.T) {
	jctx, _, _, _, _, logger := newTestContext()
	jctx.Retention = nil
	res, err := LedgerPrune(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{}, res)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// A failure on the second table still reports what the first one pruned.
func TestLedgerPrune_RLSLogFailure_ReportsOpsPrunedAndPropagates(t *testing.T) {
	jctx, _, _, _, _, _ := newTestContext()
	jctx.ProvisioningOperationsTTLDays, jctx.RLSViolationLogTTLDays = 30, 90
	boom := errors.New("permission denied for table rls_violation_log")
	jctx.Retention = &mockRetentionPruner{
		OpsFn: func(context.Context, time.Duration, int) (int64, error) { return 3, nil },
		RLSFn: func(context.Context, time.Duration, int) (int64, error) { return 0, boom },
	}
	res, err := LedgerPrune(context.Background(), jctx)
	require.ErrorIs(t, err, boom)
	assert.Equal(t, Result{Attempted: 3, Succeeded: 3}, res)
}
