//go:build integration

// Package postgres_test covers ProcessedEventsRepository (§4.5, §7) against
// the RLS-exempt `processed_events` dedup ledger: IsProcessed/MarkProcessed,
// the (event_id, consumer) composite-PK dedup (both via ON CONFLICT DO
// NOTHING and the raw unique-violation it backs), MarkProcessed joining an
// ambient transaction (the consumers' path) as well as MarkProcessedInTx
// joining an explicitly passed one, and CleanupExpired's TTL sweep.
package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessedEventsRepository(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewProcessedEventsRepository(appPool)

	t.Run("IsProcessed_NotYetMarked_ReturnsFalse", func(t *testing.T) {
		t.Parallel()
		processed, err := repo.IsProcessed(ctx, domain.ConsumerMembership, uuid.NewString())
		require.NoError(t, err)
		assert.False(t, processed)
	})

	t.Run("MarkProcessed_ThenIsProcessed_ReturnsTrue", func(t *testing.T) {
		t.Parallel()
		eventID := uuid.NewString()
		require.NoError(t, repo.MarkProcessed(ctx, domain.ConsumerTenant, eventID))

		processed, err := repo.IsProcessed(ctx, domain.ConsumerTenant, eventID)
		require.NoError(t, err)
		assert.True(t, processed)
	})

	t.Run("MarkProcessed_IsIdempotent", func(t *testing.T) {
		t.Parallel()
		eventID := uuid.NewString()
		require.NoError(t, repo.MarkProcessed(ctx, domain.ConsumerBilling, eventID))
		require.NoError(t, repo.MarkProcessed(ctx, domain.ConsumerBilling, eventID))

		var count int
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT count(*) FROM processed_events WHERE event_id = $1 AND consumer = $2`,
			eventID, string(domain.ConsumerBilling)).Scan(&count))
		assert.Equal(t, 1, count, "a redelivered SQS message must not create a second row")
	})

	t.Run("CompositePK_SameEventIDDifferentConsumersAreIndependent", func(t *testing.T) {
		t.Parallel()
		eventID := uuid.NewString()
		require.NoError(t, repo.MarkProcessed(ctx, domain.ConsumerMembership, eventID))

		processedForMembership, err := repo.IsProcessed(ctx, domain.ConsumerMembership, eventID)
		require.NoError(t, err)
		assert.True(t, processedForMembership)

		processedForTenant, err := repo.IsProcessed(ctx, domain.ConsumerTenant, eventID)
		require.NoError(t, err)
		assert.False(t, processedForTenant, "the same event_id must be independently dedup-tracked per consumer queue")
	})

	t.Run("CompositePK_RawDuplicateInsertViolatesUniqueConstraint", func(t *testing.T) {
		t.Parallel()
		eventID := uuid.NewString()
		_, err := rawPool.Exec(ctx, `INSERT INTO processed_events (event_id, consumer) VALUES ($1, $2)`, eventID, "membership")
		require.NoError(t, err)

		_, err = rawPool.Exec(ctx, `INSERT INTO processed_events (event_id, consumer) VALUES ($1, $2)`, eventID, "membership")
		require.Error(t, err)
		var pgErr *pgconn.PgError
		require.True(t, errors.As(err, &pgErr))
		assert.Equal(t, "23505", pgErr.Code, "MarkProcessed's ON CONFLICT DO NOTHING relies on exactly this PK")
	})

	t.Run("MarkProcessedInTx_CommitsAtomicallyWithCaller", func(t *testing.T) {
		t.Parallel()
		eventID := uuid.NewString()
		err := pgcommon.RunInTx(ctx, appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			return repo.MarkProcessedInTx(ctx, tx, domain.ConsumerMembership, eventID)
		})
		require.NoError(t, err)

		processed, err := repo.IsProcessed(ctx, domain.ConsumerMembership, eventID)
		require.NoError(t, err)
		assert.True(t, processed)
	})

	t.Run("MarkProcessedInTx_RolledBackWithCallerNeverCommits", func(t *testing.T) {
		t.Parallel()
		eventID := uuid.NewString()
		errBoom := errors.New("boom: caller's projection update failed")
		err := pgcommon.RunInTx(ctx, appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			if err := repo.MarkProcessedInTx(ctx, tx, domain.ConsumerMembership, eventID); err != nil {
				return err
			}
			return errBoom
		})
		require.ErrorIs(t, err, errBoom)

		processed, err := repo.IsProcessed(ctx, domain.ConsumerMembership, eventID)
		require.NoError(t, err)
		assert.False(t, processed, "the dedup write must roll back with the rest of the caller's transaction")
	})

	// The two subtests below cover what the consumers actually call.
	// MarkProcessed goes through withPool, so it joins an ambient
	// transaction when one is bound into ctx and opens its own when not —
	// that is what makes TenantConsumer/BillingConsumer's projection write
	// and dedup mark commit as a unit (§7.1, IDEMP-2) without those
	// consumers ever handling a pgx.Tx.
	t.Run("MarkProcessed_JoinsAnAmbientTransaction", func(t *testing.T) {
		t.Parallel()
		eventID := uuid.NewString()
		txRunner := pgadapter.NewTxRunner(appPool, nil)
		require.NoError(t, txRunner.RunInTx(ctx, func(txCtx context.Context) error {
			return repo.MarkProcessed(txCtx, domain.ConsumerTenant, eventID)
		}))

		processed, err := repo.IsProcessed(ctx, domain.ConsumerTenant, eventID)
		require.NoError(t, err)
		assert.True(t, processed)
	})

	t.Run("MarkProcessed_RollsBackWithTheAmbientTransaction", func(t *testing.T) {
		t.Parallel()
		eventID := uuid.NewString()
		errBoom := errors.New("boom: caller's projection update failed")
		txRunner := pgadapter.NewTxRunner(appPool, nil)
		err := txRunner.RunInTx(ctx, func(txCtx context.Context) error {
			if err := repo.MarkProcessed(txCtx, domain.ConsumerTenant, eventID); err != nil {
				return err
			}
			return errBoom
		})
		require.ErrorIs(t, err, errBoom)

		processed, err := repo.IsProcessed(ctx, domain.ConsumerTenant, eventID)
		require.NoError(t, err)
		assert.False(t, processed,
			"a consumer whose projection write fails must not leave the event marked processed — redelivery has to retry it")
	})

	// CleanupExpired_DeletesOnlyRowsOlderThanTTL and Prune_HonoursBatchLimit
	// both sweep processed_events (an RLS-exempt, non-tenant-scoped global
	// table, §4.5) with unscoped criteria — each other's backdated rows are
	// indistinguishable from its own, so running them as parallel siblings
	// races: one test's sweep can consume rows the other just inserted,
	// producing a flaky deleted-count mismatch. Neither is t.Parallel(),
	// so they run sequentially relative to each other before any of this
	// parent's other (genuinely independent) parallel subtests start.
	t.Run("CleanupExpired_DeletesOnlyRowsOlderThanTTL", func(t *testing.T) {
		oldEventID := uuid.NewString()
		recentEventID := uuid.NewString()
		_, err := rawPool.Exec(ctx, `INSERT INTO processed_events (event_id, consumer, processed_at) VALUES ($1, 'tenant', now() - interval '10 days')`, oldEventID)
		require.NoError(t, err)
		require.NoError(t, repo.MarkProcessed(ctx, domain.ConsumerTenant, recentEventID))

		deleted, err := repo.CleanupExpired(ctx, 7*24*time.Hour)
		require.NoError(t, err)
		assert.Equal(t, int64(1), deleted)

		oldProcessed, err := repo.IsProcessed(ctx, domain.ConsumerTenant, oldEventID)
		require.NoError(t, err)
		assert.False(t, oldProcessed)

		recentProcessed, err := repo.IsProcessed(ctx, domain.ConsumerTenant, recentEventID)
		require.NoError(t, err)
		assert.True(t, recentProcessed, "a row inside the TTL window must survive the sweep")
	})

	t.Run("Prune_HonoursBatchLimit", func(t *testing.T) {
		ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
		for _, id := range ids {
			_, err := rawPool.Exec(ctx, `INSERT INTO processed_events (event_id, consumer, processed_at) VALUES ($1, 'tenant', now() - interval '10 days')`, id)
			require.NoError(t, err)
		}

		deleted, err := repo.Prune(ctx, 7, 2)
		require.NoError(t, err)
		assert.Equal(t, 2, deleted)

		deleted, err = repo.Prune(ctx, 7, 2)
		require.NoError(t, err)
		assert.Equal(t, 1, deleted)
	})
}
