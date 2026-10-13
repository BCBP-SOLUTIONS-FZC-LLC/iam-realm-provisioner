package keycloak

import (
	"bytes"
	"context"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// retryMaxAttempts is the total number of tries (the first attempt plus up
// to retryMaxAttempts-1 retries). A package-level var, not a const, so
// tests can shrink it and the two delays below to keep the (many) existing
// fake-admin-backed tests that simulate a 5xx/timeout fast — see
// fake_admin_test.go's TestMain, which does exactly that for the whole
// package's test binary. Production always uses these defaults; nothing
// in application code ever reassigns them.
var (
	retryMaxAttempts = 4
	retryBaseDelay   = 200 * time.Millisecond
	retryMaxDelay    = 5 * time.Second
)

// retryTransport retries a request on a 5xx/429 response or a network-level
// error, with exponential backoff and full jitter, honoring a 429's
// Retry-After header when present (§9.4). It sits as the outermost layer in
// admin.go's transport chain, so every retried attempt still gets its own
// trace span (httpx) and 401-driven token invalidation
// (unauthorizedTransport) underneath it.
//
// A blind retry is safe here because every mutating call this adapter makes
// already has a natural conflict-detector for the "request actually
// succeeded, but the response was lost" case: CreateUser is rejected by
// Keycloak's own username/email uniqueness constraint, partialImportUser
// sets ifResourceExists=FAIL, and CreateRealm's 409 is already mapped to
// ErrRealmAlreadyDedicated. A retry that lands on an already-applied
// mutation surfaces as one of those existing conflict errors, never as a
// silent duplicate.
type retryTransport struct {
	base http.RoundTripper
}

func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// RoundTrip implements http.RoundTripper.
func (t retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer the body once so every attempt gets an identical, fully
	// readable copy — resty/gocloak requests are small JSON payloads, so
	// buffering costs nothing meaningful, and it sidesteps having to trust
	// that req.GetBody was populated correctly by every caller.
	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if closeErr := req.Body.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
		bodyBytes = b
	}

	var resp *http.Response
	var err error
	for attempt := 0; attempt < retryMaxAttempts; attempt++ {
		if attempt > 0 {
			if werr := waitForRetry(req.Context(), retryDelay(attempt, resp)); werr != nil {
				return nil, werr
			}
		}
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			req.ContentLength = int64(len(bodyBytes))
		}

		resp, err = t.base.RoundTrip(req)
		if err != nil {
			if attempt == retryMaxAttempts-1 || req.Context().Err() != nil {
				return nil, err
			}
			continue
		}
		if !isRetryableStatus(resp.StatusCode) || attempt == retryMaxAttempts-1 {
			return resp, nil
		}
		// Drain and close this attempt's response before retrying, so the
		// connection can be reused instead of leaking. No logger is
		// available at this layer and a drain/close failure here changes
		// nothing about the retry decision already made above.
		//nolint:errcheck,gosec // best-effort connection-reuse hygiene, see comment above
		io.Copy(io.Discard, resp.Body)
		//nolint:errcheck,gosec // same as above
		resp.Body.Close()
	}
	return resp, err
}

// waitForRetry sleeps for delay, or returns ctx's error immediately if ctx
// is done first — a retry must never fire after the caller has given up.
func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryDelay computes the wait before the given attempt (1-indexed: attempt
// 1 is the delay before the first retry). A 429 with a valid Retry-After
// wins outright, capped at retryMaxDelay so a misbehaving/huge
// server-supplied value can't stall a caller far past its own timeout.
// Otherwise: exponential backoff (retryBaseDelay * 2^(attempt-1), capped at
// retryMaxDelay) with full jitter, so many concurrent retrying clients
// don't all retry in lockstep.
func retryDelay(attempt int, resp *http.Response) time.Duration {
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			if d > retryMaxDelay {
				return retryMaxDelay
			}
			return d
		}
	}
	// Clamp shift: 2^32 * retryBaseDelay already vastly exceeds any sane
	// retryMaxDelay, so min keeps the shift-then-convert below well
	// clear of int64 overflow regardless of how large attempt gets.
	shift := min(attempt-1, 32)
	//nolint:gosec // G115: shift is clamped just above, so this uint->int64 conversion cannot overflow
	backoff := retryBaseDelay * time.Duration(uint(1)<<uint(shift))
	if backoff > retryMaxDelay || backoff <= 0 {
		backoff = retryMaxDelay
	}
	//nolint:gosec // G404: math/rand/v2 is intentional here — jitter for retry-backoff timing is not security-sensitive, and crypto/rand would be wasteful
	return time.Duration(rand.Int64N(int64(backoff)))
}

// maxRetryAfterSeconds bounds parseRetryAfter's integer-seconds form so the
// `* time.Second` conversion below can never overflow int64 (time.Duration
// is nanoseconds; ~292 years' worth of seconds is the real ceiling) — an
// oversized or hostile Retry-After value is clamped here rather than
// silently wrapping into a negative duration, which would fire an
// immediate retry instead of the backoff the header was asking for.
// Shares the same RFC 9110 §7.1.3 contract as httpx.ParseRetryAfter; the
// caller (retryDelay) applies its own retryMaxDelay cap on the result.
const maxRetryAfterSeconds = int64(math.MaxInt64) / int64(time.Second)

// parseRetryAfter accepts both forms RFC 9110 allows: an integer number of
// seconds, or an HTTP-date. A date already in the past yields a zero delay
// (retry immediately) rather than a negative one. Returns (0, false) when the
// header is absent or unparseable so the caller falls back to backoff.
func parseRetryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		if int64(secs) > maxRetryAfterSeconds {
			secs = int(maxRetryAfterSeconds)
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(v); err == nil {
		return max(time.Until(when), 0), true
	}
	return 0, false
}
