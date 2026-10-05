//go:build integration

// Package postgres_test covers PendingAuditEntryRepository (§4.4, §10.6):
// the durable TenantIdpConfigChanged hand-off to Audit Log's AL-5. Enqueue
// runs RLS-bound on the app pool; ClaimDue and the Mark* methods are the
// cross-tenant drainer's and run on the BYPASSRLS sysPool.
package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newIdpAuditEntry(tenantID uuid.UUID) domain.AuditEntry {
	return domain.AuditEntry{
		TenantID: tenantID, EntryType: domain.AuditEntryTypeIdpChanged, Action: domain.AuditActionCreate,
		Actor:      domain.AuditActor{Type: domain.AuditActorIAMSystem, ID: domain.SystemActorID},
		Target:     &domain.AuditTarget{Type: "idp", ID: "acme-sso"},
		OccurredAt: time.Now().UTC().Truncate(time.Millisecond), SourceService: domain.Source,
		SourceEventType: domain.AuditSourceTenantIdpConfigChanged,
		Metadata:        json.RawMessage(`{"validation_outcome":"pass"}`),
	}
}

func TestPendingAuditEntryRepository(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()
	repo := pgadapter.NewPendingAuditEntryRepository(appPool)
	sysRepo := pgadapter.NewPendingAuditEntryRepository(sysPool)

	// Subtests share one table and ClaimDue is a cross-tenant sweep, so
	// they run sequentially.

	t.Run("Enqueue_ThenClaimDue_RoundTripsEntryAndLeases", func(t *testing.T) {
		tenantID := uuid.New()
		want := newIdpAuditEntry(tenantID)
		require.NoError(t, repo.Enqueue(withTenant(ctx, tenantID), want))

		claimed, err := sysRepo.ClaimDue(ctx, 10)
		require.NoError(t, err)
		require.Len(t, claimed, 1)
		got := claimed[0]
		assert.Equal(t, tenantID, got.TenantID)
		assert.Equal(t, 1, got.Attempts)
		assert.Equal(t, want.EntryType, got.Entry.EntryType)
		assert.Equal(t, want.Target, got.Entry.Target)
		assert.True(t, want.OccurredAt.Equal(got.Entry.OccurredAt))
		var meta map[string]any
		require.NoError(t, json.Unmarshal(got.Entry.Metadata, &meta))
		assert.Equal(t, "pass", meta["validation_outcome"])

		// The claim pushed next_attempt_at out — an immediate re-claim
		// (e.g. a second replica) must not see the row again.
		again, err := sysRepo.ClaimDue(ctx, 10)
		require.NoError(t, err)
		assert.Empty(t, again)

		require.NoError(t, sysRepo.MarkDelivered(ctx, got.ID))
		var n int
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT count(*) FROM pending_audit_entries WHERE id = $1`, got.ID).Scan(&n))
		assert.Zero(t, n, "a delivered row is deleted")
	})

	t.Run("MarkRetry_KeepsPending_MarkFailed_Parks", func(t *testing.T) {
		tenantID := uuid.New()
		require.NoError(t, repo.Enqueue(withTenant(ctx, tenantID), newIdpAuditEntry(tenantID)))
		require.NoError(t, repo.Enqueue(withTenant(ctx, tenantID), newIdpAuditEntry(tenantID)))
		claimed, err := sysRepo.ClaimDue(ctx, 10)
		require.NoError(t, err)
		require.Len(t, claimed, 2)

		require.NoError(t, sysRepo.MarkRetry(ctx, claimed[0].ID, "connection refused"))
		require.NoError(t, sysRepo.MarkFailed(ctx, claimed[1].ID, "422 unknown_entry_type"))

		var status, lastErr string
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT status, last_error FROM pending_audit_entries WHERE id = $1`, claimed[0].ID).Scan(&status, &lastErr))
		assert.Equal(t, "pending", status)
		assert.Equal(t, "connection refused", lastErr)
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT status, last_error FROM pending_audit_entries WHERE id = $1`, claimed[1].ID).Scan(&status, &lastErr))
		assert.Equal(t, "failed", status)

		// Once its backoff elapses the retry row is claimable again; the
		// failed row never is.
		_, err = rawPool.Exec(ctx, `UPDATE pending_audit_entries SET next_attempt_at = now() - interval '1 second'`)
		require.NoError(t, err)
		reclaimed, err := sysRepo.ClaimDue(ctx, 10)
		require.NoError(t, err)
		require.Len(t, reclaimed, 1)
		assert.Equal(t, claimed[0].ID, reclaimed[0].ID)
		assert.Equal(t, 2, reclaimed[0].Attempts)

		gauges := pgadapter.NewGaugeRepository(sysPool)
		backlog, err := gauges.AuditEntryBacklog(ctx)
		require.NoError(t, err)
		assert.Equal(t, map[string]int64{"pending": 1, "failed": 1}, backlog)

		_, err = rawPool.Exec(ctx, `DELETE FROM pending_audit_entries`)
		require.NoError(t, err)
	})

	t.Run("ClaimDue_RespectsLimit", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			tenantID := uuid.New()
			require.NoError(t, repo.Enqueue(withTenant(ctx, tenantID), newIdpAuditEntry(tenantID)))
		}
		claimed, err := sysRepo.ClaimDue(ctx, 2)
		require.NoError(t, err)
		assert.Len(t, claimed, 2)
	})

	t.Run("Enqueue_CrossTenantWriteRejectedByRLS", func(t *testing.T) {
		err := repo.Enqueue(withTenant(ctx, uuid.New()), newIdpAuditEntry(uuid.New()))
		require.Error(t, err, "WITH CHECK must reject an entry for a tenant other than the bound GUC")
	})

}
