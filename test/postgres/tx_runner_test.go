//go:build integration

// Package postgres_test covers TxRunner (db.go) — the port.TxRunner
// implementation every service method calls to open a transaction, join
// the tx-bound outbox publisher into ctx, and commit/rollback atomically
// with the business write (RP-INV-3, §9.1).
package postgres_test

import (
	"context"
	"errors"
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeEventPublisher struct {
	events []*domain.Event
}

func (f *fakeEventPublisher) Enqueue(_ context.Context, tx pgx.Tx, event *domain.Event) error {
	if tx == nil {
		return errors.New("fakeEventPublisher: Enqueue called without an active tx")
	}
	f.events = append(f.events, event)
	return nil
}

func TestTxRunner(t *testing.T) {
	t.Parallel()
	appPool, _ := setupTestDB(t)
	ctx := context.Background()

	t.Run("RunInTx_CommitsAndInjectsTxBoundPublisher", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		pub := &fakeEventPublisher{}
		runner := pgadapter.NewTxRunner(appPool, pub)

		err := runner.RunInTx(withTenant(ctx, tenantID), func(txCtx context.Context) error {
			tx, ok := pgadapter.TxFromContext(txCtx)
			require.True(t, ok, "RunInTx must inject the active pgx.Tx into ctx")
			_, execErr := tx.Exec(txCtx, `
				INSERT INTO tenant_realms (tenant_id, realm_name, strategy, status)
				VALUES ($1, 'trial', 'shared', 'active')`, tenantID)
			if execErr != nil {
				return execErr
			}

			ctxPub, ok := port.EventPublisherFromContext(txCtx)
			require.True(t, ok, "RunInTx must inject a ContextEventPublisher when TxRunner was constructed with one")
			return ctxPub.EnqueueCtx(txCtx, &domain.Event{Type: domain.EventTrialTenantProvisioned, TenantID: tenantID})
		})
		require.NoError(t, err)

		require.Len(t, pub.events, 1)
		assert.Equal(t, tenantID, pub.events[0].TenantID)

		repo := pgadapter.NewTenantRealmRepository(appPool)
		got, err := repo.FindByTenantID(withTenant(ctx, tenantID), tenantID)
		require.NoError(t, err)
		assert.Equal(t, tenantID, got.TenantID)
	})

	t.Run("RunInTx_RollsBackOnError_NoEventAndNoRow", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		pub := &fakeEventPublisher{}
		runner := pgadapter.NewTxRunner(appPool, pub)
		errBoom := errors.New("boom: simulated business-rule failure")

		err := runner.RunInTx(withTenant(ctx, tenantID), func(txCtx context.Context) error {
			tx, _ := pgadapter.TxFromContext(txCtx)
			_, execErr := tx.Exec(txCtx, `
				INSERT INTO tenant_realms (tenant_id, realm_name, strategy, status)
				VALUES ($1, 'trial', 'shared', 'active')`, tenantID)
			if execErr != nil {
				return execErr
			}
			return errBoom
		})
		require.ErrorIs(t, err, errBoom)
		assert.Empty(t, pub.events)

		repo := pgadapter.NewTenantRealmRepository(appPool)
		_, err = repo.FindByTenantID(withTenant(ctx, tenantID), tenantID)
		require.Error(t, err)
		var derr *domain.Error
		require.True(t, errors.As(err, &derr))
		assert.Equal(t, domain.ErrTenantRealmNotFound, derr.Code, "a rolled-back transaction must leave no row behind")
	})

	t.Run("RunInTx_NilEventPublisher_LeavesNoPublisherInContext", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		runner := pgadapter.NewTxRunner(appPool, nil)

		err := runner.RunInTx(withTenant(ctx, tenantID), func(txCtx context.Context) error {
			_, ok := port.EventPublisherFromContext(txCtx)
			assert.False(t, ok, "a TxRunner built with a nil EventPublisher must not inject one")
			return nil
		})
		require.NoError(t, err)
	})

	t.Run("TxFromContext_AbsentWhenNoTxActive", func(t *testing.T) {
		t.Parallel()
		_, ok := pgadapter.TxFromContext(context.Background())
		assert.False(t, ok)
	})

	// withPool (db.go) has two branches: join an already-active tx found in
	// ctx, or open a fresh one via pgcommon.RunInTx. Every other test in this
	// package calls a repository method directly (no active tx in ctx), which
	// only ever exercises the "open fresh" branch. Calling a repository
	// method from inside a TxRunner.RunInTx callback — as the service layer
	// itself does for every Keycloak-effect-first flow (§9.1) — is what
	// exercises the "join" branch: TxFromContext must see the tx RunInTx
	// already injected and must not open a second, nested transaction.
	t.Run("RunInTx_RepositoryCallJoinsTheActiveTransaction", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		runner := pgadapter.NewTxRunner(appPool, nil)
		repo := pgadapter.NewTenantRealmRepository(appPool)

		err := runner.RunInTx(withTenant(ctx, tenantID), func(txCtx context.Context) error {
			return repo.Upsert(txCtx, newSharedTrial(tenantID))
		})
		require.NoError(t, err)

		got, err := repo.FindByTenantID(withTenant(ctx, tenantID), tenantID)
		require.NoError(t, err)
		assert.Equal(t, tenantID, got.TenantID)
	})

	t.Run("RunInTx_RepositoryCallJoinsTheActiveTransaction_RollsBackOnError", func(t *testing.T) {
		t.Parallel()
		tenantID := uuid.New()
		runner := pgadapter.NewTxRunner(appPool, nil)
		repo := pgadapter.NewTenantRealmRepository(appPool)
		errBoom := errors.New("boom: simulated failure after the joined-tx write")

		err := runner.RunInTx(withTenant(ctx, tenantID), func(txCtx context.Context) error {
			if err := repo.Upsert(txCtx, newSharedTrial(tenantID)); err != nil {
				return err
			}
			return errBoom
		})
		require.ErrorIs(t, err, errBoom)

		_, err = repo.FindByTenantID(withTenant(ctx, tenantID), tenantID)
		require.Error(t, err, "the joined-tx write must have rolled back along with the rest of the transaction")
	})
}
