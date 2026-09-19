package keycloak

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingHandler serves statuses[0], statuses[1], ... in order (repeating
// the last one if called more times than the slice has entries), and
// records every request body it saw.
type countingHandler struct {
	statuses []int
	calls    int32
	bodies   [][]byte
	headers  []http.Header
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := int(atomic.AddInt32(&h.calls, 1)) - 1
	body, _ := io.ReadAll(r.Body)
	h.bodies = append(h.bodies, body)
	h.headers = append(h.headers, r.Header.Clone())
	status := h.statuses[len(h.statuses)-1]
	if n < len(h.statuses) {
		status = h.statuses[n]
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{}`))
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func withFastRetry(t *testing.T, attempts int, base, maxDelay time.Duration) {
	t.Helper()
	origAttempts, origBase, origMax := retryMaxAttempts, retryBaseDelay, retryMaxDelay
	retryMaxAttempts, retryBaseDelay, retryMaxDelay = attempts, base, maxDelay
	t.Cleanup(func() { retryMaxAttempts, retryBaseDelay, retryMaxDelay = origAttempts, origBase, origMax })
}

func TestRetryTransport_SucceedsAfterTransientFiveHundreds(t *testing.T) {
	withFastRetry(t, 4, time.Millisecond, time.Millisecond)
	h := &countingHandler{statuses: []int{500, 500, 200}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	rt := retryTransport{base: http.DefaultTransport}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.EqualValues(t, 3, h.calls, "must retry until the 3rd attempt succeeds")
}

func TestRetryTransport_ExhaustsAttemptsAndReturnsLastResponse(t *testing.T) {
	withFastRetry(t, 3, time.Millisecond, time.Millisecond)
	h := &countingHandler{statuses: []int{500, 500, 500, 500, 500}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	rt := retryTransport{base: http.DefaultTransport}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.EqualValues(t, 3, h.calls, "must stop at retryMaxAttempts, not retry forever")
}

func TestRetryTransport_NonRetryableStatusReturnsImmediately(t *testing.T) {
	withFastRetry(t, 4, time.Millisecond, time.Millisecond)
	for _, status := range []int{400, 401, 404, 409} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := &countingHandler{statuses: []int{status}}
			srv := httptest.NewServer(h)
			defer srv.Close()

			rt := retryTransport{base: http.DefaultTransport}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
			require.NoError(t, err)

			resp, err := rt.RoundTrip(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, status, resp.StatusCode)
			assert.EqualValues(t, 1, h.calls, "a non-retryable status must never trigger a retry")
		})
	}
}

func TestRetryTransport_RetriesOnTransportError(t *testing.T) {
	withFastRetry(t, 3, time.Millisecond, time.Millisecond)
	var calls int32
	failing := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("connection refused")
	})

	rt := retryTransport{base: failing}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://unused.invalid", nil)
	require.NoError(t, err)

	//nolint:bodyclose // failing's RoundTrip always returns (nil, err) — there is no body to close
	_, err = rt.RoundTrip(req)
	require.Error(t, err)
	assert.EqualValues(t, 3, calls, "a transport-level error must retry up to retryMaxAttempts, same as a 5xx")
}

func TestRetryTransport_RebuffersRequestBodyOnEveryAttempt(t *testing.T) {
	withFastRetry(t, 3, time.Millisecond, time.Millisecond)
	h := &countingHandler{statuses: []int{500, 200}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	rt := retryTransport{base: http.DefaultTransport}
	payload := []byte(`{"hello":"world"}`)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, bytes.NewReader(payload))
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Len(t, h.bodies, 2)
	assert.Equal(t, payload, h.bodies[0], "the first attempt must see the full body")
	assert.Equal(t, payload, h.bodies[1], "the retried attempt must see the SAME full body, not an empty/partial one")
}

func TestRetryTransport_ContextCancellationStopsRetryingPromptly(t *testing.T) {
	withFastRetry(t, 5, 50*time.Millisecond, time.Second)
	h := &countingHandler{statuses: []int{500, 500, 500, 500, 500}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	rt := retryTransport{base: http.DefaultTransport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	start := time.Now()
	//nolint:bodyclose // ctx cancellation makes this return (nil, err) — there is no body to close
	_, err = rt.RoundTrip(req)
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, 500*time.Millisecond, "must stop waiting once ctx is done, not run out the full 5-attempt schedule")
}

func TestRetryTransport_429HonoursRetryAfterSeconds(t *testing.T) {
	withFastRetry(t, 3, time.Millisecond, 10*time.Second)
	var calls int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	rt := retryTransport{base: http.DefaultTransport}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.EqualValues(t, 2, calls)
}

func TestRetryDelay_ExponentialGrowthCappedAtMax(t *testing.T) {
	origBase, origMax := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = 100*time.Millisecond, time.Second
	t.Cleanup(func() { retryBaseDelay, retryMaxDelay = origBase, origMax })

	// Full jitter means retryDelay returns a value in [0, backoffCeiling) —
	// assert the ceiling grows exponentially and is capped, by sampling
	// repeatedly and checking the observed max never exceeds each attempt's
	// ceiling (100ms, 200ms, 400ms, ..., capped at 1s).
	ceilings := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, time.Second, time.Second}
	for attempt := 1; attempt <= len(ceilings); attempt++ {
		var max time.Duration
		for i := 0; i < 200; i++ {
			d := retryDelay(attempt, nil)
			require.GreaterOrEqual(t, d, time.Duration(0))
			require.Less(t, d, ceilings[attempt-1]+1, "attempt %d delay must stay within its jitter ceiling", attempt)
			if d > max {
				max = d
			}
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Run("integer seconds", func(t *testing.T) {
		d, ok := parseRetryAfter("5")
		require.True(t, ok)
		assert.Equal(t, 5*time.Second, d)
	})
	t.Run("negative seconds rejected", func(t *testing.T) {
		_, ok := parseRetryAfter("-1")
		assert.False(t, ok)
	})
	t.Run("empty rejected", func(t *testing.T) {
		_, ok := parseRetryAfter("")
		assert.False(t, ok)
	})
	t.Run("garbage rejected", func(t *testing.T) {
		_, ok := parseRetryAfter("not-a-date-or-number")
		assert.False(t, ok)
	})
	t.Run("HTTP-date in the future", func(t *testing.T) {
		when := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
		d, ok := parseRetryAfter(when)
		require.True(t, ok)
		assert.InDelta(t, 3*time.Second, d, float64(2*time.Second))
	})
	t.Run("HTTP-date in the past yields zero not negative", func(t *testing.T) {
		when := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
		d, ok := parseRetryAfter(when)
		require.True(t, ok)
		assert.Equal(t, time.Duration(0), d)
	})
	t.Run("oversized seconds value is clamped, never overflows negative", func(t *testing.T) {
		d, ok := parseRetryAfter("99999999999999999")
		require.True(t, ok)
		assert.Positive(t, d, "an oversized Retry-After must clamp to a large positive duration, not wrap negative")
	})
}

func TestRetryDelay_OversizedRetryAfterClampsToMaxDelay(t *testing.T) {
	origMax := retryMaxDelay
	retryMaxDelay = 10 * time.Second
	t.Cleanup(func() { retryMaxDelay = origMax })

	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"99999999999999999"}}}
	d := retryDelay(1, resp)
	assert.Equal(t, retryMaxDelay, d, "an oversized Retry-After must be capped at retryMaxDelay by the caller, not fire an immediate retry")
}

// An attempt number far beyond what the retry loop itself ever produces
// (retryMaxAttempts is always small) still can't overflow the
// shift-then-convert backoff math — retryDelay is called directly here,
// bypassing the loop, since production never drives attempt this high.
func TestRetryDelay_HugeAttemptClampsShiftAndStaysWithinMaxDelay(t *testing.T) {
	origBase, origMax := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = 100*time.Millisecond, time.Second
	t.Cleanup(func() { retryBaseDelay, retryMaxDelay = origBase, origMax })

	d := retryDelay(1000, nil)
	assert.GreaterOrEqual(t, d, time.Duration(0))
	assert.LessOrEqual(t, d, retryMaxDelay)
}

// erroringBody is a hand-rolled request body for exercising RoundTrip's own
// buffering step directly (bypassing the fake-admin/gocloak plumbing,
// which never hands RoundTrip a body that fails to read or close).
type erroringBody struct {
	readErr  error
	closeErr error
}

func (b erroringBody) Read(p []byte) (int, error) {
	if b.readErr != nil {
		return 0, b.readErr
	}
	return 0, io.EOF
}
func (b erroringBody) Close() error { return b.closeErr }

func TestRetryTransport_BodyReadErrorPropagatesWithoutCallingBase(t *testing.T) {
	rt := retryTransport{base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("must not reach the base transport when buffering the body fails")
		return nil, nil
	})}
	req, err := http.NewRequest(http.MethodPost, "http://example.com", nil)
	require.NoError(t, err)
	req.Body = erroringBody{readErr: errors.New("read boom")}

	_, err = rt.RoundTrip(req) //nolint:bodyclose // buffering the request body fails before any response exists — RoundTrip returns (nil, err), nothing to close
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read boom")
}

func TestRetryTransport_BodyCloseErrorPropagatesWhenReadSucceeds(t *testing.T) {
	rt := retryTransport{base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("must not reach the base transport when closing the body fails")
		return nil, nil
	})}
	req, err := http.NewRequest(http.MethodPost, "http://example.com", nil)
	require.NoError(t, err)
	req.Body = erroringBody{closeErr: errors.New("close boom")}

	_, err = rt.RoundTrip(req) //nolint:bodyclose // buffering the request body fails before any response exists — RoundTrip returns (nil, err), nothing to close
	require.Error(t, err)
	assert.Contains(t, err.Error(), "close boom")
}
