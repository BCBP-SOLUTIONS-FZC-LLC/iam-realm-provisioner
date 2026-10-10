package jobs

import (
	"context"
	"time"
)

// IdempotencyCleanup expires `in_progress` provisioning_operations rows that
// have been stuck longer than InProgressTTL. A row stays in_progress when an
// operation's database transaction rolls back (e.g. SQLSTATE 57014 statement
// timeout) before the error handler's Fail call can run. Without this job,
// callers that retry with the same Idempotency-Key receive 409
// operation_in_progress forever instead of being able to re-execute.
//
// The transition is to `failed` so the next Begin() reclaims the key (the
// reclaim-on-failed logic in IdempotencyRepository.Begin) and re-runs the
// operation — the same guarantee Fail provides during normal error handling.
//
// Run daily (e.g. 0 5 * * *). InProgressTTL defaults to 1 hour; any row
// in_progress longer than that is unambiguously an abandoned transaction
// (HTTP+Keycloak timeouts are at most 35s+8s).
func IdempotencyCleanup(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	ttl := time.Duration(jctx.InProgressTTLHours) * time.Hour
	if ttl <= 0 {
		ttl = time.Hour
	}
	batchLimit := jctx.BatchLimit
	if batchLimit <= 0 {
		batchLimit = defaultPruneBatchLimit
	}

	for {
		n, err := jctx.Idempotency.ExpireStaleInProgress(ctx, ttl, batchLimit)
		res.Attempted += n
		res.Succeeded += n
		if err != nil {
			return res, err
		}
		if n < batchLimit {
			break
		}
	}
	jctx.Logger.InfoContext(ctx, "idempotency-cleanup complete", "expired", res.Succeeded, "ttl_hours", int(ttl.Hours()))
	return res, nil
}
