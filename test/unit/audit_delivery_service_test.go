package unit_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

func pendingEntries(n int) []*domain.PendingAuditEntry {
	out := make([]*domain.PendingAuditEntry, n)
	for i := range out {
		out[i] = &domain.PendingAuditEntry{ID: uuid.New(), TenantID: uuid.New(), Attempts: 1,
			Entry: domain.AuditEntry{EntryType: domain.AuditEntryTypeIdpChanged}}
	}
	return out
}

func TestAuditDelivery_RoutesEachOutcome(t *testing.T) {
	due := pendingEntries(3)
	repo := &mockPendingAuditEntryRepository{Due: due}
	client := &mockAuditLogClient{Errs: []error{
		nil,
		fmt.Errorf("%w: 422", port.ErrAuditEntryRejected),
		errors.New("connection refused"),
	}}
	svc := service.NewAuditDeliveryService(repo, client)

	n, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []uuid.UUID{due[0].ID}, repo.Delivered)
	assert.Equal(t, []uuid.UUID{due[1].ID}, repo.Failed)
	assert.Equal(t, []uuid.UUID{due[2].ID}, repo.Retried)
}

// The row id is the AL-5 Idempotency-Key, so a redelivery after a lost
// response is deduplicated by Audit Log.
func TestAuditDelivery_UsesRowIDAsIdempotencyKey(t *testing.T) {
	due := pendingEntries(2)
	client := &mockAuditLogClient{}
	svc := service.NewAuditDeliveryService(&mockPendingAuditEntryRepository{Due: due}, client)

	_, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, []string{due[0].ID.String(), due[1].ID.String()}, client.Keys)
}

func TestAuditDelivery_ClaimFailurePropagates(t *testing.T) {
	claimErr := errors.New("db down")
	svc := service.NewAuditDeliveryService(&mockPendingAuditEntryRepository{ClaimErr: claimErr}, &mockAuditLogClient{})
	_, err := svc.DrainOnce(context.Background(), 10)
	require.ErrorIs(t, err, claimErr)
}

func TestAuditDelivery_MarkFailuresAreLoggedNotPropagated(t *testing.T) {
	repo := &mockPendingAuditEntryRepository{Due: pendingEntries(3), MarkErr: errors.New("db down")}
	client := &mockAuditLogClient{Errs: []error{nil, port.ErrAuditEntryRejected, errors.New("timeout")}}
	svc := service.NewAuditDeliveryService(repo, client).WithLogger(&mockLogger{})

	n, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// A row whose Attempts exceeds the max-attempts cap is parked as failed
// without attempting delivery — bounds the blast radius of a misbehaving
// AL-5 stuck at Retry-After: 1. ClaimDue post-increments before returning,
// so parking fires at Attempts = 51 (after 50 actual delivery attempts).
func TestAuditDelivery_ExhaustedRetryBudget_ParksAsFailed(t *testing.T) {
	due := pendingEntries(1)
	due[0].Attempts = 51 // one past max; ClaimDue post-increments, so 50 actual deliveries were made
	repo := &mockPendingAuditEntryRepository{Due: due}
	client := &mockAuditLogClient{Errs: []error{errors.New("timeout")}}
	svc := service.NewAuditDeliveryService(repo, client).WithLogger(&mockLogger{})

	_, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{due[0].ID}, repo.Failed, "row must park as failed once retry budget is exhausted")
	assert.Empty(t, repo.Retried, "MarkRetry must not fire past the cap")
	assert.Empty(t, repo.RetriedAfter, "MarkRetryAfter must not fire past the cap")
}

// A *port.RetryAfterError routes through MarkRetryAfter with the AL-5
// upstream hint rather than the default MarkRetry backoff.
func TestAuditDelivery_RetryAfterErrorRoutesThroughMarkRetryAfter(t *testing.T) {
	due := pendingEntries(1)
	repo := &mockPendingAuditEntryRepository{Due: due}
	base := errors.New("auditlog: AL-5 unavailable (status 429)")
	client := &mockAuditLogClient{Errs: []error{&port.RetryAfterError{Underlying: base, RetryAfter: 42 * time.Second}}}
	svc := service.NewAuditDeliveryService(repo, client).WithLogger(&mockLogger{})

	_, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{due[0].ID}, repo.RetriedAfter, "row must route through MarkRetryAfter")
	assert.Equal(t, []time.Duration{42 * time.Second}, repo.RetryAfterHint)
	assert.Empty(t, repo.Retried, "MarkRetry must not be called when a Retry-After hint is present")
}

func TestAuditDelivery_EmptyBacklog(t *testing.T) {
	client := &mockAuditLogClient{}
	svc := service.NewAuditDeliveryService(&mockPendingAuditEntryRepository{}, client)
	n, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, client.Keys)
}

// A row with a deterministic key (TermsAccepted) is delivered under that
// key, not its row id, so Audit Log dedupes a resumed signup's entry.
func TestAuditDelivery_UsesDeterministicKeyWhenSet(t *testing.T) {
	due := pendingEntries(1)
	due[0].IdempotencyKey = "TermsAccepted:t:u:2026-09"
	client := &mockAuditLogClient{}
	repo := &mockPendingAuditEntryRepository{Due: due}
	svc := service.NewAuditDeliveryService(repo, client)

	n, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{"TermsAccepted:t:u:2026-09"}, client.Keys)
	assert.Equal(t, []uuid.UUID{due[0].ID}, repo.Delivered, "a delivered row is deleted — no IP/user agent lingers here")
}

// When the client returns ErrAuditEntryTypeUnknown AND MarkRetry itself
// fails, the MarkRetry error must be LOGGED but NOT propagated — DrainOnce
// still returns (0, nil). An AL-5 deploy-skew condition that also has a
// DB hiccup on the retry-mark must not break the drainer loop or surface
// the mark error to the caller.
func TestAuditDelivery_UnknownEntryType_MarkRetryFails_ErrorLoggedNotPropagated(t *testing.T) {
	due := pendingEntries(1)
	repo := &mockPendingAuditEntryRepository{
		Due:     due,
		MarkErr: errors.New("db down"), // MarkRetry will return this error
	}
	unknown := fmt.Errorf("%w: AL-5 status 422", port.ErrAuditEntryTypeUnknown)
	client := &mockAuditLogClient{Errs: []error{unknown}}
	logger := &mockLogger{}
	svc := service.NewAuditDeliveryService(repo, client).WithLogger(logger)

	n, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err, "DrainOnce must not propagate the MarkRetry error")
	assert.Zero(t, n, "a row in the ErrAuditEntryTypeUnknown path is not counted as delivered")
	// MarkRetry was attempted (the ID is recorded) but failed.
	assert.Equal(t, []uuid.UUID{due[0].ID}, repo.Retried,
		"MarkRetry must still be called even though it will fail")
	assert.Empty(t, repo.Failed, "a type-unknown row must never be parked as failed")
	assert.Empty(t, repo.Delivered)
	// The MarkRetry failure must be logged as a warning, not swallowed silently.
	assert.GreaterOrEqual(t, logger.warnCalls, 1, "the MarkRetry failure must be logged")
}

// 422 UNKNOWN_ENTRY_TYPE (Audit Log not yet upgraded to know
// legal.terms.accepted) keeps the row pending and retries it — never
// parked, never dropped, even past the transient retry budget.
func TestAuditDelivery_UnknownEntryType_KeptAndRetried(t *testing.T) {
	due := pendingEntries(2)
	due[1].Attempts = 500 // far past the budget: still retried
	repo := &mockPendingAuditEntryRepository{Due: due}
	unknown := fmt.Errorf("%w: AL-5 status 422", port.ErrAuditEntryTypeUnknown)
	client := &mockAuditLogClient{Errs: []error{unknown, unknown}}
	svc := service.NewAuditDeliveryService(repo, client).WithLogger(&mockLogger{})
	before := testutil.ToFloat64(metrics.AuditEntryDeliveryTotal.WithLabelValues("unknown_entry_type"))

	n, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, []uuid.UUID{due[0].ID, due[1].ID}, repo.Retried)
	assert.Empty(t, repo.Failed, "an unknown entry type must never be parked as failed")
	assert.Empty(t, repo.Delivered)
	assert.Equal(t, before+2, testutil.ToFloat64(metrics.AuditEntryDeliveryTotal.WithLabelValues("unknown_entry_type")))
}

// When the retry budget is exhausted AND MarkFailed itself fails, the
// MarkFailed error must be LOGGED but NOT propagated — DrainOnce returns nil.
func TestAuditDelivery_ExhaustedBudget_MarkFailedFails_ErrorLoggedNotPropagated(t *testing.T) {
	due := pendingEntries(1)
	due[0].Attempts = 51 // past max (50)
	repo := &mockPendingAuditEntryRepository{
		Due:     due,
		MarkErr: errors.New("db down"),
	}
	client := &mockAuditLogClient{Errs: []error{errors.New("timeout")}}
	logger := &mockLogger{}
	svc := service.NewAuditDeliveryService(repo, client).WithLogger(logger)

	n, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err, "MarkFailed failure must not propagate from DrainOnce")
	assert.Zero(t, n)
	assert.Equal(t, []uuid.UUID{due[0].ID}, repo.Failed, "MarkFailed must still be attempted")
	assert.GreaterOrEqual(t, logger.warnCalls, 1, "the MarkFailed failure must be logged as a warning")
}

// A *port.RetryAfterError whose MarkRetryAfter call also fails must be
// LOGGED but NOT propagated — the row will be retried on the next drain
// tick via ordinary backoff.
func TestAuditDelivery_RetryAfter_MarkRetryAfterFails_ErrorLoggedNotPropagated(t *testing.T) {
	due := pendingEntries(1)
	repo := &mockPendingAuditEntryRepository{
		Due:     due,
		MarkErr: errors.New("db down"),
	}
	base := errors.New("auditlog: 429 too many requests")
	client := &mockAuditLogClient{Errs: []error{&port.RetryAfterError{Underlying: base, RetryAfter: 5 * time.Second}}}
	logger := &mockLogger{}
	svc := service.NewAuditDeliveryService(repo, client).WithLogger(logger)

	n, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err, "MarkRetryAfter failure must not propagate from DrainOnce")
	assert.Zero(t, n)
	assert.Equal(t, []uuid.UUID{due[0].ID}, repo.RetriedAfter, "MarkRetryAfter must still be attempted")
	assert.GreaterOrEqual(t, logger.warnCalls, 1, "the MarkRetryAfter failure must be logged as a warning")
}
