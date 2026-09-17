package unit_test

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// ── mockKeycloakAdmin — a functional mock: unset fields panic if called,
// letting each test wire only the methods its scenario exercises. ────────

type mockKeycloakAdmin struct {
	ReadyFn                      func(ctx context.Context) error
	CreateRealmFromTemplateFn    func(ctx context.Context, realmName, templateVersion string) error
	RegisterOIDCClientsFn        func(ctx context.Context, realmName, tenantSlug string) ([]string, error)
	DisableRealmFn               func(ctx context.Context, realmName string) error
	EnableRealmFn                func(ctx context.Context, realmName string) error
	ExportRealmFn                func(ctx context.Context, realmName string) ([]byte, error)
	DeleteRealmFn                func(ctx context.Context, realmName string) error
	DeleteRealmIfExistsFn        func(ctx context.Context, realmName string) error
	CreateTrialUserFn            func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error)
	CreateInvitedUserFn          func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error)
	FindUserByEmailFn            func(ctx context.Context, realmName, email string) (*port.ExistingUser, error)
	MigrateUserFn                func(ctx context.Context, sourceRealm, targetRealm string, keycloakUserID uuid.UUID) (bool, error)
	ListUsersByTenantFn          func(ctx context.Context, realmName string, tenantID uuid.UUID) ([]uuid.UUID, error)
	DisableUsersInRealmFn        func(ctx context.Context, realmName string, tenantID uuid.UUID) error
	EnableUsersInRealmFn         func(ctx context.Context, realmName string, tenantID uuid.UUID) error
	HardDeleteUsersInRealmFn     func(ctx context.Context, realmName string, tenantID uuid.UUID) error
	DeleteUserFn                 func(ctx context.Context, realmName string, keycloakUserID uuid.UUID) error
	RevokeUserSessionsFn         func(ctx context.Context, realmName string, keycloakUserID uuid.UUID) error
	ForceLogoutRealmFn           func(ctx context.Context, realmName string) error
	AssignRequiresMFAFn          func(ctx context.Context, realmName string, userID uuid.UUID) error
	RevokeRequiresMFAFn          func(ctx context.Context, realmName string, userID uuid.UUID) error
	HasRequiresMFAFn             func(ctx context.Context, realmName string, userID uuid.UUID) (bool, error)
	ResetMFAFn                   func(ctx context.Context, realmName string, userID uuid.UUID, requiresMFA bool) error
	CreateOrUpdateIdpFn          func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error
	DeleteIdpFn                  func(ctx context.Context, realmName, alias string) error
	ApplyRealmConfigFn           func(ctx context.Context, realmName, settingKey string, desiredValue any) error
	CreateServiceAccountClientFn func(ctx context.Context, realmName, clientID string) (uuid.UUID, error)
	SetClientSecretFn            func(ctx context.Context, realmName, clientID, secret string) error
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
func (m *mockKeycloakAdmin) FindUserByEmail(ctx context.Context, realmName, email string) (*port.ExistingUser, error) {
	return m.FindUserByEmailFn(ctx, realmName, email)
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
func (m *mockKeycloakAdmin) CreateServiceAccountClient(ctx context.Context, realmName, clientID string) (uuid.UUID, error) {
	return m.CreateServiceAccountClientFn(ctx, realmName, clientID)
}
func (m *mockKeycloakAdmin) SetClientSecret(ctx context.Context, realmName, clientID, secret string) error {
	return m.SetClientSecretFn(ctx, realmName, clientID, secret)
}

// ── mockTenantRealmRepository — in-memory, keyed by tenant_id. Optional
// *Err fields inject a failure on the next call, for exercising the rare
// same-process local-commit-failure paths. ──────────────────────────────

type mockTenantRealmRepository struct {
	rows             map[uuid.UUID]*domain.TenantRealm
	UpsertErr        error
	UpdateErr        error
	CountsByShardErr error
}

func newMockTenantRealmRepository() *mockTenantRealmRepository {
	return &mockTenantRealmRepository{rows: map[uuid.UUID]*domain.TenantRealm{}}
}

var _ port.TenantRealmRepository = (*mockTenantRealmRepository)(nil)

func (m *mockTenantRealmRepository) FindByTenantID(ctx context.Context, tenantID uuid.UUID) (*domain.TenantRealm, error) {
	if r, ok := m.rows[tenantID]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, domain.NewError(domain.ErrTenantRealmNotFound, "not found")
}
func (m *mockTenantRealmRepository) Upsert(ctx context.Context, realm *domain.TenantRealm) error {
	if m.UpsertErr != nil {
		return m.UpsertErr
	}
	cp := *realm
	cp.RecordVersion++
	m.rows[realm.TenantID] = &cp
	return nil
}
func (m *mockTenantRealmRepository) Update(ctx context.Context, realm *domain.TenantRealm) error {
	if m.UpdateErr != nil {
		return m.UpdateErr
	}
	cp := *realm
	m.rows[realm.TenantID] = &cp
	return nil
}
func (m *mockTenantRealmRepository) ListExpiredTrials(ctx context.Context, limit int) ([]*domain.TenantRealm, error) {
	var out []*domain.TenantRealm
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
}
func (m *mockTenantRealmRepository) ListSuspendedPastRetention(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
	var out []*domain.TenantRealm
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
}
func (m *mockTenantRealmRepository) CountsByShard(ctx context.Context) (map[string]int, error) {
	if m.CountsByShardErr != nil {
		return nil, m.CountsByShardErr
	}
	out := map[string]int{}
	for _, r := range m.rows {
		if r.Strategy == domain.RealmStrategyDedicated && r.KeycloakShard != nil {
			out[*r.KeycloakShard]++
		}
	}
	return out, nil
}

// ── mockIdempotencyStore — in-memory. FailErr/CompleteErr, when set,
// simulate the ledger itself misbehaving so the best-effort logging
// branches around Fail/Complete can be exercised. ────────────────────────

type mockIdempotencyStore struct {
	rows        map[string]*domain.ProvisioningOperation
	FailErr     error
	CompleteErr error
	// BeginErr, when set, makes Begin return this error (a hard ledger
	// failure, e.g. Postgres unavailable) instead of the normal
	// found/not-found flow — distinct from a `found` row, which is not an
	// error at all (§9.2).
	BeginErr error
}

func newMockIdempotencyStore() *mockIdempotencyStore {
	return &mockIdempotencyStore{rows: map[string]*domain.ProvisioningOperation{}}
}

var _ port.IdempotencyStore = (*mockIdempotencyStore)(nil)

func (m *mockIdempotencyStore) Begin(ctx context.Context, key string, tenantID uuid.UUID, operation string) (*domain.ProvisioningOperation, bool, error) {
	if m.BeginErr != nil {
		return nil, false, m.BeginErr
	}
	if existing, ok := m.rows[key]; ok {
		if existing.Status == domain.OpStatusFailed {
			// Mirrors IdempotencyRepository.Begin's ON CONFLICT DO UPDATE
			// ... WHERE status='failed' reclaim — a failed row is reset and
			// the caller proceeds, never permanently 409s.
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
	if m.FailErr != nil {
		return m.FailErr
	}
	if row, ok := m.rows[key]; ok {
		row.Status = domain.OpStatusFailed
	}
	return nil
}

// ── mockTxRunner — runs fn directly against context.Background()'s child,
// no real transaction, and injects a mockEventPublisher capturing every
// enqueued event for assertions. ──────────────────────────────────────────

type mockEventPublisher struct {
	events []*domain.Event
	// EnqueueErr, when set, makes Enqueue fail starting from the
	// FailFromCall'th invocation (1-indexed; 0 means "fail immediately, i.e.
	// from the 1st call") instead of recording the event — for exercising a
	// specific Enqueue's own `if err != nil { return err }` branch inside
	// a tx that enqueues more than one event.
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
	pub *mockEventPublisher
	// FailFn, when set, makes RunInTx return this error instead of running fn.
	FailFn func() error
	// SkipPublisher, when true, does NOT wire pub into ctx before invoking
	// fn — exercises the port.EventPublisherFromContext !ok short-circuit
	// branch (no publisher wired for this transaction, e.g. a caller that
	// runs outside the outbox-enabled request path).
	SkipPublisher bool
}

func newMockTxRunner() (*mockTxRunner, *mockEventPublisher) {
	pub := &mockEventPublisher{}
	return &mockTxRunner{pub: pub}, pub
}

// newMockTxRunnerNoPublisher returns a TxRunner whose RunInTx never wires an
// event publisher into ctx, for exercising the `!ok` branch of
// port.EventPublisherFromContext in each RunInTx closure.
func newMockTxRunnerNoPublisher() *mockTxRunner {
	return &mockTxRunner{SkipPublisher: true}
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

// ── mockIdpConfigRepository — functional mock, same convention as
// mockKeycloakAdmin: unset fields panic if called. ───────────────────────

type mockIdpConfigRepository struct {
	FindByAliasFn  func(ctx context.Context, tenantID uuid.UUID, alias string) (*domain.IdpConfig, error)
	ListByTenantFn func(ctx context.Context, tenantID uuid.UUID) ([]*domain.IdpConfig, error)
	UpsertFn       func(ctx context.Context, cfg *domain.IdpConfig) error
	DeleteFn       func(ctx context.Context, tenantID uuid.UUID, alias string) error
	ListAllFn      func(ctx context.Context) ([]*domain.IdpConfig, error)
}

var _ port.IdpConfigRepository = (*mockIdpConfigRepository)(nil)

func (m *mockIdpConfigRepository) FindByAlias(ctx context.Context, tenantID uuid.UUID, alias string) (*domain.IdpConfig, error) {
	return m.FindByAliasFn(ctx, tenantID, alias)
}
func (m *mockIdpConfigRepository) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*domain.IdpConfig, error) {
	return m.ListByTenantFn(ctx, tenantID)
}
func (m *mockIdpConfigRepository) Upsert(ctx context.Context, cfg *domain.IdpConfig) error {
	return m.UpsertFn(ctx, cfg)
}
func (m *mockIdpConfigRepository) Delete(ctx context.Context, tenantID uuid.UUID, alias string) error {
	return m.DeleteFn(ctx, tenantID, alias)
}
func (m *mockIdpConfigRepository) ListAll(ctx context.Context) ([]*domain.IdpConfig, error) {
	return m.ListAllFn(ctx)
}

// ── mockIdpValidationRepository — functional mock. ───────────────────────

type mockIdpValidationRepository struct {
	InsertFn         func(ctx context.Context, v *domain.IdpConfigValidation) error
	LatestForAliasFn func(ctx context.Context, tenantID uuid.UUID, alias string) (*domain.IdpConfigValidation, error)
}

var _ port.IdpValidationRepository = (*mockIdpValidationRepository)(nil)

func (m *mockIdpValidationRepository) Insert(ctx context.Context, v *domain.IdpConfigValidation) error {
	return m.InsertFn(ctx, v)
}
func (m *mockIdpValidationRepository) LatestForAlias(ctx context.Context, tenantID uuid.UUID, alias string) (*domain.IdpConfigValidation, error) {
	return m.LatestForAliasFn(ctx, tenantID, alias)
}

// ── mockOpenBaoClient — functional mock. ─────────────────────────────────

type mockOpenBaoClient struct {
	WriteIdpSecretFn  func(ctx context.Context, tenantID uuid.UUID, idpAlias string, secret map[string]any) error
	ReadIdpSecretFn   func(ctx context.Context, tenantID uuid.UUID, idpAlias string) (map[string]any, error)
	DeleteIdpSecretFn func(ctx context.Context, tenantID uuid.UUID, idpAlias string) error
}

var _ port.OpenBaoClient = (*mockOpenBaoClient)(nil)

func (m *mockOpenBaoClient) WriteIdpSecret(ctx context.Context, tenantID uuid.UUID, idpAlias string, secret map[string]any) error {
	return m.WriteIdpSecretFn(ctx, tenantID, idpAlias, secret)
}
func (m *mockOpenBaoClient) ReadIdpSecret(ctx context.Context, tenantID uuid.UUID, idpAlias string) (map[string]any, error) {
	return m.ReadIdpSecretFn(ctx, tenantID, idpAlias)
}
func (m *mockOpenBaoClient) DeleteIdpSecret(ctx context.Context, tenantID uuid.UUID, idpAlias string) error {
	return m.DeleteIdpSecretFn(ctx, tenantID, idpAlias)
}

// ── mockReconciliationRepository — functional mock. ──────────────────────

type mockReconciliationRepository struct {
	InsertFn      func(ctx context.Context, r *domain.RealmConfigReconciliation) error
	ListPendingFn func(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error)
	MarkAppliedFn func(ctx context.Context, id uuid.UUID) error
	// MarkFailedFn defaults to returning (ReconStatusPending, nil) when
	// unset, matching MarkFailed's usual "leave it pending for retry"
	// behavior.
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

// ── mockLogger — implements port.Logger, counting calls per level so
// best-effort "logged, not propagated" branches can be asserted on. ─────

type mockLogger struct {
	debugCalls, infoCalls, warnCalls, errorCalls int
}

var _ port.Logger = (*mockLogger)(nil)

func (m *mockLogger) Debug(msg string, fields map[string]any) { m.debugCalls++ }
func (m *mockLogger) Info(msg string, fields map[string]any)  { m.infoCalls++ }
func (m *mockLogger) Warn(msg string, fields map[string]any)  { m.warnCalls++ }
func (m *mockLogger) Error(msg string, fields map[string]any) { m.errorCalls++ }

// ── mockTokenServiceClient — functional mock: unset fields panic if
// called, letting each test wire only the methods its scenario exercises
// (§2.5, RP-17/RP-18). ────────────────────────────────────────────────────

type mockTokenServiceClient struct {
	RegisterPrincipalFn       func(ctx context.Context, tenantID uuid.UUID, req port.RegisterPrincipalRequest) (*port.RegisterPrincipalResult, error)
	IssueOrRotateCredentialFn func(ctx context.Context, tenantID, principalID uuid.UUID, req port.IssueCredentialRequest) (*port.IssueCredentialResult, error)
	ReadPrincipalFn           func(ctx context.Context, tenantID, principalID uuid.UUID) (*port.ReadPrincipalResult, error)
}

var _ port.TokenServiceClient = (*mockTokenServiceClient)(nil)

func (m *mockTokenServiceClient) RegisterPrincipal(ctx context.Context, tenantID uuid.UUID, req port.RegisterPrincipalRequest) (*port.RegisterPrincipalResult, error) {
	return m.RegisterPrincipalFn(ctx, tenantID, req)
}
func (m *mockTokenServiceClient) IssueOrRotateCredential(ctx context.Context, tenantID, principalID uuid.UUID, req port.IssueCredentialRequest) (*port.IssueCredentialResult, error) {
	return m.IssueOrRotateCredentialFn(ctx, tenantID, principalID, req)
}
func (m *mockTokenServiceClient) ReadPrincipal(ctx context.Context, tenantID, principalID uuid.UUID) (*port.ReadPrincipalResult, error) {
	return m.ReadPrincipalFn(ctx, tenantID, principalID)
}

// wirePlatformAutomationHappyPath sets kc's CreateServiceAccountClientFn/
// SetClientSecretFn and returns a mockTokenServiceClient wired end-to-end
// for a successful mint (§2.5) — the shared default every RP-1/RP-2/RP-3
// success-path test needs, since mintPlatformAutomation now runs as part
// of those flows. Tests exercising the mint step itself override
// individual *Fn fields on the returned mock or on kc afterward.
func wirePlatformAutomationHappyPath(kc *mockKeycloakAdmin) *mockTokenServiceClient {
	kc.CreateServiceAccountClientFn = func(ctx context.Context, realmName, clientID string) (uuid.UUID, error) {
		return uuid.New(), nil
	}
	kc.SetClientSecretFn = func(ctx context.Context, realmName, clientID, secret string) error { return nil }
	return &mockTokenServiceClient{
		RegisterPrincipalFn: func(ctx context.Context, tenantID uuid.UUID, req port.RegisterPrincipalRequest) (*port.RegisterPrincipalResult, error) {
			return &port.RegisterPrincipalResult{PrincipalID: uuid.New(), Created: true, RecordVersion: 1}, nil
		},
		IssueOrRotateCredentialFn: func(ctx context.Context, tenantID, principalID uuid.UUID, req port.IssueCredentialRequest) (*port.IssueCredentialResult, error) {
			return &port.IssueCredentialResult{Version: 1, Secret: "test-secret", OpenBaoPath: "iam/serviceaccount/test/v1", RecordVersion: 1}, nil
		},
	}
}
