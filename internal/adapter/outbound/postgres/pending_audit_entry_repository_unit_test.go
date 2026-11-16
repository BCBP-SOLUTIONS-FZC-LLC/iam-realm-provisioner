//go:build !integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// TestEnqueueOnce_EmptyKey_ReturnsError covers the idempotencyKey == ""
// guard that fires before any pool access, so a nil pool is safe here.
func TestEnqueueOnce_EmptyKey_ReturnsError(t *testing.T) {
	r := NewPendingAuditEntryRepository(nil)
	err := r.EnqueueOnce(context.Background(), "", domain.AuditEntry{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "idempotency")
}

// inbox.NewStore rejects a nil pool before making any network call, so
// NewInboxRepository(nil, nil) causes r.store() to return an error on the
// very first call. This covers the store-error branches in IsProcessed,
// ProcessOnce, and Prune (lines 48, 62, 85 of inbox_repository.go) and
// the NewStore error return in store() itself (line 99).
func TestInboxRepository_NilPool_CausesStoreError(t *testing.T) {
	r := NewInboxRepository(nil, nil) // nil pool → inbox.NewStore returns error
	ctx := context.Background()
	consumer := domain.ConsumerMembership

	t.Run("IsProcessed", func(t *testing.T) {
		_, err := r.IsProcessed(ctx, consumer, uuid.NewString())
		require.Error(t, err, "nil pool must propagate inbox.NewStore's error from store()")
	})

	t.Run("ProcessOnce", func(t *testing.T) {
		_, err := r.ProcessOnce(ctx, consumer, uuid.NewString(), "op", func(context.Context) error { return nil })
		require.Error(t, err)
	})

	t.Run("Prune", func(t *testing.T) {
		_, err := r.Prune(ctx, consumer, 24*time.Hour, 100)
		require.Error(t, err)
	})
}
