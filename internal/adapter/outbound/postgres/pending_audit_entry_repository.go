package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audit"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// PendingAuditEntryRepository implements port.PendingAuditEntryRepository
// on platform-audit's outbox Enqueuer (§4.4, §10.6). It writes on the app
// pool inside the caller's RunInTx (the transaction on ctx), RLS-bound to the
// tenant; delivery is the library Runner's, on the BYPASSRLS sysPool.
type PendingAuditEntryRepository struct {
	pool        *pgcommon.Pool
	enq         *auditoutbox.Enqueuer
	maxMetadata int
}

// errNoEnqueuer: a repository built without the Enqueuer fails loudly
// rather than dropping the entry.
var errNoEnqueuer = errors.New("pending_audit_entries: no platform-audit Enqueuer wired")

var _ port.PendingAuditEntryRepository = (*PendingAuditEntryRepository)(nil)

// NewPendingAuditEntryRepository constructs a PendingAuditEntryRepository
// that enqueues through enq, falling back to its own transaction on pool
// when ctx carries none.
func NewPendingAuditEntryRepository(pool *pgcommon.Pool, enq *auditoutbox.Enqueuer) *PendingAuditEntryRepository {
	return &PendingAuditEntryRepository{pool: pool, enq: enq, maxMetadata: audit.DefaultMaxMetadataBytes}
}

// WithMaxMetadataBytes sets the metadata cap the pre-enqueue validation
// uses; pass the Enqueuer's AUDIT_MAX_METADATA_BYTES so the two agree.
func (r *PendingAuditEntryRepository) WithMaxMetadataBytes(n int) *PendingAuditEntryRepository {
	if n > 0 {
		r.maxMetadata = n
	}
	return r
}

// Enqueue records entry under NewKeyStrict over the natural key of the
// change: entry type, tenant, target, action and OccurredAt. OccurredAt is
// fixed when the entry is built, before the transaction, so a retried
// transaction carries the same key and the same body (platform-audit §5.3).
func (r *PendingAuditEntryRepository) Enqueue(ctx context.Context, entry domain.AuditEntry) error {
	if r.enq == nil {
		return errNoEnqueuer
	}
	var targetType, targetID string
	if entry.Target != nil {
		targetType, targetID = entry.Target.Type, entry.Target.ID
	}
	key := r.enq.NewKeyStrict(entry.EntryType, entry.TenantID.String(), targetType, targetID, entry.Action,
		entry.OccurredAt.UTC().Format(time.RFC3339Nano))
	return r.enqueue(ctx, key, entry)
}

// EnqueueOnce records entry under idempotencyKey verbatim (audit.RawKey), so
// old and new code agree on TermsAccepted's established key. ON CONFLICT DO
// NOTHING makes a second enqueue of the same key a no-op; once delivered,
// Audit Log dedupes the same key itself.
func (r *PendingAuditEntryRepository) EnqueueOnce(ctx context.Context, idempotencyKey string, entry domain.AuditEntry) error {
	key, err := audit.RawKey(idempotencyKey)
	if err != nil {
		return err
	}
	return r.enqueue(ctx, key, entry)
}

func (r *PendingAuditEntryRepository) enqueue(ctx context.Context, key string, entry domain.AuditEntry) error {
	if r.enq == nil {
		return errNoEnqueuer
	}
	e := toLibraryEntry(entry)
	if err := r.failClosed(ctx, e, key); err != nil {
		return err
	}
	return withPool(ctx, r.pool, func(tx pgcommon.Tx) error {
		return r.enq.Enqueue(ctx, tx, e, key)
	})
}

// failClosed keeps this service's invariant that a committed business row
// always has its audit entry (§9.1, §10.6). platform-audit's Enqueue skips
// an invalid entry, and a row the RLS WITH CHECK rejects, and returns nil so
// the business write commits (its PA-INV-1). Here those cases fail the
// caller's transaction instead, before the library is called:
//   - an entry for a tenant other than the one bound on ctx (the GUC
//     app.tenant_id), which the RLS WITH CHECK would reject (42501);
//   - an entry the library's own validation rejects (NUL characters, a
//     malformed ip_address, metadata over the cap, a bad actor, …).
//
// What is left to the library's skip path is a rejection the row itself
// causes in Postgres after passing both checks, which the validation is
// designed to make unreachable.
func (r *PendingAuditEntryRepository) failClosed(ctx context.Context, e audit.Entry, key string) error {
	if g, ok := pgcommon.GUCSetFromContext(ctx); ok && g.TenantID != "" && g.TenantID != e.TenantID.String() {
		return fmt.Errorf("pending_audit_entries: entry tenant %s does not match the transaction tenant %s", e.TenantID, g.TenantID)
	}
	if err := audit.Validate(audit.FillDefaults(ctx, e, r.enq.SourceService()), key, r.maxMetadata); err != nil {
		return fmt.Errorf("pending_audit_entries: invalid audit entry: %w", err)
	}
	return nil
}

// toLibraryEntry maps the domain entry onto platform-audit's AL-5 body. The
// two share the AL-5 wire shape field for field.
func toLibraryEntry(e domain.AuditEntry) audit.Entry {
	out := audit.Entry{
		TenantID:        e.TenantID,
		EntryType:       e.EntryType,
		Action:          e.Action,
		Actor:           audit.Actor{Type: audit.ActorType(e.Actor.Type), ID: e.Actor.ID, Display: e.Actor.Display},
		OccurredAt:      e.OccurredAt,
		SourceService:   e.SourceService,
		SourceEventType: e.SourceEventType,
		IPAddress:       e.IPAddress,
		UserAgent:       e.UserAgent,
		TraceID:         e.TraceID,
		Metadata:        e.Metadata,
	}
	if e.Target != nil {
		out.Target = &audit.Target{Type: e.Target.Type, ID: e.Target.ID}
	}
	return out
}
