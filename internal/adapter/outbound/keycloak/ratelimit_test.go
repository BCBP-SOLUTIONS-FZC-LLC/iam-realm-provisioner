package keycloak

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRateLimiter_ThrottlesToConfiguredRPS(t *testing.T) {
	limiter := newRateLimiter(2) // burst 2, refill 2/s
	ctx := context.Background()

	start := time.Now()
	limiter.Wait(ctx)
	limiter.Wait(ctx)
	limiter.Wait(ctx) // exceeds the burst — must wait for a refill
	elapsed := time.Since(start)

	require.GreaterOrEqual(t, elapsed, 400*time.Millisecond, "the third call within one burst window must be throttled")
}

func TestRateLimiter_WaitReturnsOnCanceledContext(t *testing.T) {
	limiter := newRateLimiter(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		limiter.Wait(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return promptly on an already-canceled context")
	}
}
