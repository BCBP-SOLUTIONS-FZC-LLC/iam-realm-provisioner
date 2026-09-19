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
	source := domain.SuspensionSourceBillingLapse
	row := &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Status: domain.RealmStatusSuspended, SuspensionSource: &source}
	repo := &fakeRealmRepo{findFn: func(_ context.Context, id uuid.UUID) (*domain.TenantRealm, error) {
		require.Equal(t, tenantID, id)
		return row, nil
	}}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	c := NewBillingConsumer(repo, kc, dedup, newFakeTxRunner(repo, dedup), stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: tenantID, Source: "billing"})
	require.NoError(t, c.Handle(context.Background(), env))

	require.Equal(t, []string{"tenant-acme"}, kc.enableRealmCalls)
	require.Len(t, repo.updateCalls, 1)
	assert.Equal(t, domain.RealmStatusActive, repo.updateCalls[0].Status)
	assert.Nil(t, repo.updateCalls[0].DisabledAt)
	assert.Nil(t, repo.updateCalls[0].SuspensionSource, "a billing-driven reactivation must clear suspension_source, matching RP-10's own reactivate path")
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
	c := NewBillingConsumer(repo, kc, dedup, newFakeTxRunner(repo, dedup), stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: tenantID, Source: "billing"})
	require.NoError(t, c.Handle(context.Background(), env))

	assert.Empty(t, kc.enableRealmCalls, "a non-suspended tenant must not trigger a Keycloak realm enable")
	assert.Empty(t, repo.updateCalls, "a non-suspended tenant must not be written back to tenant_realms")
	assert.Len(t, dedup.marked, 1, "the event must still be recorded as processed even as a no-op")
}

// TestBillingConsumer_KeycloakRunsOutsideTheTransaction pins the §9.1 /
// CONS-2 ordering: the realm is re-enabled BEFORE the transaction opens, so
// a committed tenant_realms row always reflects real Keycloak state. The
// local write and the dedup mark then share that one transaction.
func TestBillingConsumer_KeycloakRunsOutsideTheTransaction(t *testing.T) {
	tenantID := uuid.New()
	row := &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Status: domain.RealmStatusSuspended}
	repo := &fakeRealmRepo{findFn: func(context.Context, uuid.UUID) (*domain.TenantRealm, error) { return row, nil }}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	tx := newFakeTxRunner(repo, dedup)
	c := NewBillingConsumer(repo, kc, dedup, tx, stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: tenantID, Source: "billing"})
	require.NoError(t, c.Handle(context.Background(), env))

	assert.Len(t, kc.enableRealmCalls, 1)
	assert.Equal(t, 1, tx.calls, "exactly one transaction, opened after the Keycloak call")
}

// TestBillingConsumer_KeycloakFailure_OpensNoTransaction is the same
// ordering seen from the failure side: if Keycloak rejects the enable there
// must be no local trace at all, not even an opened transaction.
func TestBillingConsumer_KeycloakFailure_OpensNoTransaction(t *testing.T) {
	tenantID := uuid.New()
	row := &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Status: domain.RealmStatusSuspended}
	repo := &fakeRealmRepo{findFn: func(context.Context, uuid.UUID) (*domain.TenantRealm, error) { return row, nil }}
	kc := &fakeKeycloakAdmin{enableRealmErr: errors.New("keycloak unavailable")}
	dedup := newStubDedup()
	tx := newFakeTxRunner(repo, dedup)
	c := NewBillingConsumer(repo, kc, dedup, tx, stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: tenantID, Source: "billing"})
	require.Error(t, c.Handle(context.Background(), env))

	assert.Zero(t, tx.calls, "a failed Keycloak effect must not open a transaction")
	assert.Empty(t, repo.updateCalls)
	assert.Empty(t, dedup.marked, "a failed effect must not be recorded as processed")
}

// TestBillingConsumer_MarkFailureRollsBackTheUpdate confirms the reactivation
// write and the dedup insert share one fate.
func TestBillingConsumer_MarkFailureRollsBackTheUpdate(t *testing.T) {
	tenantID := uuid.New()
	row := &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Status: domain.RealmStatusSuspended}
	repo := &fakeRealmRepo{findFn: func(context.Context, uuid.UUID) (*domain.TenantRealm, error) { return row, nil }}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	dedup.markProcessErr = errors.New("dedup write failed")
	c := NewBillingConsumer(repo, kc, dedup, newFakeTxRunner(repo, dedup), stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: tenantID, Source: "billing"})
	require.Error(t, c.Handle(context.Background(), env))

	assert.Empty(t, repo.updateCalls, "the reactivation write must roll back with the failed dedup insert")
	assert.Empty(t, dedup.marked)
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
	c := NewBillingConsumer(repo, kc, dedup, newFakeTxRunner(repo, dedup), stubLogger{})

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
	c := NewBillingConsumer(repo, kc, dedup, newFakeTxRunner(repo, dedup), stubLogger{})

	env := envelope(t, "TenantSuspended", map[string]any{})
	require.NoError(t, c.Handle(context.Background(), env))
	assert.Empty(t, kc.enableRealmCalls)
	assert.Len(t, dedup.marked, 1, "unknown types must still be recorded in processed_events so redelivery does not storm")
}

// An unknown event type still fails loudly if ackUnknown's own
// mark-processed write fails — the wrap-and-return branch in Handle's
// default case, distinct from the known-event-type mark-processed test
// below.
func TestBillingConsumer_UnknownEventType_MarkProcessedFailurePropagates(t *testing.T) {
	repo := &fakeRealmRepo{}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	dedup.markProcessErr = errors.New("dedup write failed")
	c := NewBillingConsumer(repo, kc, dedup, newFakeTxRunner(repo, dedup), stubLogger{})

	env := envelope(t, "TenantSuspended", map[string]any{})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "billing consumer")
}

func TestBillingConsumer_IsProcessedErrorPropagates(t *testing.T) {
	repo := &fakeRealmRepo{}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	dedup.isProcessedErr = errors.New("dedup store unreachable")
	c := NewBillingConsumer(repo, kc, dedup, newFakeTxRunner(repo, dedup), stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: uuid.New()})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "check processed_events")
}

func TestBillingConsumer_MalformedPayload_DecodeErrorPropagates(t *testing.T) {
	repo := &fakeRealmRepo{}
	kc := &fakeKeycloakAdmin{}
	dedup := newStubDedup()
	c := NewBillingConsumer(repo, kc, dedup, newFakeTxRunner(repo, dedup), stubLogger{})

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
	c := NewBillingConsumer(repo, kc, dedup, newFakeTxRunner(repo, dedup), stubLogger{})

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
	c := NewBillingConsumer(repo, kc, dedup, newFakeTxRunner(repo, dedup), stubLogger{})

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
	c := NewBillingConsumer(repo, kc, dedup, newFakeTxRunner(repo, dedup), stubLogger{})

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
	c := NewBillingConsumer(repo, kc, dedup, newFakeTxRunner(repo, dedup), stubLogger{})

	env := envelope(t, domain.EventTenantReactivated, domain.BillingTenantReactivatedPayload{TenantID: uuid.New()})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mark processed")
}
