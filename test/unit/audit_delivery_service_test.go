package unit_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

func pendingEntries(n int) []*domain.PendingAuditEntry {
	out := make([]*domain.PendingAuditEntry, n)
	for i := range out {
		out[i] = &domain.PendingAuditEntry{ID: uuid.New(), TenantID: uuid.New(), Attempts: 1,
			Entry: domain.AuditEntry{EntryType: domain.AuditEntryTypeIdpChanged}}
	}
	return out
}

func TestAuditDelivery_RoutesEachOutcome(t *testing.T) {
	due := pendingEntries(3)
	repo := &mockPendingAuditEntryRepository{Due: due}
	client := &mockAuditLogClient{Errs: []error{
		nil,
		fmt.Errorf("%w: 422", port.ErrAuditEntryRejected),
		errors.New("connection refused"),
	}}
	svc := service.NewAuditDeliveryService(repo, client)

	n, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []uuid.UUID{due[0].ID}, repo.Delivered)
	assert.Equal(t, []uuid.UUID{due[1].ID}, repo.Failed)
	assert.Equal(t, []uuid.UUID{due[2].ID}, repo.Retried)
}

// The row id is the AL-5 Idempotency-Key, so a redelivery after a lost
// response is deduplicated by Audit Log.
func TestAuditDelivery_UsesRowIDAsIdempotencyKey(t *testing.T) {
	due := pendingEntries(2)
	client := &mockAuditLogClient{}
	svc := service.NewAuditDeliveryService(&mockPendingAuditEntryRepository{Due: due}, client)

	_, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, []string{due[0].ID.String(), due[1].ID.String()}, client.Keys)
}

func TestAuditDelivery_ClaimFailurePropagates(t *testing.T) {
	claimErr := errors.New("db down")
	svc := service.NewAuditDeliveryService(&mockPendingAuditEntryRepository{ClaimErr: claimErr}, &mockAuditLogClient{})
	_, err := svc.DrainOnce(context.Background(), 10)
	require.ErrorIs(t, err, claimErr)
}

func TestAuditDelivery_MarkFailuresAreLoggedNotPropagated(t *testing.T) {
	repo := &mockPendingAuditEntryRepository{Due: pendingEntries(3), MarkErr: errors.New("db down")}
	client := &mockAuditLogClient{Errs: []error{nil, port.ErrAuditEntryRejected, errors.New("timeout")}}
	svc := service.NewAuditDeliveryService(repo, client).WithLogger(&mockLogger{})

	n, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestAuditDelivery_EmptyBacklog(t *testing.T) {
	client := &mockAuditLogClient{}
	svc := service.NewAuditDeliveryService(&mockPendingAuditEntryRepository{}, client)
	n, err := svc.DrainOnce(context.Background(), 10)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, client.Keys)
}
