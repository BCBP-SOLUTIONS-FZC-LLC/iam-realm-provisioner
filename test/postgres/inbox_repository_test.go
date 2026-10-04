//go:build integration

// Package postgres_test covers InboxRepository (§4.5, §7) — this service's
// port.Inbox / port.InboxPruner over platform-events' inbox.Store — against
// the RLS-exempt processed_events ledger: the claim-plus-fn transaction
// (exactly-once, all-or-nothing), the per-consumer composite key, the
// IsProcessed pre-check, and Prune's per-consumer retention sweep.
package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInboxRepository(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	inbox := pgadapter.NewInboxRepository(appPool, nil)
	noop := func(context.Context) error { return nil }

	t.Run("IsProcessed_NotYetClaimed_ReturnsFalse", func(t *testing.T) {
		t.Parallel()
		processed, err := inbox.IsProcessed(ctx, domain.ConsumerMembership, uuid.NewString())
		require.NoError(t, err)
		assert.False(t, processed)
	})

	t.Run("ProcessOnce_RunsFnOnce_ThenDuplicate", func(t *testing.T) {
		t.Parallel()
		eventID := uuid.NewString()
		runs := 0
		fn := func(context.Context) error { runs++; return nil }

		dup, err := inbox.ProcessOnce(ctx, domain.ConsumerTenant, eventID, domain.EventTrialStarted, fn)
		require.NoError(t, err)
		assert.False(t, dup)
		dup, err = inbox.ProcessOnce(ctx, domain.ConsumerTenant, eventID, domain.EventTrialStarted, fn)
		require.NoError(t, err)
		assert.True(t, dup, "a redelivered event is a duplicate")
		assert.Equal(t, 1, runs, "fn must not run for a duplicate")

		processed, err := inbox.IsProcessed(ctx, domain.ConsumerTenant, eventID)
		require.NoError(t, err)
		assert.True(t, processed)
	})

	t.Run("ProcessOnce_FnError_RollsBackTheClaim", func(t *testing.T) {
		t.Parallel()
		eventID := uuid.NewString()
		boom := errors.New("boom: projection write failed")
		_, err := inbox.ProcessOnce(ctx, domain.ConsumerBilling, eventID, domain.EventTenantReactivated, func(context.Context) error { return boom })
		require.ErrorIs(t, err, boom)

		processed, err := inbox.IsProcessed(ctx, domain.ConsumerBilling, eventID)
		require.NoError(t, err)
		assert.False(t, processed, "a failed fn must leave the event unclaimed so SQS redelivers it")
	})

	// fn's repository writes join the claim's transaction (WithTx), bound to
	// the caller's tenant GUC — so a tenant-scoped projection write and the
	// claim commit or roll back together (IDEMP-2).
	t.Run("ProcessOnce_FnWritesJoinTheClaimTransaction", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		seedInboxTrialRealm(ctx, t, rawPool, tenantID)
		realms := pgadapter.NewTenantRealmRepository(appPool)
		tctx := withTenant(ctx, tenantID)
		ends := time.Now().Add(14 * 24 * time.Hour).UTC().Truncate(time.Second)

		boom := errors.New("boom after the write")
		_, err := inbox.ProcessOnce(tctx, domain.ConsumerTenant, uuid.NewString(), domain.EventTrialStarted, func(txCtx context.Context) error {
			row, err := realms.FindByTenantID(txCtx, tenantID)
			require.NoError(t, err)
			row.TrialEndsAt = &ends
			require.NoError(t, realms.Update(txCtx, row))
			return boom
		})
		require.ErrorIs(t, err, boom)
		row, err := realms.FindByTenantID(tctx, tenantID)
		require.NoError(t, err)
		assert.Nil(t, row.TrialEndsAt, "the projection write rolls back with the claim")

		_, err = inbox.ProcessOnce(tctx, domain.ConsumerTenant, uuid.NewString(), domain.EventTrialStarted, func(txCtx context.Context) error {
			row, err := realms.FindByTenantID(txCtx, tenantID)
			if err != nil {
				return err
			}
			row.TrialEndsAt = &ends
			return realms.Update(txCtx, row)
		})
		require.NoError(t, err)
		row, err = realms.FindByTenantID(tctx, tenantID)
		require.NoError(t, err)
		require.NotNil(t, row.TrialEndsAt)
		assert.True(t, ends.Equal(*row.TrialEndsAt))
	})

	t.Run("CompositeKey_SameEventIDDifferentConsumersAreIndependent", func(t *testing.T) {
		t.Parallel()
		eventID := uuid.NewString()
		_, err := inbox.ProcessOnce(ctx, domain.ConsumerMembership, eventID, "X", noop)
		require.NoError(t, err)

		dup, err := inbox.ProcessOnce(ctx, domain.ConsumerTenant, eventID, "X", noop)
		require.NoError(t, err)
		assert.False(t, dup, "the same event_id is dedup-tracked independently per consumer queue")
	})

	t.Run("CompositeKey_RawDuplicateInsertViolatesPK", func(t *testing.T) {
		t.Parallel()
		eventID := uuid.NewString()
		_, err := rawPool.Exec(ctx, `INSERT INTO processed_events (event_id, consumer) VALUES ($1, $2)`, eventID, "membership")
		require.NoError(t, err)
		_, err = rawPool.Exec(ctx, `INSERT INTO processed_events (event_id, consumer) VALUES ($1, $2)`, eventID, "membership")
		var pgErr *pgconn.PgError
		require.True(t, errors.As(err, &pgErr))
		assert.Equal(t, "23505", pgErr.Code, "inbox's ON CONFLICT DO NOTHING relies on exactly this PK")
		assert.Equal(t, "processed_events_pkey", pgErr.ConstraintName, "the constraint name inbox's own schema uses")
	})

	t.Run("NonUUIDEventID_Rejected", func(t *testing.T) {
		t.Parallel()
		_, err := inbox.IsProcessed(ctx, domain.ConsumerMembership, "not-a-uuid")
		require.Error(t, err)
		_, err = inbox.ProcessOnce(ctx, domain.ConsumerMembership, "not-a-uuid", "X", noop)
		require.Error(t, err)
	})
}

// Prune is per consumer and honors retention: only the named consumer's
// rows older than retention go. Its own container — it backdates rows.
func TestInboxRepository_Prune(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	inbox := pgadapter.NewInboxRepository(appPool, nil)

	old := uuid.NewString()
	oldOtherConsumer := uuid.NewString()
	fresh := uuid.NewString()
	for _, r := range []struct {
		id, consumer string
		age          string
	}{{old, "tenant", "10 days"}, {oldOtherConsumer, "billing", "10 days"}, {fresh, "tenant", "1 hour"}} {
		_, err := rawPool.Exec(ctx, `INSERT INTO processed_events (event_id, consumer, processed_at) VALUES ($1, $2, now() - $3::interval)`, r.id, r.consumer, r.age)
		require.NoError(t, err)
	}

	n, err := inbox.Prune(ctx, domain.ConsumerTenant, 8*24*time.Hour, 100)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	for id, want := range map[string]bool{old: false, oldOtherConsumer: true, fresh: true} {
		var exists bool
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM processed_events WHERE event_id = $1)`, id).Scan(&exists))
		assert.Equal(t, want, exists, id)
	}
}

func seedInboxTrialRealm(ctx context.Context, t *testing.T, rawPool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, tenantID uuid.UUID) {
	t.Helper()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO tenant_realms (tenant_id, realm_name, strategy, status)
		VALUES ($1, 'trial', 'shared', 'active')`, tenantID)
	require.NoError(t, err)
}

// A consumer handler that emits a follow-on event enqueues it through the
// publisher ProcessOnce injects, inside the same transaction as the dedup
// claim — so a rolled-back projection never leaves a claimed event behind,
// and a redelivery never enqueues the event twice.
func TestInboxRepository_ProcessOnce_InjectsTxBoundPublisher(t *testing.T) {
	t.Parallel()
	appPool, _ := setupTestDB(t)
	ctx := context.Background()
	pub := &fakeEventPublisher{}
	inbox := pgadapter.NewInboxRepository(appPool, pub)
	tenantID := uuid.New()
	eventID := uuid.NewString()

	emit := func(txCtx context.Context) error {
		_, inTx := pgadapter.TxFromContext(txCtx)
		require.True(t, inTx, "fn runs inside the claim transaction")
		p, ok := port.EventPublisherFromContext(txCtx)
		require.True(t, ok, "an inbox built with a publisher injects it")
		return p.Enqueue(txCtx, &domain.Event{Type: domain.EventTrialTenantProvisioned, TenantID: tenantID})
	}

	dup, err := inbox.ProcessOnce(ctx, domain.ConsumerTenant, eventID, domain.EventTrialStarted, emit)
	require.NoError(t, err)
	assert.False(t, dup)
	dup, err = inbox.ProcessOnce(ctx, domain.ConsumerTenant, eventID, domain.EventTrialStarted, emit)
	require.NoError(t, err)
	assert.True(t, dup)
	require.Len(t, pub.events, 1, "the redelivery is a duplicate and enqueues nothing")
	assert.Equal(t, tenantID, pub.events[0].TenantID)
}

// Every InboxRepository method surfaces a lost database as db_unavailable,
// never as "already processed" or a silent success.
func TestInboxRepository_ConnectivityFailure_WrapsAsDatabaseUnavailable(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	closingPool := setupSysPool(t, rawPool)
	inbox := pgadapter.NewInboxRepository(closingPool, nil)
	closingPool.Close()

	processed, err := inbox.IsProcessed(ctx, domain.ConsumerBilling, uuid.NewString())
	require.Error(t, err)
	assert.False(t, processed)
	assert.Contains(t, err.Error(), "database unavailable")

	ran := false
	dup, err := inbox.ProcessOnce(ctx, domain.ConsumerBilling, uuid.NewString(), domain.EventTrialStarted, func(context.Context) error { ran = true; return nil })
	require.Error(t, err)
	assert.False(t, dup)
	assert.False(t, ran)

	_, err = inbox.Prune(ctx, domain.ConsumerBilling, time.Hour, 10)
	require.Error(t, err)
}
