package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

func TestTenantConsumer_TrialStarted_UpsertsTrialEndsAtOnly(t *testing.T) {
	tenantID := uuid.New()
	existing := &domain.TenantRealm{TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusActive}
	repo := &fakeRealmRepo{findFn: func(_ context.Context, id uuid.UUID) (*domain.TenantRealm, error) {
		require.Equal(t, tenantID, id)
		return existing, nil
	}}
	dedup := newStubDedup()
	c := NewTenantConsumer(repo, dedup, stubLogger{})

	wantEndsAt := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	env := envelope(t, domain.EventTrialStarted, domain.TrialStartedPayload{
		TenantID: tenantID, Plan: "starter", TrialEndsAt: wantEndsAt.Format(time.RFC3339),
	})

	require.NoError(t, c.Handle(context.Background(), env))
	require.Len(t, repo.updateCalls, 1)
	require.NotNil(t, repo.updateCalls[0].TrialEndsAt)
	assert.True(t, wantEndsAt.Equal(*repo.updateCalls[0].TrialEndsAt))
	assert.Len(t, dedup.marked, 1)
}

// TestTenantConsumer_Redelivery_IsIdempotentNotDoubleWrite confirms a
// replayed TrialStarted (same envelope ID) short-circuits on the second
// Handle call rather than issuing a second Update — per the consumer-dedup
// contract, redelivery must be a no-op, not a second write.
func TestTenantConsumer_Redelivery_IsIdempotentNotDoubleWrite(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRealmRepo{findFn: func(_ context.Context, id uuid.UUID) (*domain.TenantRealm, error) {
		return &domain.TenantRealm{TenantID: tenantID, Strategy: domain.RealmStrategyShared, Status: domain.RealmStatusActive}, nil
	}}
	dedup := newStubDedup()
	c := NewTenantConsumer(repo, dedup, stubLogger{})

	env := envelope(t, domain.EventTrialStarted, domain.TrialStartedPayload{
		TenantID: tenantID, Plan: "starter", TrialEndsAt: time.Now().UTC().Format(time.RFC3339),
	})

	require.NoError(t, c.Handle(context.Background(), env))
	require.NoError(t, c.Handle(context.Background(), env))
	assert.Len(t, repo.updateCalls, 1, "a redelivered TrialStarted must not double-write trial_ends_at")
}

func TestTenantConsumer_UnknownEventType_Acknowledged(t *testing.T) {
	repo := &fakeRealmRepo{findFn: func(context.Context, uuid.UUID) (*domain.TenantRealm, error) {
		t.Fatal("FindByTenantID must not be called for an unhandled event type")
		return nil, nil
	}}
	dedup := newStubDedup()
	c := NewTenantConsumer(repo, dedup, stubLogger{})

	env := envelope(t, "TrialExpired", map[string]any{})
	require.NoError(t, c.Handle(context.Background(), env))
	assert.Empty(t, repo.updateCalls)
}

func TestTenantConsumer_IsProcessedErrorPropagates(t *testing.T) {
	repo := &fakeRealmRepo{}
	dedup := newStubDedup()
	dedup.isProcessedErr = errors.New("dedup store unreachable")
	c := NewTenantConsumer(repo, dedup, stubLogger{})

	env := envelope(t, domain.EventTrialStarted, domain.TrialStartedPayload{TenantID: uuid.New(), TrialEndsAt: time.Now().Format(time.RFC3339)})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "check processed_events")
}

func TestTenantConsumer_MalformedPayload_DecodeErrorPropagates(t *testing.T) {
	repo := &fakeRealmRepo{}
	dedup := newStubDedup()
	c := NewTenantConsumer(repo, dedup, stubLogger{})

	env := events.NewEnvelope(domain.EventTrialStarted, "test", json.RawMessage(`{"tenant_id":"not-a-uuid"}`))
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode "+domain.EventTrialStarted)
}

func TestTenantConsumer_InvalidTrialEndsAtFormat_ParseErrorPropagates(t *testing.T) {
	repo := &fakeRealmRepo{}
	dedup := newStubDedup()
	c := NewTenantConsumer(repo, dedup, stubLogger{})

	env := envelope(t, domain.EventTrialStarted, domain.TrialStartedPayload{TenantID: uuid.New(), TrialEndsAt: "not-a-timestamp"})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse trial_ends_at")
}

func TestTenantConsumer_FindByTenantIDErrorPropagates(t *testing.T) {
	wantErr := errors.New("row not found")
	repo := &fakeRealmRepo{findFn: func(context.Context, uuid.UUID) (*domain.TenantRealm, error) { return nil, wantErr }}
	dedup := newStubDedup()
	c := NewTenantConsumer(repo, dedup, stubLogger{})

	env := envelope(t, domain.EventTrialStarted, domain.TrialStartedPayload{TenantID: uuid.New(), TrialEndsAt: time.Now().Format(time.RFC3339)})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Empty(t, dedup.marked)
}

func TestTenantConsumer_UpdateErrorPropagates(t *testing.T) {
	repo := &fakeRealmRepo{
		findFn:    func(context.Context, uuid.UUID) (*domain.TenantRealm, error) { return &domain.TenantRealm{}, nil },
		updateErr: errors.New("optimistic lock conflict"),
	}
	dedup := newStubDedup()
	c := NewTenantConsumer(repo, dedup, stubLogger{})

	env := envelope(t, domain.EventTrialStarted, domain.TrialStartedPayload{TenantID: uuid.New(), TrialEndsAt: time.Now().Format(time.RFC3339)})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "update trial_ends_at")
	assert.Empty(t, dedup.marked)
}

func TestTenantConsumer_MarkProcessedErrorPropagates(t *testing.T) {
	repo := &fakeRealmRepo{findFn: func(context.Context, uuid.UUID) (*domain.TenantRealm, error) { return &domain.TenantRealm{}, nil }}
	dedup := newStubDedup()
	dedup.markProcessErr = errors.New("dedup write failed")
	c := NewTenantConsumer(repo, dedup, stubLogger{})

	env := envelope(t, domain.EventTrialStarted, domain.TrialStartedPayload{TenantID: uuid.New(), TrialEndsAt: time.Now().Format(time.RFC3339)})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mark processed")
}
