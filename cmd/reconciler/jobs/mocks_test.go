package jobs

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// White-box mocks for the jobs package, following the same "functional
// mock" convention as internal/core/service's test suite
// (test/unit/mocks_test.go): unset *Fn fields panic if called, so each test
// wires only the methods its scenario actually exercises.

// ── mockKeycloakAdmin ─────────────────────────────────────────────────────

type mockKeycloakAdmin struct {
	ReadyFn                   func(ctx context.Context) error
	CreateRealmFromTemplateFn func(ctx context.Context, realmName, templateVersion string) error
	RegisterOIDCClientsFn     func(ctx context.Context, realmName, tenantSlug string) ([]string, error)
	DisableRealmFn            func(ctx context.Context, realmName string) error
	EnableRealmFn             func(ctx context.Context, realmName string) error
	ExportRealmFn             func(ctx context.Context, realmName string) ([]byte, error)
	DeleteRealmFn             func(ctx context.Context, realmName string) error
	DeleteRealmIfExistsFn     func(ctx context.Context, realmName string) error
	CreateTrialUserFn         func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error)
	CreateInvitedUserFn       func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error)
	MigrateUserFn             func(ctx context.Context, sourceRealm, targetRealm string, keycloakUserID uuid.UUID) (bool, error)
	ListUsersByTenantFn       func(ctx context.Context, realmName string, tenantID uuid.UUID) ([]uuid.UUID, error)
	DisableUsersInRealmFn     func(ctx context.Context, realmName string, tenantID uuid.UUID) error
	EnableUsersInRealmFn      func(ctx context.Context, realmName string, tenantID uuid.UUID) error
	HardDeleteUsersInRealmFn  func(ctx context.Context, realmName string, tenantID uuid.UUID) error
	DeleteUserFn              func(ctx context.Context, realmName string, keycloakUserID uuid.UUID) error
	RevokeUserSessionsFn      func(ctx context.Context, realmName string, keycloakUserID uuid.UUID) error
	ForceLogoutRealmFn        func(ctx context.Context, realmName string) error
	AssignRequiresMFAFn       func(ctx context.Context, realmName string, userID uuid.UUID) error
	RevokeRequiresMFAFn       func(ctx context.Context, realmName string, userID uuid.UUID) error
	HasRequiresMFAFn          func(ctx context.Context, realmName string, userID uuid.UUID) (bool, error)
	ResetMFAFn                func(ctx context.Context, realmName string, userID uuid.UUID, requiresMFA bool) error
	CreateOrUpdateIdpFn       func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error
	DeleteIdpFn               func(ctx context.Context, realmName, alias string) error
	ApplyRealmConfigFn        func(ctx context.Context, realmName, settingKey string, desiredValue any) error
}

var _ port.KeycloakAdmin = (*mockKeycloakAdmin)(nil)

func (m *mockKeycloakAdmin) Ready(ctx context.Context) error { return m.ReadyFn(ctx) }
func (m *mockKeycloakAdmin) CreateRealmFromTemplate(ctx context.Context, realmName, templateVersion string) error {
	return m.CreateRealmFromTemplateFn(ctx, realmName, templateVersion)
}
func (m *mockKeycloakAdmin) RegisterOIDCClients(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
	return m.RegisterOIDCClientsFn(ctx, realmName, tenantSlug)
}
func (m *mockKeycloakAdmin) DisableRealm(ctx context.Context, realmName string) error {
	return m.DisableRealmFn(ctx, realmName)
}
func (m *mockKeycloakAdmin) EnableRealm(ctx context.Context, realmName string) error {
	return m.EnableRealmFn(ctx, realmName)
}
func (m *mockKeycloakAdmin) ExportRealm(ctx context.Context, realmName string) ([]byte, error) {
	return m.ExportRealmFn(ctx, realmName)
}
func (m *mockKeycloakAdmin) DeleteRealm(ctx context.Context, realmName string) error {
	return m.DeleteRealmFn(ctx, realmName)
}
func (m *mockKeycloakAdmin) DeleteRealmIfExists(ctx context.Context, realmName string) error {
	return m.DeleteRealmIfExistsFn(ctx, realmName)
}
func (m *mockKeycloakAdmin) CreateTrialUser(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
	return m.CreateTrialUserFn(ctx, req)
}
func (m *mockKeycloakAdmin) CreateInvitedUser(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
	return m.CreateInvitedUserFn(ctx, realmName, req)
}
func (m *mockKeycloakAdmin) MigrateUser(ctx context.Context, sourceRealm, targetRealm string, keycloakUserID uuid.UUID) (bool, error) {
	return m.MigrateUserFn(ctx, sourceRealm, targetRealm, keycloakUserID)
}
func (m *mockKeycloakAdmin) ListUsersByTenant(ctx context.Context, realmName string, tenantID uuid.UUID) ([]uuid.UUID, error) {
	return m.ListUsersByTenantFn(ctx, realmName, tenantID)
}
func (m *mockKeycloakAdmin) DisableUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) error {
	return m.DisableUsersInRealmFn(ctx, realmName, tenantID)
}
func (m *mockKeycloakAdmin) EnableUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) error {
	return m.EnableUsersInRealmFn(ctx, realmName, tenantID)
}
func (m *mockKeycloakAdmin) HardDeleteUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) error {
	return m.HardDeleteUsersInRealmFn(ctx, realmName, tenantID)
}
func (m *mockKeycloakAdmin) DeleteUser(ctx context.Context, realmName string, keycloakUserID uuid.UUID) error {
	return m.DeleteUserFn(ctx, realmName, keycloakUserID)
}
func (m *mockKeycloakAdmin) RevokeUserSessions(ctx context.Context, realmName string, keycloakUserID uuid.UUID) error {
	return m.RevokeUserSessionsFn(ctx, realmName, keycloakUserID)
}
func (m *mockKeycloakAdmin) ForceLogoutRealm(ctx context.Context, realmName string) error {
	return m.ForceLogoutRealmFn(ctx, realmName)
}
func (m *mockKeycloakAdmin) AssignRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) error {
	return m.AssignRequiresMFAFn(ctx, realmName, userID)
}
func (m *mockKeycloakAdmin) RevokeRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) error {
	return m.RevokeRequiresMFAFn(ctx, realmName, userID)
}
func (m *mockKeycloakAdmin) HasRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) (bool, error) {
	return m.HasRequiresMFAFn(ctx, realmName, userID)
}
func (m *mockKeycloakAdmin) ResetMFA(ctx context.Context, realmName string, userID uuid.UUID, requiresMFA bool) error {
	return m.ResetMFAFn(ctx, realmName, userID, requiresMFA)
}
func (m *mockKeycloakAdmin) CreateOrUpdateIdp(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
	return m.CreateOrUpdateIdpFn(ctx, realmName, idp)
}
func (m *mockKeycloakAdmin) DeleteIdp(ctx context.Context, realmName, alias string) error {
	return m.DeleteIdpFn(ctx, realmName, alias)
}
func (m *mockKeycloakAdmin) ApplyRealmConfig(ctx context.Context, realmName, settingKey string, desiredValue any) error {
	return m.ApplyRealmConfigFn(ctx, realmName, settingKey, desiredValue)
}

// ── mockTenantRealmRepository ─────────────────────────────────────────────

type mockTenantRealmRepository struct {
	FindByTenantIDFn             func(ctx context.Context, tenantID uuid.UUID) (*domain.TenantRealm, error)
	UpsertFn                     func(ctx context.Context, realm *domain.TenantRealm) error
	UpdateFn                     func(ctx context.Context, realm *domain.TenantRealm) error
	ListExpiredTrialsFn          func(ctx context.Context, limit int) ([]*domain.TenantRealm, error)
	ListSuspendedPastRetentionFn func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error)
	CountsByShardFn              func(ctx context.Context) (map[string]int, error)
}

var _ port.TenantRealmRepository = (*mockTenantRealmRepository)(nil)

func (m *mockTenantRealmRepository) FindByTenantID(ctx context.Context, tenantID uuid.UUID) (*domain.TenantRealm, error) {
	return m.FindByTenantIDFn(ctx, tenantID)
}
func (m *mockTenantRealmRepository) Upsert(ctx context.Context, realm *domain.TenantRealm) error {
	return m.UpsertFn(ctx, realm)
}
func (m *mockTenantRealmRepository) Update(ctx context.Context, realm *domain.TenantRealm) error {
	if m.UpdateFn == nil {
		return nil
	}
	return m.UpdateFn(ctx, realm)
}
func (m *mockTenantRealmRepository) ListExpiredTrials(ctx context.Context, limit int) ([]*domain.TenantRealm, error) {
	return m.ListExpiredTrialsFn(ctx, limit)
}
func (m *mockTenantRealmRepository) ListSuspendedPastRetention(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
	return m.ListSuspendedPastRetentionFn(ctx, retentionDays, limit)
}
func (m *mockTenantRealmRepository) CountsByShard(ctx context.Context) (map[string]int, error) {
	return m.CountsByShardFn(ctx)
}

// ── mockReconciliationRepository ──────────────────────────────────────────

type mockReconciliationRepository struct {
	InsertFn      func(ctx context.Context, r *domain.RealmConfigReconciliation) error
	ListPendingFn func(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error)
	MarkAppliedFn func(ctx context.Context, id uuid.UUID) error
	// MarkFailedFn defaults to returning (ReconStatusPending, nil) when
	// unset, matching MarkFailed's usual "leave it pending for retry"
	// behavior — only tests exercising the poison-pill cap itself need to
	// set this explicitly to return ReconStatusFailed.
	MarkFailedFn func(ctx context.Context, id uuid.UUID, lastErr string, maxAttempts int) (domain.ReconStatus, error)
}

var _ port.ReconciliationRepository = (*mockReconciliationRepository)(nil)

func (m *mockReconciliationRepository) Insert(ctx context.Context, r *domain.RealmConfigReconciliation) error {
	return m.InsertFn(ctx, r)
}
func (m *mockReconciliationRepository) ListPending(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error) {
	return m.ListPendingFn(ctx, limit)
}
func (m *mockReconciliationRepository) MarkApplied(ctx context.Context, id uuid.UUID) error {
	return m.MarkAppliedFn(ctx, id)
}
func (m *mockReconciliationRepository) MarkFailed(ctx context.Context, id uuid.UUID, lastErr string, maxAttempts int) (domain.ReconStatus, error) {
	if m.MarkFailedFn == nil {
		return domain.ReconStatusPending, nil
	}
	return m.MarkFailedFn(ctx, id, lastErr, maxAttempts)
}

// ── mockIdempotencyStore — in-memory, keyed by idempotency key. Also
// counts Begin/Complete/Fail invocations so tests can assert a Keycloak-
// effect-first no-op branch never touched the ledger. ────────────────────

type mockIdempotencyStore struct {
	rows        map[string]*domain.ProvisioningOperation
	BeginErr    error
	CompleteErr error
	FailErr     error

	BeginCalls    int
	CompleteCalls int
	FailCalls     int
}

func newMockIdempotencyStore() *mockIdempotencyStore {
	return &mockIdempotencyStore{rows: map[string]*domain.ProvisioningOperation{}}
}

var _ port.IdempotencyStore = (*mockIdempotencyStore)(nil)

func (m *mockIdempotencyStore) Begin(ctx context.Context, key string, tenantID uuid.UUID, operation string) (*domain.ProvisioningOperation, bool, error) {
	m.BeginCalls++
	if m.BeginErr != nil {
		return nil, false, m.BeginErr
	}
	if existing, ok := m.rows[key]; ok {
		if existing.Status == domain.OpStatusFailed {
			// Mirrors the real IdempotencyRepository.Begin's ON CONFLICT DO
			// UPDATE ... WHERE status='failed' reclaim — a failed row is
			// reset and the caller proceeds, never permanently 409s.
			existing.Status = domain.OpStatusInProgress
			existing.Result = nil
			existing.CompletedAt = nil
			return nil, false, nil
		}
		cp := *existing
		return &cp, true, nil
	}
	m.rows[key] = &domain.ProvisioningOperation{IdempotencyKey: key, TenantID: tenantID, Operation: operation, Status: domain.OpStatusInProgress}
	return nil, false, nil
}
func (m *mockIdempotencyStore) Complete(ctx context.Context, key string, result json.RawMessage) error {
	m.CompleteCalls++
	if m.CompleteErr != nil {
		return m.CompleteErr
	}
	if row, ok := m.rows[key]; ok {
		row.Status = domain.OpStatusSucceeded
		row.Result = result
	}
	return nil
}
func (m *mockIdempotencyStore) Fail(ctx context.Context, key string) error {
	m.FailCalls++
	if m.FailErr != nil {
		return m.FailErr
	}
	if row, ok := m.rows[key]; ok {
		row.Status = domain.OpStatusFailed
	}
	return nil
}

// ── mockObjectStore ────────────────────────────────────────────────────────

type mockObjectStore struct {
	PutRealmExportFn func(ctx context.Context, key string, data []byte) error
}

var _ port.ObjectStore = (*mockObjectStore)(nil)

func (m *mockObjectStore) PutRealmExport(ctx context.Context, key string, data []byte) error {
	return m.PutRealmExportFn(ctx, key, data)
}

// ── mockSubscriptionLapseReader ────────────────────────────────────────────

type mockSubscriptionLapseReader struct {
	ListLapsedSubscriptionsFn func(ctx context.Context) ([]port.LapsedSubscription, error)
}

var _ port.SubscriptionLapseReader = (*mockSubscriptionLapseReader)(nil)

func (m *mockSubscriptionLapseReader) ListLapsedSubscriptions(ctx context.Context) ([]port.LapsedSubscription, error) {
	return m.ListLapsedSubscriptionsFn(ctx)
}

// ── mockEventPublisher / mockTxRunner — same convention as
// test/unit/mocks_test.go: RunInTx wires a tx-bound event publisher into
// ctx that records every enqueued event. ──────────────────────────────────

type mockEventPublisher struct {
	events       []*domain.Event
	EnqueueErr   error
	FailFromCall int
	callCount    int
}

func (p *mockEventPublisher) Enqueue(ctx context.Context, event *domain.Event) error {
	p.callCount++
	if p.EnqueueErr != nil {
		failFrom := p.FailFromCall
		if failFrom == 0 {
			failFrom = 1
		}
		if p.callCount >= failFrom {
			return p.EnqueueErr
		}
	}
	p.events = append(p.events, event)
	return nil
}

type mockTxRunner struct {
	pub           *mockEventPublisher
	FailFn        func() error
	SkipPublisher bool
}

func newMockTxRunner() (*mockTxRunner, *mockEventPublisher) {
	pub := &mockEventPublisher{}
	return &mockTxRunner{pub: pub}, pub
}

var _ port.TxRunner = (*mockTxRunner)(nil)

func (r *mockTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if r.FailFn != nil {
		if err := r.FailFn(); err != nil {
			return err
		}
	}
	if !r.SkipPublisher {
		ctx = port.WithEventPublisher(ctx, r.pub)
	}
	return fn(ctx)
}

// ── mockLogger — implements port.Logger, both counting calls per level and
// capturing the message text so a specific WARN branch can be asserted on
// (e.g. "no SubscriptionLapseReader wired"). ────────────────────────────────

type mockLogger struct {
	debugCalls, infoCalls, warnCalls, errorCalls int
	warnMsgs, infoMsgs, errorMsgs                []string
}

var _ port.Logger = (*mockLogger)(nil)

func (m *mockLogger) Debug(msg string, fields map[string]any) { m.debugCalls++ }
func (m *mockLogger) Info(msg string, fields map[string]any) {
	m.infoCalls++
	m.infoMsgs = append(m.infoMsgs, msg)
}
func (m *mockLogger) Warn(msg string, fields map[string]any) {
	m.warnCalls++
	m.warnMsgs = append(m.warnMsgs, msg)
}
func (m *mockLogger) Error(msg string, fields map[string]any) {
	m.errorCalls++
	m.errorMsgs = append(m.errorMsgs, msg)
}

// newTestContext builds a *Context with every dependency wired to a fresh
// mock/fake and zero-value tuning knobs (BatchLimit etc. are set by
// individual tests as needed). Callers overwrite whichever fields their
// scenario exercises.
func newTestContext() (*Context, *mockKeycloakAdmin, *mockTenantRealmRepository, *mockIdempotencyStore, *mockEventPublisher, *mockLogger) {
	kc := &mockKeycloakAdmin{}
	realms := &mockTenantRealmRepository{}
	idem := newMockIdempotencyStore()
	tx, pub := newMockTxRunner()
	logger := &mockLogger{}
	jctx := &Context{
		TxRunner:                  tx,
		TenantRealms:              realms,
		Idempotency:               idem,
		Keycloak:                  kc,
		Logger:                    port.NewSlogStyleLogger(logger),
		BatchLimit:                100,
		MaxReconciliationAttempts: 20,
	}
	return jctx, kc, realms, idem, pub, logger
}
