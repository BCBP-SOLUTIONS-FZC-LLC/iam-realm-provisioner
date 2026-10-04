package service

import (
	"context"
	"time"
)

// compensationTimeout bounds one compensating cleanup call.
const compensationTimeout = 30 * time.Second

// compensationContext returns a context for compensating cleanup (§9.3):
// it keeps ctx's values (trace, request identity) but not its
// cancellation, bounded by compensationTimeout. Compensation runs exactly
// when the request context is most likely done — the 30s request deadline
// fired mid-sequence, or the client disconnected — and cleanup on a
// cancelled context would fail immediately, leaking the half-built realm,
// client or secret it exists to remove.
func compensationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), compensationTimeout)
}
