package keycloak

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

func TestRateLimitTransport_ThrottlesEachRoundTrip(t *testing.T) {
	limiter := newRateLimiter(2) // burst 2, refill 2/s
	var calls int32
	inner := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	rt := rateLimitTransport{limiter: limiter, base: inner}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://unused.invalid", nil)
	require.NoError(t, err)

	start := time.Now()
	for i := 0; i < 3; i++ {
		resp, err := rt.RoundTrip(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 400*time.Millisecond, "the 3rd RoundTrip within one burst window must be throttled")
	assert.EqualValues(t, 3, calls)
}

// CRITICAL (resilience): retryTransport's retries must be throttled too,
// not just each outer Admin method's first attempt — a retry storm against
// an already-struggling Keycloak (5xx/429/timeout, precisely when the rate
// limiter matters most) must stay capped at KEYCLOAK_RATE_LIMIT_RPS rather
// than multiplying it by up to retryMaxAttempts. retryTransport's own
// backoff is set to near-zero here specifically to isolate the rate
// limiter's effect — any observed throttling below can only come from
// rateLimitTransport being consulted on every attempt, not from
// retryTransport's own delay.
func TestRateLimitTransport_ThrottlesRetriesToo(t *testing.T) {
	withFastRetry(t, 4, time.Millisecond, time.Millisecond)
	limiter := newRateLimiter(2) // burst 2, refill 2/s
	h := &countingHandler{statuses: []int{500, 500, 500, 200}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	rt := retryTransport{base: rateLimitTransport{limiter: limiter, base: http.DefaultTransport}}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	start := time.Now()
	resp, err := rt.RoundTrip(req)
	elapsed := time.Since(start)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.EqualValues(t, 4, h.calls)
	assert.GreaterOrEqual(t, elapsed, 400*time.Millisecond,
		"4 attempts through a burst-2 rate limiter must be throttled even with retryTransport's own backoff near-zero")
}
