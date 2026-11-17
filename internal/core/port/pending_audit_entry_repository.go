package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// PendingAuditEntryRepository is the enqueue half of the Audit Log AL-5
// hand-off (§4.4, §10.6): it writes an entry into `pending_audit_entries`
// inside the caller's RunInTx, so the entry commits or rolls back with the
// business write it records. Delivery is platform-audit's outbox Runner
// (cmd/server), not this port.
//
// Fail closed: an entry that Audit Log would reject, or one for a tenant
// other than the transaction's, is an error, so the caller's RunInTx rolls
// back and a committed business row always has its audit entry (§9.1,
// §10.6). This deliberately departs from platform-audit's default, which
// skips such an entry and lets the write commit (its PA-INV-1).
type PendingAuditEntryRepository interface {
	// Enqueue records entry under a key derived from the natural key of the
	// change (entry type, tenant, target, action and entry.ChangeID, or
	// OccurredAt when ChangeID is empty), so the same change enqueues the
	// same entry once.
	Enqueue(ctx context.Context, entry domain.AuditEntry) error
	// EnqueueOnce records entry under the caller's established key verbatim
	// (TermsAccepted's TermsAccepted:<tenant>:<user>:<version>). A row
	// already holding that key makes it a no-op, so a replayed or resumed
	// signup never queues a second TermsAccepted (the first one wins).
	EnqueueOnce(ctx context.Context, idempotencyKey string, entry domain.AuditEntry) error
}
