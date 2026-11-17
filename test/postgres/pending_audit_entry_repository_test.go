//go:build integration

// Package postgres_test covers PendingAuditEntryRepository (§4.4, §10.6) on
// platform-audit's outbox: the as-built pending_audit_entries table is
// converted by AdoptAuditOutbox (setupTestDB runs it, as MigrateSchema
// does), Enqueue runs RLS-bound on the app pool inside the business
// transaction, and the library Runner delivers from the BYPASSRLS sysPool.
package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audittest"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// newTestEnqueuer is the Enqueuer cmd/server builds from the AUDIT_* env.
func newTestEnqueuer(t testing.TB) *auditoutbox.Enqueuer {
	t.Helper()
	enq, err := auditoutbox.NewEnqueuer(auditoutbox.EnqueueConfig{SourceService: domain.Source})
	require.NoError(t, err)
	return enq
}

// newAuditRepo is a PendingAuditEntryRepository on pool with the test
// Enqueuer.
func newAuditRepo(t testing.TB, pool *pgcommon.Pool) *pgadapter.PendingAuditEntryRepository {
	t.Helper()
	return pgadapter.NewPendingAuditEntryRepository(pool, newTestEnqueuer(t))
}

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
	repo := newAuditRepo(t, appPool)
	tx := pgadapter.NewTxRunner(appPool, nil)

	count := func(t *testing.T, tenantID uuid.UUID) int {
		t.Helper()
		var n int
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT count(*) FROM pending_audit_entries WHERE tenant_id = $1`, tenantID).Scan(&n))
		return n
	}

	t.Run("Schema_IsLibraryShape_AndAdoptionIsIdempotent", func(t *testing.T) {
		var trig bool
		require.NoError(t, rawPool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'platform_audit_compat')`).Scan(&trig))
		assert.True(t, trig, "release N keeps the compatibility trigger until FinishAdoption")
		var keyNotNull bool
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT attnotnull FROM pg_attribute
			WHERE attrelid = 'pending_audit_entries'::regclass AND attname = 'idempotency_key'`).Scan(&keyNotNull))
		assert.True(t, keyNotNull)
		var version int64
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT version FROM `+auditoutbox.MigrationsTable).Scan(&version))
		assert.Equal(t, int64(1), version, "ApplySchema holds at version 1 while the compatibility trigger exists")

		superDSN := rawPool.Config().ConnString()
		require.NoError(t, pgadapter.AdoptAuditOutbox(ctx, superDSN, nil), "a second run is a no-op")
	})

	t.Run("Enqueue_InTx_WritesOneRowPerChange", func(t *testing.T) {
		tenantID := uuid.New()
		entry := newIdpAuditEntry(tenantID)
		tctx := withTenant(ctx, tenantID)
		for range 2 { // a retried transaction carries the same entry, hence the same key
			require.NoError(t, tx.RunInTx(tctx, func(ctx context.Context) error { return repo.Enqueue(ctx, entry) }))
		}
		assert.Equal(t, 1, count(t, tenantID))

		var key string
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT idempotency_key FROM pending_audit_entries WHERE tenant_id = $1`, tenantID).Scan(&key))
		_, err := uuid.Parse(key)
		assert.NoError(t, err, "Enqueue keys are NewKeyStrict UUIDs")

		other := entry
		other.OccurredAt = entry.OccurredAt.Add(time.Second)
		require.NoError(t, tx.RunInTx(tctx, func(ctx context.Context) error { return repo.Enqueue(ctx, other) }))
		assert.Equal(t, 2, count(t, tenantID), "a different change is a different key")
	})

	t.Run("Enqueue_RolledBackTx_LeavesNoRow", func(t *testing.T) {
		tenantID := uuid.New()
		err := tx.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
			require.NoError(t, repo.Enqueue(ctx, newIdpAuditEntry(tenantID)))
			return assert.AnError
		})
		require.ErrorIs(t, err, assert.AnError)
		assert.Zero(t, count(t, tenantID), "the entry commits or rolls back with the business write")
	})

	t.Run("EnqueueOnce_KeepsTheRawKey_AndDedupes", func(t *testing.T) {
		tenantID := uuid.New()
		key := domain.TermsAcceptedIdempotencyKey(tenantID, uuid.New(), "2026-09")
		tctx := withTenant(ctx, tenantID)
		for range 2 {
			require.NoError(t, repo.EnqueueOnce(tctx, key, newIdpAuditEntry(tenantID)))
		}
		var got string
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT idempotency_key FROM pending_audit_entries WHERE tenant_id = $1`, tenantID).Scan(&got))
		assert.Equal(t, key, got)
		assert.Equal(t, 1, count(t, tenantID))
	})

	// Fail closed: an entry for another tenant (which the RLS WITH CHECK
	// would reject, and the library would then skip) or one Audit Log would
	// reject fails the transaction, so the business write rolls back with it.
	t.Run("Enqueue_FailsClosed_RollsBackTheBusinessWrite", func(t *testing.T) {
		tenantID, victim := uuid.New(), uuid.New()
		err := tx.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
			return repo.Enqueue(ctx, newIdpAuditEntry(victim))
		})
		require.ErrorContains(t, err, "does not match the transaction tenant")
		assert.Zero(t, count(t, victim))

		bad := newIdpAuditEntry(tenantID)
		bad.IPAddress = "not-an-ip"
		err = tx.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
			return repo.Enqueue(ctx, bad)
		})
		require.ErrorContains(t, err, "invalid audit entry")
		assert.Zero(t, count(t, tenantID))
	})

	// An old pod (pre-adoption code) inserts without a key and parks
	// without failed_at: the compatibility trigger fills both.
	t.Run("CompatTrigger_AcceptsOldCodeInserts", func(t *testing.T) {
		tenantID := uuid.New()
		var id uuid.UUID
		var key string
		require.NoError(t, rawPool.QueryRow(ctx,
			`INSERT INTO pending_audit_entries (tenant_id, entry) VALUES ($1, '{}') RETURNING id, idempotency_key`, tenantID).Scan(&id, &key))
		assert.Equal(t, id.String(), key)
		_, err := rawPool.Exec(ctx, `UPDATE pending_audit_entries SET status = 'failed', last_error = 'audit entry rejected by audit log: 422' WHERE id = $1`, id)
		require.NoError(t, err)
		var outcome string
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT last_outcome FROM pending_audit_entries WHERE id = $1`, id).Scan(&outcome))
		assert.Equal(t, "rejected", outcome)
		_, err = rawPool.Exec(ctx, `DELETE FROM pending_audit_entries WHERE id = $1`, id)
		require.NoError(t, err)
	})

	t.Run("AppRole_HasEnqueueAccess", func(t *testing.T) {
		require.NoError(t, auditoutbox.CheckEnqueueAccess(ctx, appPool))
	})

	// The Runner on the BYPASSRLS sysPool (the migrator role, with the
	// migration's grants) passes NewRunner's cross-tenant access check and
	// delivers every tenant's rows, deleting each on 201.
	t.Run("Runner_OnSysPool_DeliversAndDeletes", func(t *testing.T) {
		_, err := rawPool.Exec(ctx, `DELETE FROM pending_audit_entries`)
		require.NoError(t, err)
		a, b := uuid.New(), uuid.New()
		require.NoError(t, tx.RunInTx(withTenant(ctx, a), func(ctx context.Context) error { return repo.Enqueue(ctx, newIdpAuditEntry(a)) }))
		termsKey := domain.TermsAcceptedIdempotencyKey(b, uuid.New(), "2026-09")
		terms := domain.TermsAcceptedAuditEntry(b, uuid.New(), domain.TermsAcceptance{Version: "2026-09", AcceptedAt: time.Now().UTC()}, "trial", "free", "")
		require.NoError(t, repo.EnqueueOnce(withTenant(ctx, b), termsKey, terms))

		srv := audittest.NewServer(t)
		runner, err := auditoutbox.NewRunner(auditoutbox.Config{
			Pool: sysPool, Client: srv.Client(domain.Source), Enqueuer: newTestEnqueuer(t),
			PurgeInterval: 0,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.Stop(ctx) })

		res, err := runner.DrainOnce(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, res.Delivered)
		srv.AssertRecorded(t, audittest.Match{EntryType: domain.AuditEntryTypeIdpChanged, TenantID: a})
		srv.AssertRecorded(t, audittest.Match{EntryType: domain.AuditEntryTypeTermsAccepted, TenantID: b, IdempotencyKey: termsKey})
		assert.Zero(t, count(t, a))
		assert.Zero(t, count(t, b))
	})
}

// Every entry type this service emits is in Audit Log's taxonomy
// (platform-audit iam-adopters §3).
func TestAuditEntryTypes_AreKnownToAuditLog(t *testing.T) {
	audittest.RequireKnownTypes(t, domain.AuditEntryTypeIdpChanged, domain.AuditEntryTypeTermsAccepted)
}
