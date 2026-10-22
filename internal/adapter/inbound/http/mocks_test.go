package http

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// ── fakeKeycloakAdmin — functional mock: unset fields panic if called,
// letting each test wire only the methods its scenario exercises (mirrors
// internal/core/service/mocks_test.go's convention). ────────────────────────

type fakeKeycloakAdmin struct {
	ReadyFn                              func(ctx context.Context) error
	CreateRealmFromTemplateFn            func(ctx context.Context, realmName, templateVersion string) error
	RegisterOIDCClientsFn                func(ctx context.Context, realmName, tenantSlug string) ([]string, error)
	DisableRealmFn                       func(ctx context.Context, realmName string) error
	EnableRealmFn                        func(ctx context.Context, realmName string) error
	ExportRealmFn                        func(ctx context.Context, realmName string) ([]byte, error)
	DeleteRealmFn                        func(ctx context.Context, realmName string) error
	DeleteRealmIfExistsFn                func(ctx context.Context, realmName string) error
	CreateTrialUserFn                    func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error)
	CreateInvitedUserFn                  func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error)
	FindUserByEmailFn                    func(ctx context.Context, realmName, email string) (*port.ExistingUser, error)
	MigrateUserFn                        func(ctx context.Context, sourceRealm, targetRealm string, keycloakUserID uuid.UUID) (bool, error)
	ListUsersByTenantFn                  func(ctx context.Context, realmName string, tenantID uuid.UUID) ([]uuid.UUID, error)
	DisableUsersInRealmFn                func(ctx context.Context, realmName string, tenantID uuid.UUID) error
	EnableUsersInRealmFn                 func(ctx context.Context, realmName string, tenantID uuid.UUID) error
	HardDeleteUsersInRealmFn             func(ctx context.Context, realmName string, tenantID uuid.UUID) error
	DeleteUserFn                         func(ctx context.Context, realmName string, keycloakUserID uuid.UUID) error
	RevokeUserSessionsFn                 func(ctx context.Context, realmName string, keycloakUserID uuid.UUID) error
	ForceLogoutRealmFn                   func(ctx context.Context, realmName string) error
	AssignRequiresMFAFn                  func(ctx context.Context, realmName string, userID uuid.UUID) error
	RevokeRequiresMFAFn                  func(ctx context.Context, realmName string, userID uuid.UUID) error
	HasRequiresMFAFn                     func(ctx context.Context, realmName string, userID uuid.UUID) (bool, error)
	ResetMFAFn                           func(ctx context.Context, realmName string, userID uuid.UUID, requiresMFA bool) error
	CreateOrUpdateIdpFn                  func(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error
	DeleteIdpFn                          func(ctx context.Context, realmName, alias string) error
	ApplyRealmConfigFn                   func(ctx context.Context, realmName, settingKey string, desiredValue any) error
	CreateServiceAccountClientFn         func(ctx context.Context, realmName, clientID, jwksURL string) (uuid.UUID, bool, error)
	ClearServiceAccountKeysCacheFn       func(ctx context.Context, realmName string) error
	DeleteServiceAccountClientIfExistsFn func(ctx context.Context, realmName, clientID string) error
}

var _ port.KeycloakAdmin = (*fakeKeycloakAdmin)(nil)

func (m *fakeKeycloakAdmin) Ready(ctx context.Context) error { return m.ReadyFn(ctx) }
func (m *fakeKeycloakAdmin) CreateRealmFromTemplate(ctx context.Context, realmName, templateVersion string) error {
	return m.CreateRealmFromTemplateFn(ctx, realmName, templateVersion)
}
func (m *fakeKeycloakAdmin) RegisterOIDCClients(ctx context.Context, realmName, tenantSlug string) ([]string, error) {
	return m.RegisterOIDCClientsFn(ctx, realmName, tenantSlug)
}
func (m *fakeKeycloakAdmin) DisableRealm(ctx context.Context, realmName string) error {
	return m.DisableRealmFn(ctx, realmName)
}
func (m *fakeKeycloakAdmin) EnableRealm(ctx context.Context, realmName string) error {
	return m.EnableRealmFn(ctx, realmName)
}
func (m *fakeKeycloakAdmin) ExportRealm(ctx context.Context, realmName string) ([]byte, error) {
	return m.ExportRealmFn(ctx, realmName)
}
func (m *fakeKeycloakAdmin) DeleteRealm(ctx context.Context, realmName string) error {
	return m.DeleteRealmFn(ctx, realmName)
}
func (m *fakeKeycloakAdmin) DeleteRealmIfExists(ctx context.Context, realmName string) error {
	return m.DeleteRealmIfExistsFn(ctx, realmName)
}
func (m *fakeKeycloakAdmin) CreateTrialUser(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
	return m.CreateTrialUserFn(ctx, req)
}
func (m *fakeKeycloakAdmin) CreateInvitedUser(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
	return m.CreateInvitedUserFn(ctx, realmName, req)
}
func (m *fakeKeycloakAdmin) FindUserByEmail(ctx context.Context, realmName, email string) (*port.ExistingUser, error) {
	return m.FindUserByEmailFn(ctx, realmName, email)
}
func (m *fakeKeycloakAdmin) MigrateUser(ctx context.Context, sourceRealm, targetRealm string, keycloakUserID uuid.UUID) (bool, error) {
	return m.MigrateUserFn(ctx, sourceRealm, targetRealm, keycloakUserID)
}
func (m *fakeKeycloakAdmin) ListUsersByTenant(ctx context.Context, realmName string, tenantID uuid.UUID) ([]uuid.UUID, error) {
	return m.ListUsersByTenantFn(ctx, realmName, tenantID)
}
func (m *fakeKeycloakAdmin) DisableUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) error {
	return m.DisableUsersInRealmFn(ctx, realmName, tenantID)
}
func (m *fakeKeycloakAdmin) EnableUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) error {
	return m.EnableUsersInRealmFn(ctx, realmName, tenantID)
}
func (m *fakeKeycloakAdmin) HardDeleteUsersInRealm(ctx context.Context, realmName string, tenantID uuid.UUID) error {
	return m.HardDeleteUsersInRealmFn(ctx, realmName, tenantID)
}
func (m *fakeKeycloakAdmin) DeleteUser(ctx context.Context, realmName string, keycloakUserID uuid.UUID) error {
	return m.DeleteUserFn(ctx, realmName, keycloakUserID)
}
func (m *fakeKeycloakAdmin) RevokeUserSessions(ctx context.Context, realmName string, keycloakUserID uuid.UUID) error {
	return m.RevokeUserSessionsFn(ctx, realmName, keycloakUserID)
}
func (m *fakeKeycloakAdmin) ForceLogoutRealm(ctx context.Context, realmName string) error {
	return m.ForceLogoutRealmFn(ctx, realmName)
}
func (m *fakeKeycloakAdmin) AssignRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) error {
	return m.AssignRequiresMFAFn(ctx, realmName, userID)
}
func (m *fakeKeycloakAdmin) RevokeRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) error {
	return m.RevokeRequiresMFAFn(ctx, realmName, userID)
}
func (m *fakeKeycloakAdmin) HasRequiresMFA(ctx context.Context, realmName string, userID uuid.UUID) (bool, error) {
	return m.HasRequiresMFAFn(ctx, realmName, userID)
}
func (m *fakeKeycloakAdmin) ResetMFA(ctx context.Context, realmName string, userID uuid.UUID, requiresMFA bool) error {
	return m.ResetMFAFn(ctx, realmName, userID, requiresMFA)
}
func (m *fakeKeycloakAdmin) CreateOrUpdateIdp(ctx context.Context, realmName string, idp port.IdpKeycloakConfig) error {
	return m.CreateOrUpdateIdpFn(ctx, realmName, idp)
}
func (m *fakeKeycloakAdmin) DeleteIdp(ctx context.Context, realmName, alias string) error {
	return m.DeleteIdpFn(ctx, realmName, alias)
}
func (m *fakeKeycloakAdmin) ApplyRealmConfig(ctx context.Context, realmName, settingKey string, desiredValue any) error {
	return m.ApplyRealmConfigFn(ctx, realmName, settingKey, desiredValue)
}
func (m *fakeKeycloakAdmin) CreateServiceAccountClient(ctx context.Context, realmName, clientID, jwksURL string) (uuid.UUID, bool, error) {
	return m.CreateServiceAccountClientFn(ctx, realmName, clientID, jwksURL)
}
func (m *fakeKeycloakAdmin) ClearServiceAccountKeysCache(ctx context.Context, realmName string) error {
	return m.ClearServiceAccountKeysCacheFn(ctx, realmName)
}
func (m *fakeKeycloakAdmin) DeleteServiceAccountClientIfExists(ctx context.Context, realmName, clientID string) error {
	return m.DeleteServiceAccountClientIfExistsFn(ctx, realmName, clientID)
}

// ── fakeTenantRealmRepository — in-memory, keyed by tenant_id. ──────────────

type fakeTenantRealmRepository struct {
	rows map[uuid.UUID]*domain.TenantRealm
	// UpdateFn, when set, overrides Update's default in-memory write — used
	// to inject e.g. ErrOptimisticLockConflict, which the real Postgres
	// repository can return but this in-memory fake otherwise never would.
	UpdateFn func(ctx context.Context, realm *domain.TenantRealm) error
}

var _ port.TenantRealmRepository = (*fakeTenantRealmRepository)(nil)

func (m *fakeTenantRealmRepository) FindByTenantID(ctx context.Context, tenantID uuid.UUID) (*domain.TenantRealm, error) {
	if r, ok := m.rows[tenantID]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, domain.NewError(domain.ErrTenantRealmNotFound, "not found")
}
func (m *fakeTenantRealmRepository) Upsert(ctx context.Context, realm *domain.TenantRealm) error {
	cp := *realm
	cp.RecordVersion++
	m.rows[realm.TenantID] = &cp
	return nil
}
func (m *fakeTenantRealmRepository) Update(ctx context.Context, realm *domain.TenantRealm) error {
	if m.UpdateFn != nil {
		return m.UpdateFn(ctx, realm)
	}
	cp := *realm
	m.rows[realm.TenantID] = &cp
	return nil
}
func (m *fakeTenantRealmRepository) ListExpiredTrials(ctx context.Context, limit int) ([]*domain.TenantRealm, error) {
	var out []*domain.TenantRealm
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
}
func (m *fakeTenantRealmRepository) ListSuspendedPastRetention(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
	var out []*domain.TenantRealm
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
}
func (m *fakeTenantRealmRepository) IsTombstoned(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	r, ok := m.rows[tenantID]
	return ok && r.DeletedAt != nil, nil
}
func (m *fakeTenantRealmRepository) CountsByShard(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	for _, r := range m.rows {
		if r.Strategy == domain.RealmStrategyDedicated && r.KeycloakShard != nil {
			out[*r.KeycloakShard]++
		}
	}
	return out, nil
}

// ── fakeIdempotencyStore — in-memory. ────────────────────────────────────────

type fakeIdempotencyStore struct {
	rows map[string]*domain.ProvisioningOperation
}

var _ port.IdempotencyStore = (*fakeIdempotencyStore)(nil)

func (m *fakeIdempotencyStore) Begin(ctx context.Context, key string, tenantID uuid.UUID, operation string) (*domain.ProvisioningOperation, bool, error) {
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
func (m *fakeIdempotencyStore) Complete(ctx context.Context, key string, result json.RawMessage) error {
	if row, ok := m.rows[key]; ok {
		row.Status = domain.OpStatusSucceeded
		row.Result = result
	}
	return nil
}
func (m *fakeIdempotencyStore) Fail(ctx context.Context, key string) error {
	if row, ok := m.rows[key]; ok {
		row.Status = domain.OpStatusFailed
	}
	return nil
}

// ── fakeTxRunner — runs fn directly, injecting a fakeEventPublisher that
// captures every enqueued event for assertions. ─────────────────────────────

type fakeEventPublisher struct {
	events []*domain.Event
}

func (p *fakeEventPublisher) Enqueue(ctx context.Context, event *domain.Event) error {
	p.events = append(p.events, event)
	return nil
}

type fakeTxRunner struct {
	pub    *fakeEventPublisher
	FailFn func() error
}

var _ port.TxRunner = (*fakeTxRunner)(nil)

func (r *fakeTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if r.FailFn != nil {
		if err := r.FailFn(); err != nil {
			return err
		}
	}
	ctx = port.WithEventPublisher(ctx, r.pub)
	return fn(ctx)
}

// ── fakeIdpConfigRepository — in-memory, keyed by (tenant_id, alias). ───────

type idpConfigKey struct {
	tenantID uuid.UUID
	alias    string
}

type fakeIdpConfigRepository struct {
	rows map[idpConfigKey]*domain.IdpConfig
	// ListAllFn, when set, overrides ListAll's default in-memory scan.
	ListAllFn func(ctx context.Context) ([]*domain.IdpConfig, error)
	// CountActiveByTenantFn, when set, overrides CountActiveByTenant —
	// use this to inject a repository error into RP-11 tests.
	CountActiveByTenantFn func(ctx context.Context, tenantID uuid.UUID) (int, error)
}

var _ port.IdpConfigRepository = (*fakeIdpConfigRepository)(nil)

func (m *fakeIdpConfigRepository) FindByAlias(ctx context.Context, tenantID uuid.UUID, alias string) (*domain.IdpConfig, error) {
	if r, ok := m.rows[idpConfigKey{tenantID, alias}]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, domain.NewError(domain.ErrIdpConfigNotFound, "not found")
}
func (m *fakeIdpConfigRepository) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*domain.IdpConfig, error) {
	var out []*domain.IdpConfig
	for k, r := range m.rows {
		if k.tenantID == tenantID {
			out = append(out, r)
		}
	}
	return out, nil
}
func (m *fakeIdpConfigRepository) Upsert(ctx context.Context, cfg *domain.IdpConfig) error {
	cp := *cfg
	m.rows[idpConfigKey{cfg.TenantID, cfg.IdpAlias}] = &cp
	return nil
}
func (m *fakeIdpConfigRepository) Delete(ctx context.Context, tenantID uuid.UUID, alias string) error {
	delete(m.rows, idpConfigKey{tenantID, alias})
	return nil
}
func (m *fakeIdpConfigRepository) ListAll(ctx context.Context) ([]*domain.IdpConfig, error) {
	if m.ListAllFn != nil {
		return m.ListAllFn(ctx)
	}
	var out []*domain.IdpConfig
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
}
func (m *fakeIdpConfigRepository) CountActiveByTenant(ctx context.Context, tenantID uuid.UUID) (int, error) {
	if m.CountActiveByTenantFn != nil {
		return m.CountActiveByTenantFn(ctx, tenantID)
	}
	n := 0
	for k, r := range m.rows {
		if k.tenantID == tenantID && r.Status == domain.IdpConfigStatusActive {
			n++
		}
	}
	return n, nil
}

// ── fakeIdpValidationRepository — append-only, in-memory. ───────────────────

type fakeIdpValidationRepository struct {
	rows []*domain.IdpConfigValidation
}

var _ port.IdpValidationRepository = (*fakeIdpValidationRepository)(nil)

func (m *fakeIdpValidationRepository) Insert(ctx context.Context, v *domain.IdpConfigValidation) error {
	m.rows = append(m.rows, v)
	return nil
}
func (m *fakeIdpValidationRepository) LatestForAlias(ctx context.Context, tenantID uuid.UUID, alias string) (*domain.IdpConfigValidation, error) {
	for i := len(m.rows) - 1; i >= 0; i-- {
		if m.rows[i].TenantID == tenantID && m.rows[i].IdpAlias == alias {
			return m.rows[i], nil
		}
	}
	return nil, domain.NewError(domain.ErrIdpConfigNotFound, "no validation found")
}

// ── fakeOpenBaoClient — in-memory KV, keyed by (tenant_id, alias). ──────────

type fakeOpenBaoClient struct {
	secrets map[idpConfigKey]map[string]any
	// WriteIdpSecretFn/DeleteIdpSecretFn, when set, override the default
	// in-memory behavior — used to inject e.g. ErrObjectStoreUnavailable.
	WriteIdpSecretFn  func(ctx context.Context, tenantID uuid.UUID, idpAlias string, secret map[string]any) error
	DeleteIdpSecretFn func(ctx context.Context, tenantID uuid.UUID, idpAlias string) error
}

var _ port.OpenBaoClient = (*fakeOpenBaoClient)(nil)

func (m *fakeOpenBaoClient) WriteIdpSecret(ctx context.Context, tenantID uuid.UUID, idpAlias string, secret map[string]any) error {
	if m.WriteIdpSecretFn != nil {
		return m.WriteIdpSecretFn(ctx, tenantID, idpAlias, secret)
	}
	m.secrets[idpConfigKey{tenantID, idpAlias}] = secret
	return nil
}
func (m *fakeOpenBaoClient) ReadIdpSecret(ctx context.Context, tenantID uuid.UUID, idpAlias string) (map[string]any, error) {
	if s, ok := m.secrets[idpConfigKey{tenantID, idpAlias}]; ok {
		return s, nil
	}
	return nil, domain.NewError(domain.ErrIdpConfigNotFound, "secret not found")
}
func (m *fakeOpenBaoClient) DeleteIdpSecret(ctx context.Context, tenantID uuid.UUID, idpAlias string) error {
	if m.DeleteIdpSecretFn != nil {
		return m.DeleteIdpSecretFn(ctx, tenantID, idpAlias)
	}
	delete(m.secrets, idpConfigKey{tenantID, idpAlias})
	return nil
}

// ── fakeReconciliationRepository — in-memory. ───────────────────────────────

type fakeReconciliationRepository struct {
	rows []*domain.RealmConfigReconciliation
}

var _ port.ReconciliationRepository = (*fakeReconciliationRepository)(nil)

func (m *fakeReconciliationRepository) Insert(ctx context.Context, r *domain.RealmConfigReconciliation) error {
	m.rows = append(m.rows, r)
	return nil
}
func (m *fakeReconciliationRepository) ListPending(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error) {
	return m.rows, nil
}
func (m *fakeReconciliationRepository) MarkApplied(ctx context.Context, id uuid.UUID) error {
	return nil
}
func (m *fakeReconciliationRepository) MarkFailed(ctx context.Context, id uuid.UUID, lastErr string, maxAttempts int) (domain.ReconStatus, error) {
	return domain.ReconStatusPending, nil
}

// ── fakeOrgMembershipClient. ─────────────────────────────────────────────────

type fakeOrgMembershipClient struct {
	HasResidualMFARequirementFn func(ctx context.Context, tenantID, userID uuid.UUID) (bool, error)
}

var _ port.OrgMembershipClient = (*fakeOrgMembershipClient)(nil)

func (m *fakeOrgMembershipClient) HasResidualMFARequirement(ctx context.Context, tenantID, userID uuid.UUID) (bool, error) {
	return m.HasResidualMFARequirementFn(ctx, tenantID, userID)
}

// ── fakeTokenServiceClient (§2.5, RP-17/RP-18). ──────────────────────────────

type fakeTokenServiceClient struct {
	RegisterPrincipalFn       func(ctx context.Context, tenantID uuid.UUID, req port.RegisterPrincipalRequest) (*port.RegisterPrincipalResult, error)
	IssueOrRotateCredentialFn func(ctx context.Context, tenantID, principalID uuid.UUID, req port.IssueCredentialRequest) (*port.IssueCredentialResult, error)
	RevokeCredentialFn        func(ctx context.Context, tenantID, principalID uuid.UUID) error
	ReadPrincipalFn           func(ctx context.Context, tenantID, principalID uuid.UUID) (*port.ReadPrincipalResult, error)
	JWKSURLForFn              func(tenantID uuid.UUID) string
}

var _ port.TokenServiceClient = (*fakeTokenServiceClient)(nil)

func (m *fakeTokenServiceClient) RegisterPrincipal(ctx context.Context, tenantID uuid.UUID, req port.RegisterPrincipalRequest) (*port.RegisterPrincipalResult, error) {
	return m.RegisterPrincipalFn(ctx, tenantID, req)
}
func (m *fakeTokenServiceClient) IssueOrRotateCredential(ctx context.Context, tenantID, principalID uuid.UUID, req port.IssueCredentialRequest) (*port.IssueCredentialResult, error) {
	return m.IssueOrRotateCredentialFn(ctx, tenantID, principalID, req)
}
func (m *fakeTokenServiceClient) RevokeCredential(ctx context.Context, tenantID, principalID uuid.UUID) error {
	if m.RevokeCredentialFn != nil {
		return m.RevokeCredentialFn(ctx, tenantID, principalID)
	}
	return nil
}
func (m *fakeTokenServiceClient) RevokeAllCredentials(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}
func (m *fakeTokenServiceClient) ReadPrincipal(ctx context.Context, tenantID, principalID uuid.UUID) (*port.ReadPrincipalResult, error) {
	return m.ReadPrincipalFn(ctx, tenantID, principalID)
}
func (m *fakeTokenServiceClient) JWKSURLFor(tenantID uuid.UUID) string {
	return m.JWKSURLForFn(tenantID)
}

// ── fakePinger — for router readyz tests. ───────────────────────────────────

type fakePinger struct {
	HealthFn func(ctx context.Context) error
}

var _ Pinger = (*fakePinger)(nil)

func (p *fakePinger) Health(ctx context.Context) error { return p.HealthFn(ctx) }

// fakePendingAuditEntryRepository accepts every enqueued AL-5 entry; the
// handler tests don't assert on audit delivery (test/unit covers it).
type fakePendingAuditEntryRepository struct{ enqueued []domain.AuditEntry }

var _ port.PendingAuditEntryRepository = (*fakePendingAuditEntryRepository)(nil)

func (f *fakePendingAuditEntryRepository) Enqueue(_ context.Context, e domain.AuditEntry) error {
	f.enqueued = append(f.enqueued, e)
	return nil
}

func (f *fakePendingAuditEntryRepository) ClaimDue(context.Context, int) ([]*domain.PendingAuditEntry, error) {
	return nil, nil
}
func (f *fakePendingAuditEntryRepository) MarkDelivered(context.Context, uuid.UUID) error { return nil }
func (f *fakePendingAuditEntryRepository) MarkRetry(context.Context, uuid.UUID, string) error {
	return nil
}
func (f *fakePendingAuditEntryRepository) MarkRetryAfter(context.Context, uuid.UUID, string, time.Duration) error {
	return nil
}
func (f *fakePendingAuditEntryRepository) MarkFailed(context.Context, uuid.UUID, string) error {
	return nil
}
