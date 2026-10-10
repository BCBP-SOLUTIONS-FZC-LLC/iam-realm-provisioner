package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// captureIdempotencyStore records calls to ExpireStaleInProgress so tests
// can assert on the TTL and limit passed by IdempotencyCleanup.
type captureIdempotencyStore struct {
	mockIdempotencyStore // embed no-op for Begin/Complete/Fail

	capturedTTL   time.Duration
	capturedLimit int
	returnN       int
	returnErr     error
}

func (s *captureIdempotencyStore) ExpireStaleInProgress(_ context.Context, olderThan time.Duration, limit int) (int, error) {
	s.capturedTTL = olderThan
	s.capturedLimit = limit
	return s.returnN, s.returnErr
}

func newCleanupContext(idem port.IdempotencyStore) *Context {
	tx, _ := newMockTxRunner()
	logger := &mockLogger{}
	return &Context{
		TxRunner:           tx,
		Idempotency:        idem,
		Logger:             port.NewSlogStyleLogger(logger),
		BatchLimit:         50,
		InProgressTTLHours: 2,
	}
}

func TestIdempotencyCleanup_PassesTTLAndLimitToStore(t *testing.T) {
	store := &captureIdempotencyStore{
		mockIdempotencyStore: *newMockIdempotencyStore(),
		returnN:              3,
	}
	jctx := newCleanupContext(store)
	jctx.InProgressTTLHours = 3
	jctx.BatchLimit = 25

	res, err := IdempotencyCleanup(context.Background(), jctx)

	require.NoError(t, err)
	assert.Equal(t, 3*time.Hour, store.capturedTTL, "TTL must derive from InProgressTTLHours")
	assert.Equal(t, 25, store.capturedLimit, "batch limit must match jctx.BatchLimit")
	assert.Equal(t, 3, res.Attempted)
	assert.Equal(t, 3, res.Succeeded)
}

func TestIdempotencyCleanup_DefaultTTLWhenZero(t *testing.T) {
	store := &captureIdempotencyStore{mockIdempotencyStore: *newMockIdempotencyStore()}
	jctx := newCleanupContext(store)
	jctx.InProgressTTLHours = 0 // unset — must default to 1 hour

	_, err := IdempotencyCleanup(context.Background(), jctx)

	require.NoError(t, err)
	assert.Equal(t, time.Hour, store.capturedTTL, "zero InProgressTTLHours must default to 1 hour")
}

func TestIdempotencyCleanup_LoopsUntilBatchSmaller(t *testing.T) {
	// First call returns a full batch (limit=2), second returns 1 → loop stops.
	calls := 0
	jctx := newCleanupContext(&multiCallStore{
		results: []int{2, 1}, // call 1 → 2 (full batch), call 2 → 1 (stop)
		calls:   &calls,
	})
	jctx.BatchLimit = 2

	res, err := IdempotencyCleanup(context.Background(), jctx)

	require.NoError(t, err)
	assert.Equal(t, 2, calls, "must loop exactly twice — once for the full batch, once for the remainder")
	assert.Equal(t, 3, res.Attempted, "total = 2 + 1")
	assert.Equal(t, 3, res.Succeeded)
}

func TestIdempotencyCleanup_PropagatesError(t *testing.T) {
	boom := errors.New("db unavailable")
	store := &captureIdempotencyStore{
		mockIdempotencyStore: *newMockIdempotencyStore(),
		returnN:              5,
		returnErr:            boom,
	}
	jctx := newCleanupContext(store)

	res, err := IdempotencyCleanup(context.Background(), jctx)

	require.ErrorIs(t, err, boom)
	// Partial count (5) must be preserved even on error so the caller's log
	// shows actual progress rather than 0.
	assert.Equal(t, 5, res.Attempted)
	assert.Equal(t, 5, res.Succeeded)
}

// multiCallStore returns successive values from results on each
// ExpireStaleInProgress call, enabling loop-convergence tests.
type multiCallStore struct {
	mockIdempotencyStore
	results []int
	calls   *int
}

func (s *multiCallStore) ExpireStaleInProgress(_ context.Context, _ time.Duration, _ int) (int, error) {
	i := *s.calls
	*s.calls++
	if i >= len(s.results) {
		return 0, nil
	}
	return s.results[i], nil
}
