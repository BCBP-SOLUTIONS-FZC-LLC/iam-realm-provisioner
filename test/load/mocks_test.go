//go:build load

package load_test

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// The dependencies below are minimal in-memory stand-ins for
// ProvisioningService's non-Keycloak ports (TenantRealmRepository,
// IdempotencyStore, TxRunner) — the same shape as
// test/unit/mocks_test.go's mockTenantRealmRepository/
// mockIdempotencyStore/mockTxRunner, trimmed to what a load run needs (no
// error-injection knobs) and made goroutine-safe since a load run wants
// realistic contention-free bookkeeping, not concurrency itself (Convert's
// own migration loop is sequential).
//
// Only Keycloak is faked at the HTTP-transport level (fakeKeycloakServer,
// fake_keycloak_test.go) per the LLD §14.6 loopback-network/mock-Keycloak
// caveat — these three are faked at the Go-interface level instead,
// exactly like every other unit test of Convert, since RunInTx/Postgres
// behavior is not what this suite measures.

type loadTenantRealmRepository struct {
	mu   sync.Mutex
	rows map[uuid.UUID]*domain.TenantRealm
}

func newLoadTenantRealmRepository() *loadTenantRealmRepository {
	return &loadTenantRealmRepository{rows: map[uuid.UUID]*domain.TenantRealm{}}
}

var _ port.TenantRealmRepository = (*loadTenantRealmRepository)(nil)

func (m *loadTenantRealmRepository) seed(realm *domain.TenantRealm) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *realm
	m.rows[realm.TenantID] = &cp
}

func (m *loadTenantRealmRepository) FindByTenantID(_ context.Context, tenantID uuid.UUID) (*domain.TenantRealm, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[tenantID]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, domain.NewError(domain.ErrTenantRealmNotFound, "not found")
}

func (m *loadTenantRealmRepository) Upsert(_ context.Context, realm *domain.TenantRealm) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *realm
	cp.RecordVersion++
	m.rows[realm.TenantID] = &cp
	return nil
}

func (m *loadTenantRealmRepository) Update(_ context.Context, realm *domain.TenantRealm) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *realm
	m.rows[realm.TenantID] = &cp
	return nil
}

func (m *loadTenantRealmRepository) ListExpiredTrials(_ context.Context, _ int) ([]*domain.TenantRealm, error) {
	return nil, nil
}

func (m *loadTenantRealmRepository) ListSuspendedPastRetention(_ context.Context, _, _ int) ([]*domain.TenantRealm, error) {
	return nil, nil
}

func (m *loadTenantRealmRepository) CountsByShard(_ context.Context) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, r := range m.rows {
		if r.Strategy == domain.RealmStrategyDedicated && r.KeycloakShard != nil {
			out[*r.KeycloakShard]++
		}
	}
	return out, nil
}

type loadIdempotencyStore struct {
	mu   sync.Mutex
	rows map[string]*domain.ProvisioningOperation
}

func newLoadIdempotencyStore() *loadIdempotencyStore {
	return &loadIdempotencyStore{rows: map[string]*domain.ProvisioningOperation{}}
}

var _ port.IdempotencyStore = (*loadIdempotencyStore)(nil)

func (m *loadIdempotencyStore) Begin(_ context.Context, key string, tenantID uuid.UUID, operation string) (*domain.ProvisioningOperation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.rows[key]; ok {
		if existing.Status == domain.OpStatusFailed {
			// Mirrors IdempotencyRepository.Begin's ON CONFLICT DO UPDATE
			// ... WHERE status='failed' reclaim.
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

func (m *loadIdempotencyStore) Complete(_ context.Context, key string, result json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row, ok := m.rows[key]; ok {
		row.Status = domain.OpStatusSucceeded
		row.Result = result
	}
	return nil
}

func (m *loadIdempotencyStore) Fail(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row, ok := m.rows[key]; ok {
		row.Status = domain.OpStatusFailed
	}
	return nil
}

// loadEventPublisher discards every enqueued event — this suite measures
// the Keycloak-facing migration loop, not outbox behavior.
type loadEventPublisher struct{}

func (loadEventPublisher) Enqueue(_ context.Context, _ *domain.Event) error { return nil }

type loadTxRunner struct{ pub loadEventPublisher }

var _ port.TxRunner = (*loadTxRunner)(nil)

func (r *loadTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(port.WithEventPublisher(ctx, r.pub))
}

// noopLogger implements port.Logger without emitting anything — the
// per-call WARN paths Convert exercises on non-fatal Keycloak errors
// (e.g. ForceLogoutRealm) aren't interesting to this suite's assertions.
type noopLogger struct{}

func (noopLogger) Debug(string, map[string]any) {}
func (noopLogger) Info(string, map[string]any)  {}
func (noopLogger) Warn(string, map[string]any)  {}
func (noopLogger) Error(string, map[string]any) {}

var _ port.Logger = noopLogger{}
