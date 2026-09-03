package keycloak

import (
	"context"
	"net/http"

	"golang.org/x/time/rate"
)

// rateLimiter throttles outbound Keycloak Admin API calls (§9.4, RP-D9) so
// a burst — e.g. migrating dozens of users at conversion — never exceeds
// KEYCLOAK_RATE_LIMIT_RPS.
type rateLimiter struct {
	limiter *rate.Limiter
}

func newRateLimiter(rps int) *rateLimiter {
	return &rateLimiter{limiter: rate.NewLimiter(rate.Limit(rps), rps)}
}

// Wait blocks until a token is available or ctx is done. Errors (context
// cancellation) are deliberately swallowed here — the subsequent gocloak
// call will itself fail against the canceled context with a clear error.
func (r *rateLimiter) Wait(ctx context.Context) {
	_ = r.limiter.Wait(ctx) //nolint:errcheck // deliberately swallowed, see doc comment above
}

// rateLimitTransport throttles every actual outbound HTTP request to
// KEYCLOAK_RATE_LIMIT_RPS — including each of retryTransport's retries,
// not just the first attempt. It sits as retryTransport's base in admin.go's
// transport chain specifically so a retry storm against an already-struggling
// Keycloak (5xx/429/timeout — precisely when the rate limiter matters most)
// stays capped at the configured rate rather than multiplying it by up to
// retryMaxAttempts. A single call to Admin.token gated the limiter once per
// outer method call before this existed, which both under-throttled retries
// and over-throttled a token-cache hit that made no network call at all;
// this replaces that call site entirely.
type rateLimitTransport struct {
	limiter *rateLimiter
	base    http.RoundTripper
}

func (t rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.limiter.Wait(req.Context())
	return t.base.RoundTrip(req)
}
