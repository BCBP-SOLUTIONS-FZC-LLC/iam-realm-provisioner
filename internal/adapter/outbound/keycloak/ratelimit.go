package keycloak

import (
	"context"
	"time"

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

var _ = time.Second
