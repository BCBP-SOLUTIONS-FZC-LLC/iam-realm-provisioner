package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type ctxKey struct{}

// Compensating cleanup must survive the request context being done (30s
// deadline, client disconnect) — otherwise the half-built realm/client it
// exists to remove leaks — while keeping the request's values and staying
// bounded.
func TestCompensationContext_SurvivesCancelledParent_KeepsValues_IsBounded(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "trace"))
	cancelParent()
	require.Error(t, parent.Err())

	ctx, cancel := compensationContext(parent)
	defer cancel()
	assert.NoError(t, ctx.Err(), "cleanup context must not inherit the parent's cancellation")
	assert.Equal(t, "trace", ctx.Value(ctxKey{}), "request-scoped values (trace, identity) are kept")
	deadline, ok := ctx.Deadline()
	require.True(t, ok, "cleanup is bounded")
	assert.WithinDuration(t, time.Now().Add(compensationTimeout), deadline, 2*time.Second)
}
