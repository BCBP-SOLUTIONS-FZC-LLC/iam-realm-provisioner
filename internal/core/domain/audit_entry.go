package domain

import (
	"time"

	"github.com/google/uuid"
)

// Audit Log direct-write constants (HLD §9.4, §10.6). TenantIdpConfigChanged
// is an Audit Log *entry type*, not a bus event — it is deliberately absent
// from the 9-event iam-tenant-events inventory above (§25) and reaches
// Audit Log through its AL-5 direct-write ingest
// (POST /api/v1/internal/audit-entries) instead.
const (
	// AuditEntryTypeIdpChanged is Audit Log's taxonomy value for an IdP
	// configuration change (Audit LLD §7.1, retention tier security_3y —
	// derived by Audit, never chosen here).
	AuditEntryTypeIdpChanged = "config.idp.changed"
	// AuditSourceTenantIdpConfigChanged is this service's verbatim entry
	// name, carried as AL-5's source_event_type (provenance).
	AuditSourceTenantIdpConfigChanged = "TenantIdpConfigChanged"
)

// AuditAction values — AL-5's coarse verb.
const (
	AuditActionCreate = "create"
	AuditActionUpdate = "update"
	AuditActionDelete = "delete"
)

// AuditActorType values — AL-5's actor.type vocabulary this service uses.
const (
	AuditActorUser      = "user"
	AuditActorIAMSystem = "iam_system"
)

// AuditActor is AL-5's actor object.
type AuditActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// AuditTarget is AL-5's target object.
type AuditTarget struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// AuditEntry is one AL-5 direct-write audit entry, persisted verbatim in
// `pending_audit_entries.entry` until delivered. Its JSON shape is AL-5's
// request body.
type AuditEntry struct {
	TenantID        uuid.UUID      `json:"tenant_id"`
	EntryType       string         `json:"entry_type"`
	Action          string         `json:"action"`
	Actor           AuditActor     `json:"actor"`
	Target          *AuditTarget   `json:"target,omitempty"`
	OccurredAt      time.Time      `json:"occurred_at"`
	SourceService   string         `json:"source_service"`
	SourceEventType string         `json:"source_event_type"`
	IPAddress       string         `json:"ip_address,omitempty"`
	UserAgent       string         `json:"user_agent,omitempty"`
	TraceID         string         `json:"trace_id,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

// PendingAuditEntry is one claimed `pending_audit_entries` row. ID is the
// AL-5 Idempotency-Key — minted once at enqueue, so every delivery retry
// of the same entry is deduplicated by Audit Log.
type PendingAuditEntry struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	Entry    AuditEntry
	Attempts int
}
