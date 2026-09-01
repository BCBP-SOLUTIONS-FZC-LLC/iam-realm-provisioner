package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// TestBillingConsumer_SuspendedTenant_ReEnablesRealm covers RP-EVT-7's main
// path: Billing's TenantReactivated re-enables a dedicated realm that had
// reached `suspended`.
func TestBillingConsumer_SuspendedTenant_ReEnablesRealm(t *testing.T) {
	tenantID := uuid.New()
	row := &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Status: domain.RealmStatusSuspended}
	repo := &fakeRealmRepo{findFn: func(_ context.Context, id uuid.UUID) (*domain.TenantRealm, error) {
		require.Equal(t, tenantID, id)
		return row, nil
	}}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	c := NewBillingConsumer(repo, kc, dedup, stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: tenantID, Source: "billing"})
	require.NoError(t, c.Handle(context.Background(), env))

	require.Equal(t, []string{"tenant-acme"}, kc.enableRealmCalls)
	require.Len(t, repo.updateCalls, 1)
	assert.Equal(t, domain.RealmStatusActive, repo.updateCalls[0].Status)
	assert.Nil(t, repo.updateCalls[0].DisabledAt)
	assert.Len(t, dedup.marked, 1)
}

// TestBillingConsumer_NonSuspendedTenant_NoOpButStillRecorded covers the
// "never reached suspended" branch — no Keycloak effect, no Update, but the
// event is still marked processed (§7.1).
func TestBillingConsumer_NonSuspendedTenant_NoOpButStillRecorded(t *testing.T) {
	tenantID := uuid.New()
	row := &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Status: domain.RealmStatusActive}
	repo := &fakeRealmRepo{findFn: func(context.Context, uuid.UUID) (*domain.TenantRealm, error) { return row, nil }}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	c := NewBillingConsumer(repo, kc, dedup, stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: tenantID, Source: "billing"})
	require.NoError(t, c.Handle(context.Background(), env))

	assert.Empty(t, kc.enableRealmCalls, "a non-suspended tenant must not trigger a Keycloak realm enable")
	assert.Empty(t, repo.updateCalls, "a non-suspended tenant must not be written back to tenant_realms")
	assert.Len(t, dedup.marked, 1, "the event must still be recorded as processed even as a no-op")
}

// TestBillingConsumer_Redelivery_IsNoOp confirms a redelivered message
// (identical envelope ID) short-circuits before touching Keycloak or the
// repository a second time.
func TestBillingConsumer_Redelivery_IsNoOp(t *testing.T) {
	tenantID := uuid.New()
	row := &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Status: domain.RealmStatusSuspended}
	repo := &fakeRealmRepo{findFn: func(context.Context, uuid.UUID) (*domain.TenantRealm, error) { return row, nil }}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	c := NewBillingConsumer(repo, kc, dedup, stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: tenantID, Source: "billing"})
	require.NoError(t, c.Handle(context.Background(), env))
	require.NoError(t, c.Handle(context.Background(), env))

	assert.Len(t, kc.enableRealmCalls, 1, "a redelivered event must not re-execute the Keycloak effect")
	assert.Len(t, repo.updateCalls, 1)
}

func TestBillingConsumer_UnknownEventType_Acknowledged(t *testing.T) {
	repo := &fakeRealmRepo{findFn: func(context.Context, uuid.UUID) (*domain.TenantRealm, error) {
		t.Fatal("FindByTenantID must not be called for an unhandled event type")
		return nil, nil
	}}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	c := NewBillingConsumer(repo, kc, dedup, stubLogger{})

	env := envelope(t, "TenantSuspended", map[string]any{})
	require.NoError(t, c.Handle(context.Background(), env))
	assert.Empty(t, kc.enableRealmCalls)
}

func TestBillingConsumer_IsProcessedErrorPropagates(t *testing.T) {
	repo := &fakeRealmRepo{}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	dedup.isProcessedErr = errors.New("dedup store unreachable")
	c := NewBillingConsumer(repo, kc, dedup, stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: uuid.New()})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "check processed_events")
}

func TestBillingConsumer_MalformedPayload_DecodeErrorPropagates(t *testing.T) {
	repo := &fakeRealmRepo{}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	c := NewBillingConsumer(repo, kc, dedup, stubLogger{})

	env := events.NewEnvelope(domain.EventTenantReactivated, "test", json.RawMessage(`{"tenant_id":"not-a-uuid"}`))
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode "+domain.EventTenantReactivated)
}

func TestBillingConsumer_FindByTenantIDErrorPropagates(t *testing.T) {
	wantErr := errors.New("row not found")
	repo := &fakeRealmRepo{findFn: func(context.Context, uuid.UUID) (*domain.TenantRealm, error) { return nil, wantErr }}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	c := NewBillingConsumer(repo, kc, dedup, stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: uuid.New()})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Empty(t, dedup.marked)
}

func TestBillingConsumer_EnableRealmErrorPropagates(t *testing.T) {
	row := &domain.TenantRealm{RealmName: "tenant-acme", Status: domain.RealmStatusSuspended}
	repo := &fakeRealmRepo{findFn: func(context.Context, uuid.UUID) (*domain.TenantRealm, error) { return row, nil }}
	kc := &fakeKeycloakAdmin{enableRealmErr: errors.New("keycloak unavailable")}
	dedup := newStubDedup()
	c := NewBillingConsumer(repo, kc, dedup, stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: uuid.New()})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enable realm")
	assert.Empty(t, repo.updateCalls)
	assert.Empty(t, dedup.marked)
}

func TestBillingConsumer_UpdateErrorPropagates(t *testing.T) {
	row := &domain.TenantRealm{RealmName: "tenant-acme", Status: domain.RealmStatusSuspended}
	repo := &fakeRealmRepo{
		findFn:    func(context.Context, uuid.UUID) (*domain.TenantRealm, error) { return row, nil },
		updateErr: errors.New("optimistic lock conflict"),
	}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	c := NewBillingConsumer(repo, kc, dedup, stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: uuid.New()})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "update tenant_realms")
	assert.Empty(t, dedup.marked)
}

func TestBillingConsumer_MarkProcessedErrorPropagates(t *testing.T) {
	row := &domain.TenantRealm{RealmName: "tenant-acme", Status: domain.RealmStatusActive}
	repo := &fakeRealmRepo{findFn: func(context.Context, uuid.UUID) (*domain.TenantRealm, error) { return row, nil }}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	dedup.markProcessErr = errors.New("dedup write failed")
	c := NewBillingConsumer(repo, kc, dedup, stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: uuid.New()})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mark processed")
}
