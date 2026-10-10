//go:build integration

// Package postgres_test covers InboxStoreAdapter (§4.5, §7) against the
// RLS-exempt `processed_events` dedup ledger: IsProcessed/MarkProcessed,
// the (event_id, consumer) composite-PK dedup (both via ON CONFLICT DO
// NOTHING and the raw unique-violation it backs), MarkProcessed joining an
// ambient transaction (IDEMP-2 — the consumers' path), and Prune's TTL sweep.
// All SQL is owned by platform-events inbox.Store; this test suite verifies
// the adapter's routing and ambient-tx integration.
package postgres_test

import (
	"context"
	"errors"
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/inbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessedEventsRepository(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()

	mustStore := func(consumer string) *inbox.Store {
		s, err := inbox.NewStore(appPool, consumer)
		require.NoError(t, err)
		return s
	}
	repo := pgadapter.NewInboxStoreAdapter(
		mustStore(string(domain.ConsumerMembership)),
		mustStore(string(domain.ConsumerTenant)),
		mustStore(string(domain.ConsumerBilling)),
	)

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

	// The two subtests below cover what the consumers actually call.
	// InboxStoreAdapter.MarkProcessed detects an ambient transaction via
	// TxFromContext and executes the INSERT on it, so the dedup mark commits
	// with the caller's projection write in one transaction (IDEMP-2).
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

	// Prune_HonoursBatchLimit and Prune_DeletesOnlyRowsOlderThanTTL both
	// sweep processed_events with unscoped criteria — each other's backdated
	// rows are indistinguishable, so they are not t.Parallel() and run
	// sequentially relative to each other.
	t.Run("Prune_DeletesOnlyRowsOlderThanTTL", func(t *testing.T) {
		oldEventID := uuid.NewString()
		recentEventID := uuid.NewString()
		_, err := rawPool.Exec(ctx, `INSERT INTO processed_events (event_id, consumer, processed_at) VALUES ($1, 'tenant', now() - interval '10 days')`, oldEventID)
		require.NoError(t, err)
		require.NoError(t, repo.MarkProcessed(ctx, domain.ConsumerTenant, recentEventID))

		deleted, err := repo.Prune(ctx, 7, 100)
		require.NoError(t, err)
		assert.Equal(t, 1, deleted)

		oldProcessed, err := repo.IsProcessed(ctx, domain.ConsumerTenant, oldEventID)
		require.NoError(t, err)
		assert.False(t, oldProcessed)

		recentProcessed, err := repo.IsProcessed(ctx, domain.ConsumerTenant, recentEventID)
		require.NoError(t, err)
		assert.True(t, recentProcessed, "a row inside the TTL window must survive the sweep")
	})

	// InboxStoreAdapter.Prune delegates to inbox.Store.Prune, which loops
	// internally until all eligible rows are deleted (limit is the per-DELETE
	// batch size, not the total cap per call). A single Prune call therefore
	// clears all eligible rows for a consumer; the reconciler job calls it
	// repeatedly only when needed, but convergence happens within one call.
	t.Run("Prune_DeletesAllEligibleRows", func(t *testing.T) {
		ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
		for _, id := range ids {
			_, err := rawPool.Exec(ctx, `INSERT INTO processed_events (event_id, consumer, processed_at) VALUES ($1, 'tenant', now() - interval '10 days')`, id)
			require.NoError(t, err)
		}

		// inbox.Store.Prune loops internally: limit=2 means each DELETE batch
		// is at most 2 rows, but the loop runs until no more rows match.
		// All 3 rows are deleted in one Prune call.
		deleted, err := repo.Prune(ctx, 7, 2)
		require.NoError(t, err)
		assert.Equal(t, 3, deleted, "all 3 eligible rows deleted in one call — inbox.Store.Prune loops until done")

		// Second call finds nothing — idempotent.
		deleted, err = repo.Prune(ctx, 7, 2)
		require.NoError(t, err)
		assert.Equal(t, 0, deleted)
	})
}
