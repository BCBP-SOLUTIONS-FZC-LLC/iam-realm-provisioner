//go:build integration

// Package postgres_test covers IdempotencyRepository (§4.4, §9.2) against
// `provisioning_operations`: Begin's ON CONFLICT DO UPDATE probe (which
// reclaims a `failed` row back to `in_progress` but never touches a
// `succeeded` or genuinely `in_progress` one), Complete/Fail's terminal
// transitions, and a genuine cross-tenant idempotency-key collision edge
// case that RLS interacts badly with (see
// TestIdempotencyRepository/Begin_CrossTenantKeyCollision_DefeatsIdempotency
// below).
package postgres_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdempotencyRepository(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewIdempotencyRepository(appPool)

	t.Run("Begin_FreshKey_InsertsInProgressRow", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		key := "provision-trial:" + tenantID.String()

		existing, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		assert.False(t, found)
		assert.Nil(t, existing)
	})

	t.Run("Begin_DuplicateKey_ReportsInProgress", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		key := "provision-trial:" + tenantID.String()

		_, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		require.False(t, found)

		existing, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		require.True(t, found)
		require.NotNil(t, existing)
		assert.Equal(t, domain.OpStatusInProgress, existing.Status)
		assert.Equal(t, tenantID, existing.TenantID)
		assert.Equal(t, domain.OpProvisionTrial, existing.Operation)
	})

	t.Run("Complete_ThenBeginReplaysStoredResult", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		key := "provision-trial:" + tenantID.String()

		_, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		require.False(t, found)

		result := json.RawMessage(`{"tenant_id":"` + tenantID.String() + `","realm":"trial"}`)
		require.NoError(t, repo.Complete(tctx, key, result))

		existing, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		require.True(t, found)
		require.NotNil(t, existing)
		assert.Equal(t, domain.OpStatusSucceeded, existing.Status)
		assert.JSONEq(t, string(result), string(existing.Result))
		require.NotNil(t, existing.CompletedAt)
	})

	t.Run("Fail_ThenBeginReclaimsTheRowAndProceeds", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		key := "provision-trial:" + tenantID.String()

		_, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		require.False(t, found)
		require.NoError(t, repo.Fail(tctx, key))

		// Begin reclaims a `failed` row back to `in_progress` and reports
		// found=false — the caller proceeds and re-executes the operation,
		// exactly as Fail's own doc comment promises ("caller retries with
		// the same key, which re-executes the operation"). Without this,
		// idempotency_key's PRIMARY KEY plus no cleanup job for
		// provisioning_operations would let one transient failure
		// permanently 409 every future retry of the same key.
		existing, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		assert.False(t, found)
		assert.Nil(t, existing)

		// The reclaimed row really is reset, not left half-failed: a
		// subsequent duplicate Begin (as if two requests raced after the
		// reclaim) reports it in_progress, not failed, with the stale
		// result/completed_at cleared.
		reclaimed, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		require.True(t, found)
		require.NotNil(t, reclaimed)
		assert.Equal(t, domain.OpStatusInProgress, reclaimed.Status)
		assert.Nil(t, reclaimed.CompletedAt)
		assert.Empty(t, reclaimed.Result)
	})

	t.Run("Succeeded_ThenFailedRow_Begin_ReportsSucceededNeverReclaimed", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		key := "provision-trial:" + tenantID.String()

		_, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		require.False(t, found)
		result := json.RawMessage(`{"ok":true}`)
		require.NoError(t, repo.Complete(tctx, key, result))

		// Only a `failed` row is ever reclaimed — a `succeeded` row must
		// stay a pure replay target forever, never re-executed.
		existing, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		require.True(t, found)
		require.NotNil(t, existing)
		assert.Equal(t, domain.OpStatusSucceeded, existing.Status)
		assert.JSONEq(t, string(result), string(existing.Result))
	})

	t.Run("Begin_DuplicateKey_InProgressIsNeverReclaimed", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		key := "provision-trial:" + tenantID.String()

		_, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		require.False(t, found)

		// A genuinely still-in_progress row must never be reclaimed —
		// doing so risks two concurrent executions of the same operation.
		existing, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		require.True(t, found)
		require.NotNil(t, existing)
		assert.Equal(t, domain.OpStatusInProgress, existing.Status)
	})

	t.Run("Complete_UnknownKey_IsSilentNoOp", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		key := "never-begun:" + tenantID.String()

		assert.NoError(t, repo.Complete(tctx, key, json.RawMessage(`{}`)))

		existing, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		assert.False(t, found, "Complete on a key nobody Begin'd must not fabricate a row")
		assert.Nil(t, existing)
	})

	t.Run("Fail_UnknownKey_IsSilentNoOp", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		key := "never-begun-fail:" + tenantID.String()

		assert.NoError(t, repo.Fail(tctx, key))

		existing, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		assert.False(t, found)
		assert.Nil(t, existing)
	})

	t.Run("Begin_ConcurrentSameKey_ExactlyOneWinsFresh", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		key := "concurrent:" + tenantID.String()

		const n = 8
		var freshCount int32
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				_, found, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
				assert.NoError(t, err)
				if !found {
					atomic.AddInt32(&freshCount, 1)
				}
			}()
		}
		wg.Wait()
		assert.Equal(t, int32(1), freshCount, "the unique idempotency_key PK must let exactly one concurrent Begin see found=false")
	})

	// Begin_CrossTenantKeyCollision_DefeatsIdempotency documents a genuine
	// architectural edge case discovered while writing this test: if two
	// different tenants ever race on the SAME literal idempotency_key value
	// (a caller bug — e.g. a client that doesn't namespace keys by tenant,
	// or a replayed header reused across tenants), Begin's RLS interaction
	// silently defeats the idempotency guarantee it exists to provide,
	// rather than surfacing a conflict:
	//
	//  1. Tenant A's Begin succeeds normally, inserting a row it owns.
	//  2. Tenant B's Begin then runs the same
	//     `INSERT ... ON CONFLICT (idempotency_key) DO NOTHING`. Postgres
	//     enforces the PRIMARY KEY globally regardless of row visibility,
	//     so the insert conflicts (RowsAffected()==0) — but the very next
	//     statement, the SELECT that fetches "the existing row" to return
	//     to the caller, is RLS-scoped and tenant A's row is invisible to
	//     tenant B's GUC. That SELECT returns zero rows, and Begin's own
	//     code (`if errors.Is(scanErr, pgx.ErrNoRows) { found = false }`)
	//     then reports the call as a **fresh** Begin — found=false,
	//     existing=nil — even though a row for that key already exists.
	//  3. Because the row was never actually inserted for tenant B (the
	//     conflict silently swallowed the INSERT), every subsequent Begin
	//     call from tenant B with the same key repeats step 2 forever: it
	//     is reported "fresh" every single time, never landing a row of
	//     its own. A later Complete/Fail(key) call from tenant B's context
	//     is likewise RLS-filtered to 0 rows affected and returns nil
	//     error — a silent no-op — so no failure ever surfaces to the
	//     caller.
	//
	// Net effect: a cross-tenant idempotency-key collision does not
	// error out or corrupt tenant A's row, but it permanently strips
	// idempotency from tenant B's operation under that key — every retry
	// looks "fresh" and would re-execute the Keycloak-effect-first flow
	// from scratch. This is a real (if narrow) gap between the RLS model
	// and the idempotency ledger's global PK, not a bug in this test.
	t.Run("Begin_CrossTenantKeyCollision_DefeatsIdempotency", func(t *testing.T) {
		t.Parallel()
		tenantA, tenantB := uuid.New(), uuid.New()
		key := "shared-key-collision"

		existing, found, err := repo.Begin(withTenant(ctx, tenantA), key, tenantA, domain.OpProvisionTrial)
		require.NoError(t, err)
		require.False(t, found)
		require.Nil(t, existing)

		for i := 0; i < 2; i++ {
			existing, found, err = repo.Begin(withTenant(ctx, tenantB), key, tenantB, domain.OpProvisionDirect)
			require.NoError(t, err)
			assert.False(t, found, "tenant B's colliding Begin is reported fresh every time, not just once")
			assert.Nil(t, existing)
		}

		err = repo.Complete(withTenant(ctx, tenantB), key, json.RawMessage(`{"tenant_id":"`+tenantB.String()+`"}`))
		assert.NoError(t, err, "Complete never checks RowsAffected, so an RLS-blocked update against tenant A's row returns nil, not an error")

		// Read ground truth as the superuser (bypasses RLS) rather than
		// through either tenant's own RLS-filtered view.
		var status, ownerTenantID string
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT status, tenant_id::text FROM provisioning_operations WHERE idempotency_key = $1`, key).
			Scan(&status, &ownerTenantID))
		assert.Equal(t, "in_progress", status, "tenant A's original row must be completely untouched by tenant B's collision")
		assert.Equal(t, tenantA.String(), ownerTenantID)
	})
}

// TestIdempotencyRepository_ExpireStaleInProgress covers the idempotency-cleanup
// job path (§9.2): rows that remain in_progress longer than a TTL threshold
// are transitioned to failed, unblocking callers whose key was stuck after a
// 503 mid-operation (the transaction rolled back before Fail could run).
//
// provisioning_operations is RLS-scoped, so:
//   - Begin/Complete/Fail use the app pool (tenant GUC injected via tctx).
//   - ExpireStaleInProgress is a cross-tenant sweep and uses sysPool (BYPASSRLS),
//     matching how cmd/reconciler wires it: pgadapter.NewIdempotencyRepository(sysPool).
func TestIdempotencyRepository_ExpireStaleInProgress(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()
	// App-pool repo for tenant-scoped operations (Begin/Complete/Fail).
	repo := pgadapter.NewIdempotencyRepository(appPool)
	// Sys-pool repo for the cross-tenant expire sweep — matches reconciler wiring.
	sysRepo := pgadapter.NewIdempotencyRepository(sysPool)
	tenantID := uuid.New()
	tctx := withTenant(ctx, tenantID)

	t.Run("ExpiresRowsOlderThanTTL", func(t *testing.T) {
		key := "expire-old-" + uuid.NewString()
		_, _, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		// Backdate the row so it appears stale.
		_, err = rawPool.Exec(ctx,
			`UPDATE provisioning_operations SET created_at = now() - interval '2 hours' WHERE idempotency_key = $1`, key)
		require.NoError(t, err)

		n, err := sysRepo.ExpireStaleInProgress(ctx, time.Hour, 100)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, n, 1, "at least the backdated row must be expired")

		var status string
		require.NoError(t, rawPool.QueryRow(ctx,
			`SELECT status FROM provisioning_operations WHERE idempotency_key = $1`, key).Scan(&status))
		assert.Equal(t, "failed", status, "expired row must be transitioned to failed so the next Begin reclaims it")
	})

	t.Run("DoesNotExpireRecentRows", func(t *testing.T) {
		key := "expire-recent-" + uuid.NewString()
		_, _, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)

		n, err := sysRepo.ExpireStaleInProgress(ctx, time.Hour, 100)
		require.NoError(t, err)
		// Only rows older than 1 hour should be expired; this row was just inserted.
		var status string
		require.NoError(t, rawPool.QueryRow(ctx,
			`SELECT status FROM provisioning_operations WHERE idempotency_key = $1`, key).Scan(&status))
		assert.Equal(t, "in_progress", status, "a recently inserted row must not be expired")
		_ = n
	})

	t.Run("DoesNotExpireSucceededRows", func(t *testing.T) {
		key := "expire-succeeded-" + uuid.NewString()
		_, _, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
		require.NoError(t, err)
		require.NoError(t, repo.Complete(tctx, key, nil))
		// Backdate to ensure it would be expired if it were in_progress.
		_, err = rawPool.Exec(ctx,
			`UPDATE provisioning_operations SET created_at = now() - interval '2 hours' WHERE idempotency_key = $1`, key)
		require.NoError(t, err)

		_, err = sysRepo.ExpireStaleInProgress(ctx, time.Hour, 100)
		require.NoError(t, err)

		var status string
		require.NoError(t, rawPool.QueryRow(ctx,
			`SELECT status FROM provisioning_operations WHERE idempotency_key = $1`, key).Scan(&status))
		assert.Equal(t, "succeeded", status, "a succeeded row must never be expired regardless of age")
	})

	t.Run("RespectsLimit", func(t *testing.T) {
		// Insert 3 stale rows; expire with limit=2.
		for i := 0; i < 3; i++ {
			key := "expire-limit-" + uuid.NewString()
			_, _, err := repo.Begin(tctx, key, tenantID, domain.OpProvisionTrial)
			require.NoError(t, err)
			_, err = rawPool.Exec(ctx,
				`UPDATE provisioning_operations SET created_at = now() - interval '2 hours' WHERE idempotency_key = $1`, key)
			require.NoError(t, err)
		}

		n, err := sysRepo.ExpireStaleInProgress(ctx, time.Hour, 2)
		require.NoError(t, err)
		assert.Equal(t, 2, n, "must expire at most limit rows per call")
	})
}
