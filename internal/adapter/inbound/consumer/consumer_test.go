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
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// ── test doubles ──────────────────────────────────────────────────────────

type stubLogger struct{}

func (stubLogger) Debug(string, map[string]any) {}
func (stubLogger) Info(string, map[string]any)  {}
func (stubLogger) Warn(string, map[string]any)  {}
func (stubLogger) Error(string, map[string]any) {}

type stubDedup struct {
	processed      map[string]bool
	marked         []string
	isProcessedErr error
	markProcessErr error
}

func newStubDedup() *stubDedup { return &stubDedup{processed: map[string]bool{}} }

func (d *stubDedup) IsProcessed(ctx context.Context, consumer domain.ProcessedEventsConsumer, eventID string) (bool, error) {
	if d.isProcessedErr != nil {
		return false, d.isProcessedErr
	}
	return d.processed[string(consumer)+":"+eventID], nil
}
func (d *stubDedup) MarkProcessed(ctx context.Context, consumer domain.ProcessedEventsConsumer, eventID string) error {
	if d.markProcessErr != nil {
		return d.markProcessErr
	}
	d.processed[string(consumer)+":"+eventID] = true
	d.marked = append(d.marked, eventID)
	return nil
}

func (d *stubDedup) Prune(context.Context, int, int) (int, error) { return 0, nil }

// fakeTxRunner stands in for postgres.TxRunner. It models the one property
// the consumers depend on: all-or-nothing. On a non-nil error from fn it
// truncates whatever the repo and dedup stubs recorded during the call, so a
// test can assert that a failed projection write also discards the
// processed_events mark instead of leaving the event marked as handled.
type fakeTxRunner struct {
	repo  *fakeRealmRepo
	dedup *stubDedup
	calls int
}

func newFakeTxRunner(repo *fakeRealmRepo, dedup *stubDedup) *fakeTxRunner {
	return &fakeTxRunner{repo: repo, dedup: dedup}
}

func (f *fakeTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	f.calls++
	updatesAtStart := 0
	if f.repo != nil {
		updatesAtStart = len(f.repo.updateCalls)
	}
	markedAtStart := len(f.dedup.marked)

	err := fn(ctx)
	if err != nil {
		if f.repo != nil {
			f.repo.updateCalls = f.repo.updateCalls[:updatesAtStart]
		}
		for _, id := range f.dedup.marked[markedAtStart:] {
			delete(f.dedup.processed, string(domain.ConsumerTenant)+":"+id)
			delete(f.dedup.processed, string(domain.ConsumerBilling)+":"+id)
			delete(f.dedup.processed, string(domain.ConsumerMembership)+":"+id)
		}
		f.dedup.marked = f.dedup.marked[:markedAtStart]
	}
	return err
}

type stubMfaSyncer struct {
	calls []struct {
		tenantID, userID uuid.UUID
		grant            bool
	}
	err error
}

func (s *stubMfaSyncer) SyncRequiresMfa(ctx context.Context, tenantID, userID uuid.UUID, grant bool) error {
	s.calls = append(s.calls, struct {
		tenantID, userID uuid.UUID
		grant            bool
	}{tenantID, userID, grant})
	return s.err
}

// fakeRealmRepo embeds the port.TenantRealmRepository interface (nil by
// default — any method beyond FindByTenantID/Update panics if called,
// which is intentional: tenant_consumer.go and billing_consumer.go only
// ever call those two).
type fakeRealmRepo struct {
	port.TenantRealmRepository
	findFn      func(ctx context.Context, tenantID uuid.UUID) (*domain.TenantRealm, error)
	updateErr   error
	updateCalls []*domain.TenantRealm
}

func (f *fakeRealmRepo) FindByTenantID(ctx context.Context, tenantID uuid.UUID) (*domain.TenantRealm, error) {
	return f.findFn(ctx, tenantID)
}

func (f *fakeRealmRepo) Update(ctx context.Context, realm *domain.TenantRealm) error {
	f.updateCalls = append(f.updateCalls, realm)
	return f.updateErr
}

// fakeKeycloakAdmin embeds port.KeycloakAdmin (nil by default) and
// overrides only EnableRealm — the single method billing_consumer.go
// calls.
type fakeKeycloakAdmin struct {
	port.KeycloakAdmin
	enableRealmCalls []string
	enableRealmErr   error
}

func (f *fakeKeycloakAdmin) EnableRealm(ctx context.Context, realmName string) error {
	f.enableRealmCalls = append(f.enableRealmCalls, realmName)
	return f.enableRealmErr
}

func envelope(t *testing.T, eventType string, payload any) events.Envelope[json.RawMessage] {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return events.NewEnvelope(eventType, "test", json.RawMessage(raw))
}

func newTestMembership(mfa *stubMfaSyncer, dedup *stubDedup) *MembershipConsumer {
	return NewMembershipConsumer(mfa, dedup, newFakeTxRunner(nil, dedup), stubLogger{})
}

// ── MembershipConsumer ────────────────────────────────────────────────────

func TestMembershipConsumer_DepartmentGrant_ApproverLevel_Assigns(t *testing.T) {
	mfa := &stubMfaSyncer{}
	dedup := newStubDedup()
	c := newTestMembership(mfa, dedup)

	tenantID, userID, deptID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	env := envelope(t, domain.EventDepartmentMembershipGranted, domain.DepartmentMembershipGrantedPayload{
		UserID: userID, TenantID: tenantID, DepartmentID: deptID, Level: "approver", ActorID: actorID,
	})

	require.NoError(t, c.Handle(context.Background(), env))
	require.Len(t, mfa.calls, 1)
	assert.True(t, mfa.calls[0].grant)
	assert.Equal(t, userID, mfa.calls[0].userID)
	assert.Len(t, dedup.marked, 1)
}

func TestMembershipConsumer_DepartmentGrant_PreparatorLevel_NoOp(t *testing.T) {
	mfa := &stubMfaSyncer{}
	dedup := newStubDedup()
	c := newTestMembership(mfa, dedup)

	env := envelope(t, domain.EventDepartmentMembershipGranted, domain.DepartmentMembershipGrantedPayload{
		UserID: uuid.New(), TenantID: uuid.New(), DepartmentID: uuid.New(), Level: "preparator", ActorID: uuid.New(),
	})
	require.NoError(t, c.Handle(context.Background(), env))
	assert.Empty(t, mfa.calls, "a preparator-level grant must not trigger requires-mfa")
}

func TestMembershipConsumer_TenantRoleRevoked_TriggersDrop(t *testing.T) {
	mfa := &stubMfaSyncer{}
	dedup := newStubDedup()
	c := newTestMembership(mfa, dedup)

	userID := uuid.New()
	env := envelope(t, domain.EventTenantRoleRevoked, domain.TenantRoleRevokedPayload{
		UserID: userID, TenantID: uuid.New(), RoleCode: "tenant_admin", ActorID: uuid.New(),
	})
	require.NoError(t, c.Handle(context.Background(), env))
	require.Len(t, mfa.calls, 1)
	assert.False(t, mfa.calls[0].grant)
}

// DepartmentMembershipRevoked is NOT in the frozen filter — if one somehow
// arrives, it must be acknowledged (no-op), not error.
func TestMembershipConsumer_UnknownEventType_Acknowledged(t *testing.T) {
	mfa := &stubMfaSyncer{}
	dedup := newStubDedup()
	c := newTestMembership(mfa, dedup)

	env := envelope(t, "DepartmentMembershipRevoked", map[string]any{})
	require.NoError(t, c.Handle(context.Background(), env))
	assert.Empty(t, mfa.calls)
	assert.Len(t, dedup.marked, 1, "unknown types must still be recorded in processed_events so redelivery does not storm")
}

// Redelivery of an already-processed event must be a no-op (no second
// SyncRequiresMfa call).
func TestMembershipConsumer_Redelivery_IsNoOp(t *testing.T) {
	mfa := &stubMfaSyncer{}
	dedup := newStubDedup()
	c := newTestMembership(mfa, dedup)

	// Constructed ONCE and reused for both Handle calls — a real SQS
	// redelivery carries the identical envelope ID both times.
	env := envelope(t, domain.EventTenantRoleGranted, domain.TenantRoleGrantedPayload{
		UserID: uuid.New(), TenantID: uuid.New(), RoleCode: "tenant_owner", ActorID: uuid.New(),
	})
	require.NoError(t, c.Handle(context.Background(), env))
	require.NoError(t, c.Handle(context.Background(), env))
	assert.Len(t, mfa.calls, 1, "a redelivered event must not re-execute the effect")
}

// An unknown event type still fails loudly if ackUnknown's own
// mark-processed write fails — the wrap-and-return branch in Handle's
// default case, distinct from the known-event-type mark-processed test
// below.
func TestMembershipConsumer_UnknownEventType_MarkProcessedFailurePropagates(t *testing.T) {
	mfa := &stubMfaSyncer{}
	dedup := newStubDedup()
	dedup.markProcessErr = errors.New("dedup write failed")
	c := newTestMembership(mfa, dedup)

	env := envelope(t, "DepartmentMembershipRevoked", map[string]any{})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "membership consumer")
}

func TestMembershipConsumer_IsProcessedErrorPropagates(t *testing.T) {
	mfa := &stubMfaSyncer{}
	dedup := newStubDedup()
	dedup.isProcessedErr = errors.New("dedup store unreachable")
	c := newTestMembership(mfa, dedup)

	env := envelope(t, domain.EventTenantRoleGranted, domain.TenantRoleGrantedPayload{
		UserID: uuid.New(), TenantID: uuid.New(), RoleCode: "tenant_owner", ActorID: uuid.New(),
	})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "check processed_events")
	assert.Empty(t, mfa.calls)
}

func TestMembershipConsumer_DepartmentLevelChanged_ApproverLevel_Assigns(t *testing.T) {
	mfa := &stubMfaSyncer{}
	dedup := newStubDedup()
	c := newTestMembership(mfa, dedup)

	userID := uuid.New()
	env := envelope(t, domain.EventDepartmentMembershipLevelChanged, domain.DepartmentMembershipLevelChangedPayload{
		UserID: userID, TenantID: uuid.New(), DepartmentID: uuid.New(), PreviousLevel: "preparator", NewLevel: "approver", ActorID: uuid.New(),
	})
	require.NoError(t, c.Handle(context.Background(), env))
	require.Len(t, mfa.calls, 1)
	assert.True(t, mfa.calls[0].grant)
	assert.Equal(t, userID, mfa.calls[0].userID)
}

// A de-elevation (approver -> preparator) is documented as NOT a drop
// trigger — the only drop trigger is TenantRoleRevoked (§7.1).
func TestMembershipConsumer_DepartmentLevelChanged_DeElevationToPreparator_NoOp(t *testing.T) {
	mfa := &stubMfaSyncer{}
	dedup := newStubDedup()
	c := newTestMembership(mfa, dedup)

	env := envelope(t, domain.EventDepartmentMembershipLevelChanged, domain.DepartmentMembershipLevelChangedPayload{
		UserID: uuid.New(), TenantID: uuid.New(), DepartmentID: uuid.New(), PreviousLevel: "approver", NewLevel: "preparator", ActorID: uuid.New(),
	})
	require.NoError(t, c.Handle(context.Background(), env))
	assert.Empty(t, mfa.calls, "a de-elevation must not trigger a revoke — only TenantRoleRevoked does")
}

func TestMembershipConsumer_TenantRoleGranted_Assigns(t *testing.T) {
	mfa := &stubMfaSyncer{}
	dedup := newStubDedup()
	c := newTestMembership(mfa, dedup)

	userID := uuid.New()
	env := envelope(t, domain.EventTenantRoleGranted, domain.TenantRoleGrantedPayload{
		UserID: userID, TenantID: uuid.New(), RoleCode: "tender_admin", ActorID: uuid.New(),
	})
	require.NoError(t, c.Handle(context.Background(), env))
	require.Len(t, mfa.calls, 1)
	assert.True(t, mfa.calls[0].grant)
	assert.Equal(t, userID, mfa.calls[0].userID)
}

func TestMembershipConsumer_MalformedPayload_DecodeErrorPerEventType(t *testing.T) {
	for _, eventType := range []string{
		domain.EventDepartmentMembershipGranted,
		domain.EventDepartmentMembershipLevelChanged,
		domain.EventTenantRoleGranted,
		domain.EventTenantRoleRevoked,
	} {
		t.Run(eventType, func(t *testing.T) {
			mfa := &stubMfaSyncer{}
			dedup := newStubDedup()
			c := newTestMembership(mfa, dedup)

			env := events.NewEnvelope(eventType, "test", json.RawMessage(`{"user_id":"not-a-uuid"}`))
			err := c.Handle(context.Background(), env)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "decode "+eventType)
			assert.Empty(t, mfa.calls)
		})
	}
}

// A SyncRequiresMfa failure must propagate wrapped with the event type, and
// must NOT mark the event processed — a retried redelivery must re-attempt
// the effect, not silently skip it.
func TestMembershipConsumer_SyncRequiresMfaFailure_PropagatesAndDoesNotMarkProcessed(t *testing.T) {
	mfa := &stubMfaSyncer{err: errors.New("keycloak unavailable")}
	dedup := newStubDedup()
	c := newTestMembership(mfa, dedup)

	env := envelope(t, domain.EventTenantRoleRevoked, domain.TenantRoleRevokedPayload{
		UserID: uuid.New(), TenantID: uuid.New(), RoleCode: "tenant_admin", ActorID: uuid.New(),
	})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TenantRoleRevoked")
	assert.Empty(t, dedup.marked, "a failed effect must not be recorded as processed")
}

func TestMembershipConsumer_MarkProcessedFailurePropagates(t *testing.T) {
	mfa := &stubMfaSyncer{}
	dedup := newStubDedup()
	dedup.markProcessErr = errors.New("dedup write failed")
	c := newTestMembership(mfa, dedup)

	env := envelope(t, domain.EventTenantRoleGranted, domain.TenantRoleGrantedPayload{
		UserID: uuid.New(), TenantID: uuid.New(), RoleCode: "tenant_owner", ActorID: uuid.New(),
	})
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mark processed")
}
