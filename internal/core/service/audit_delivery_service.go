package service

import (
	"context"
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// AuditDeliveryService drains pending_audit_entries to Audit Log's AL-5
// direct-write ingest (HLD §9.4, §10.6) — the delivery half of the
// TenantIdpConfigChanged hand-off IdpService enqueues transactionally.
// Delivery is at-least-once: each row's id is its AL-5 Idempotency-Key,
// so a retry after a lost response never double-records.
type AuditDeliveryService struct {
	entries port.PendingAuditEntryRepository
	client  port.AuditLogClient
	log     port.SlogStyleLogger
}

// NewAuditDeliveryService wires AuditDeliveryService. entries must be
// backed by the BYPASSRLS sysPool — the drain is cross-tenant.
func NewAuditDeliveryService(entries port.PendingAuditEntryRepository, client port.AuditLogClient) *AuditDeliveryService {
	return &AuditDeliveryService{entries: entries, client: client}
}

// WithLogger attaches a logger and returns s for chaining.
func (s *AuditDeliveryService) WithLogger(log port.Logger) *AuditDeliveryService {
	s.log = port.NewSlogStyleLogger(log)
	return s
}

// DrainOnce claims up to limit due rows and delivers each: success deletes
// the row, a permanent rejection (port.ErrAuditEntryRejected) parks it as
// 'failed', and anything else leaves it pending for its already-scheduled
// backoff retry. Returns the number delivered; an error only when the
// claim itself fails.
func (s *AuditDeliveryService) DrainOnce(ctx context.Context, limit int) (int, error) {
	due, err := s.entries.ClaimDue(ctx, limit)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, p := range due {
		werr := s.client.WriteEntry(ctx, p.ID.String(), p.Entry)
		switch {
		case werr == nil:
			metrics.AuditEntryDeliveryTotal.WithLabelValues("delivered").Inc()
			delivered++
			if err := s.entries.MarkDelivered(ctx, p.ID); err != nil {
				// Harmless: the row is redelivered after its backoff and
				// AL-5 answers the replayed key with 200.
				s.log.WarnContext(ctx, "audit_delivery: mark delivered failed", "entry_id", p.ID, "error", err.Error())
			}
		case errors.Is(werr, port.ErrAuditEntryRejected):
			metrics.AuditEntryDeliveryTotal.WithLabelValues("rejected").Inc()
			s.log.ErrorContext(ctx, "audit_delivery: entry permanently rejected by audit log", "entry_id", p.ID, "tenant_id", p.TenantID, "error", werr.Error())
			if err := s.entries.MarkFailed(ctx, p.ID, werr.Error()); err != nil {
				s.log.WarnContext(ctx, "audit_delivery: mark failed did not persist", "entry_id", p.ID, "error", err.Error())
			}
		default:
			metrics.AuditEntryDeliveryTotal.WithLabelValues("retry").Inc()
			s.log.WarnContext(ctx, "audit_delivery: delivery failed, will retry", "entry_id", p.ID, "attempts", p.Attempts, "error", werr.Error())
			if err := s.entries.MarkRetry(ctx, p.ID, werr.Error()); err != nil {
				s.log.WarnContext(ctx, "audit_delivery: mark retry did not persist", "entry_id", p.ID, "error", err.Error())
			}
		}
	}
	return delivered, nil
}
