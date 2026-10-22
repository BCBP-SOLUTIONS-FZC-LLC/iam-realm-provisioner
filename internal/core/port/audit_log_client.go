package port

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// ErrAuditEntryRejected marks a permanent AL-5 rejection (a 4xx other than
// 429 — e.g. 422 unknown_entry_type / metadata_too_large). Retrying the
// same body can never succeed, so the drainer parks the row as 'failed'
// instead of retrying it. Every other WriteEntry error is transient.
var ErrAuditEntryRejected = errors.New("audit entry rejected by audit log")

// ErrAuditEntryTypeUnknown marks AL-5's 422 UNKNOWN_ENTRY_TYPE: this
// service knows an entry type the deployed Audit Log does not yet (deploy
// skew — e.g. legal.terms.accepted ahead of Audit Log rev 0.31). It is
// deliberately NOT ErrAuditEntryRejected: the row is kept pending and
// retried on its backoff, with no retry budget, until Audit Log learns the
// type — never parked or dropped.
var ErrAuditEntryTypeUnknown = errors.New("audit log does not know this entry type yet")

// RetryAfterError wraps a transient AL-5 failure whose 429 response
// carried a parseable Retry-After header (RFC 7231 §7.1.3). RetryAfter is
// the hint's delay; AuditDeliveryService uses it to override the row's
// default exponential backoff via MarkRetryAfter.
type RetryAfterError struct {
	Underlying error
	RetryAfter time.Duration
}

func (e *RetryAfterError) Error() string { return e.Underlying.Error() }
func (e *RetryAfterError) Unwrap() error { return e.Underlying }

// AuditLogClient is this service's outbound dependency on the Audit Log
// Service's AL-5 direct-write ingest (POST /api/v1/internal/audit-entries,
// HLD §9.4) — the transport for the non-bus TenantIdpConfigChanged entry.
type AuditLogClient interface {
	// WriteEntry POSTs entry under idempotencyKey. A replay of an
	// already-persisted key is success (AL-5 answers 200, not 409).
	WriteEntry(ctx context.Context, idempotencyKey string, entry domain.AuditEntry) error
}

// PendingAuditEntryRepository persists `pending_audit_entries` (§4.4) —
// the durable hand-off between an IdP-config commit and AL-5 delivery.
type PendingAuditEntryRepository interface {
	// Enqueue records entry as pending. Called inside the same RunInTx as
	// the keycloak_idp_configs write it records.
	Enqueue(ctx context.Context, entry domain.AuditEntry) error
	// EnqueueOnce records entry as pending under the deterministic AL-5
	// Idempotency-Key idempotencyKey, inside the caller's RunInTx. A row
	// already holding that key makes it a no-op, so a replayed or resumed
	// signup never queues a second TermsAccepted (the first one wins).
	EnqueueOnce(ctx context.Context, idempotencyKey string, entry domain.AuditEntry) error
	// ClaimDue atomically claims up to limit due pending rows across every
	// tenant, incrementing each one's attempts and pushing its
	// next_attempt_at out by an exponential backoff — which doubles as a
	// lease, so a concurrent replica's claim skips them. Requires the
	// BYPASSRLS pool.
	ClaimDue(ctx context.Context, limit int) ([]*domain.PendingAuditEntry, error)
	// MarkDelivered deletes a delivered row.
	MarkDelivered(ctx context.Context, id uuid.UUID) error
	// MarkRetry records lastErr on a row left pending for its next
	// (already backed-off) attempt.
	MarkRetry(ctx context.Context, id uuid.UUID, lastErr string) error
	// MarkRetryAfter records lastErr and overrides next_attempt_at with
	// now()+delay — used when AL-5 returned a Retry-After hint on 429 that
	// should take precedence over the row's default exponential backoff.
	MarkRetryAfter(ctx context.Context, id uuid.UUID, lastErr string, delay time.Duration) error
	// MarkFailed parks a permanently-rejected row as 'failed'.
	MarkFailed(ctx context.Context, id uuid.UUID, lastErr string) error
}
