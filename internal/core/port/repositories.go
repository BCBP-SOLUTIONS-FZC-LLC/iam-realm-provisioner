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
	// ExpireStaleInProgress transitions `in_progress` rows older than
	// olderThan to `failed`, up to limit rows per call. This unblocks callers
	// that received a 503 mid-operation and cannot retry because their key
	// appears stuck in_progress (the operation's transaction rolled back but
	// Fail was never reached). Returns the count of rows transitioned.
	ExpireStaleInProgress(ctx context.Context, olderThan time.Duration, limit int) (int, error)
}

// ProcessedEventsStore implements the consumer dedup ledger (§4.5, §7).
// The concrete implementation is InboxStoreAdapter, which routes to three
// platform-events inbox.Store instances — one per inbound consumer. All
// processed_events SQL is owned by inbox.Store; this port defines the
// per-consumer lookup/mark/prune contract used by consumers and the
// reconciler's processed-events-prune job.
type ProcessedEventsStore interface {
	IsProcessed(ctx context.Context, consumer domain.ProcessedEventsConsumer, eventID string) (bool, error)
	MarkProcessed(ctx context.Context, consumer domain.ProcessedEventsConsumer, eventID string) error
	// Prune deletes up to limit rows older than ttlDays across all consumers
	// (PE-1 / IDEMP-4). Batched so a backlog larger than one tick converges
	// over several ticks instead of one unbounded DELETE.
	Prune(ctx context.Context, ttlDays, limit int) (int, error)
}
