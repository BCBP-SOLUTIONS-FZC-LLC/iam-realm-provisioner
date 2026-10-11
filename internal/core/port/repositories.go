package port

import (
	"context"
	"encoding/json"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/google/uuid"
)

// TenantRealmRepository persists `tenant_realms` (§4.2). Every method binds
// the RLS GUC transaction-locally via the caller's ctx (RLS-6).
type TenantRealmRepository interface {
	FindByTenantID(ctx context.Context, tenantID uuid.UUID) (*domain.TenantRealm, error)
	Upsert(ctx context.Context, realm *domain.TenantRealm) error
	Update(ctx context.Context, realm *domain.TenantRealm) error
	// ListExpiredTrials returns shared-trial realms whose trial_ends_at has
	// passed (RP-C1, idx_tenant_realms_trial_exp), limited to limit rows.
	ListExpiredTrials(ctx context.Context, limit int) ([]*domain.TenantRealm, error)
	// ListSuspendedPastRetention returns dedicated realms suspended by the
	// RP-C3 billing-lapse sweep (never operator suspensions, §8.8) whose
	// disabled_at is older than retentionDays, limited to limit rows.
	ListSuspendedPastRetention(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error)
	// CountsByShard returns the current live dedicated-realm count per
	// keycloak_shard, read from the local mirror (§9.5 shard placement —
	// this service is the single writer, so the count is authoritative
	// without re-querying Keycloak).
	CountsByShard(ctx context.Context) (map[string]int, error)
}

// IdpConfigRepository persists `keycloak_idp_configs` (§4.3).
type IdpConfigRepository interface {
	FindByAlias(ctx context.Context, tenantID uuid.UUID, alias string) (*domain.IdpConfig, error)
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*domain.IdpConfig, error)
	Upsert(ctx context.Context, cfg *domain.IdpConfig) error
	Delete(ctx context.Context, tenantID uuid.UUID, alias string) error
	// ListAll returns every IdP config across every tenant (RP-13 —
	// operator-only whole-estate portability audit). Callers must use a
	// BYPASSRLS pool since this is deliberately cross-tenant.
	ListAll(ctx context.Context) ([]*domain.IdpConfig, error)
}

// IdpValidationRepository persists `keycloak_idp_config_validations` (§4.3)
// — append-only, one row per §11.6 validation run.
type IdpValidationRepository interface {
	Insert(ctx context.Context, v *domain.IdpConfigValidation) error
	LatestForAlias(ctx context.Context, tenantID uuid.UUID, alias string) (*domain.IdpConfigValidation, error)
}

// ReconciliationRepository persists `realm_config_reconciliations` (§4.4) —
// backs RP-INV-6 durability.
type ReconciliationRepository interface {
	Insert(ctx context.Context, r *domain.RealmConfigReconciliation) error
	ListPending(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error)
	MarkApplied(ctx context.Context, id uuid.UUID) error
	// MarkFailed increments id's attempt count and records lastErr,
	// atomically moving it to the terminal 'failed' status instead of
	// leaving it 'pending' once the new attempt count reaches maxAttempts
	// (RP-C4 poison-pill cap, §8.10) — otherwise a permanently-broken
	// setting would retry every tick forever. Returns the row's resulting
	// status so the caller can distinguish "still pending, retry next
	// tick" from "gave up".
	MarkFailed(ctx context.Context, id uuid.UUID, lastErr string, maxAttempts int) (domain.ReconStatus, error)
}

// IdempotencyStore implements the `provisioning_operations` ledger (§4.4,
// §9.2). Every externally-triggered mutation is keyed here so a retry or
// at-least-once redelivery replays the stored result instead of
// re-executing the Keycloak mutation.
type IdempotencyStore interface {
	// Begin inserts an `in_progress` row. Returns (existing, true, nil) when
	// the key already exists (caller must inspect existing.Status: replay
	// `succeeded`.Result, or return 409 operation_in_progress for
	// `in_progress`).
	Begin(ctx context.Context, key string, tenantID uuid.UUID, operation string) (existing *domain.ProvisioningOperation, found bool, err error)
	Complete(ctx context.Context, key string, result json.RawMessage) error
	Fail(ctx context.Context, key string) error
}

// Inbox is the exactly-once consumer port backing the processed_events
// table (§4.5, §7, IDEMP-1..4), implemented over platform-events'
// pkg/inbox (one inbox.Store per consumer name) — matching
// iam-org-membership's port.Inbox.
//
// ProcessOnce claims (consumer, eventID) and runs fn in ONE transaction:
// the claim (INSERT … ON CONFLICT DO NOTHING) is the first statement, so a
// concurrent delivery of the same event blocks on the claim's row lock and
// then sees it as a duplicate, instead of both deliveries applying the
// projection twice. fn's ctx carries that transaction (and the tx-bound
// event publisher), so every repository call inside fn joins it. If fn
// returns an error nothing is recorded and the message is retried.
// duplicate is true when the event was already processed; fn was not run.
//
// IsProcessed is the cheap pre-check a consumer runs before an external
// side effect that must not repeat on a duplicate (a Keycloak call, which
// by §9.1 never runs inside a database transaction).
//
// eventID must be a UUID (every platform-events envelope ID is).
type Inbox interface {
	IsProcessed(ctx context.Context, consumer domain.ProcessedEventsConsumer, eventID string) (bool, error)
	ProcessOnce(ctx context.Context, consumer domain.ProcessedEventsConsumer, eventID, eventType string, fn func(txCtx context.Context) error) (duplicate bool, err error)
}

// InboxPruner deletes one consumer's processed_events rows older than
// retention, in batches of batch rows until none are left (PE-1).
type InboxPruner interface {
	Prune(ctx context.Context, consumer domain.ProcessedEventsConsumer, retention time.Duration, batch int) (int64, error)
}

// RetentionPruner deletes rows past their retention window from the
// operational/audit tables no other job prunes (ledger-prune). Each method
// deletes in batches of batch rows until none is left and returns the
// total. Requires the BYPASSRLS pool (cross-tenant).
type RetentionPruner interface {
	// PruneProvisioningOperations deletes idempotency-ledger rows created
	// before olderThan ago — far past any caller's retry window and the
	// in_progress lease.
	PruneProvisioningOperations(ctx context.Context, olderThan time.Duration, batch int) (int64, error)
	// PruneRLSViolationLog deletes sampled RLS violation records older than
	// olderThan (they are already counted into rls_violations_total).
	PruneRLSViolationLog(ctx context.Context, olderThan time.Duration, batch int) (int64, error)
}
