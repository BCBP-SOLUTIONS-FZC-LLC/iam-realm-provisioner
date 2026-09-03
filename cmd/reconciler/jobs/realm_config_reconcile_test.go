package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

func newPendingReconciliation(tenantID uuid.UUID, settingKey string, desired json.RawMessage) *domain.RealmConfigReconciliation {
	return &domain.RealmConfigReconciliation{
		ID:           uuid.New(),
		TenantID:     tenantID,
		SettingKey:   settingKey,
		DesiredValue: desired,
	}
}

func TestRealmConfigReconcile_HappyPath_AppliesAndMarks(t *testing.T) {
	jctx, kc, realms, _, _, _ := newTestContext()
	tenantID := uuid.New()
	rec := newPendingReconciliation(tenantID, "local_accounts_enabled", json.RawMessage(`true`))
	recon := &mockReconciliationRepository{}
	jctx.Reconciliations = recon

	var gotLimit int
	recon.ListPendingFn = func(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error) {
		gotLimit = limit
		return []*domain.RealmConfigReconciliation{rec}, nil
	}
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		assert.Equal(t, tenantID, tid)
		return &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x"}, nil
	}
	var gotRealm, gotKey string
	var gotValue any
	kc.ApplyRealmConfigFn = func(ctx context.Context, realmName, settingKey string, desiredValue any) error {
		gotRealm, gotKey, gotValue = realmName, settingKey, desiredValue
		return nil
	}
	var markedApplied uuid.UUID
	recon.MarkAppliedFn = func(ctx context.Context, id uuid.UUID) error {
		markedApplied = id
		return nil
	}
	jctx.BatchLimit = 25

	res, err := RealmConfigReconcile(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, 25, gotLimit)
	assert.Equal(t, "tenant-x", gotRealm)
	assert.Equal(t, "local_accounts_enabled", gotKey)
	assert.Equal(t, true, gotValue)
	assert.Equal(t, rec.ID, markedApplied)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res)
}

func TestRealmConfigReconcile_TenantLookupFails_LoggedAndMarkedFailed(t *testing.T) {
	jctx, _, realms, _, _, logger := newTestContext()
	tenantID := uuid.New()
	rec := newPendingReconciliation(tenantID, "local_accounts_enabled", json.RawMessage(`true`))
	recon := &mockReconciliationRepository{}
	jctx.Reconciliations = recon
	recon.ListPendingFn = func(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error) {
		return []*domain.RealmConfigReconciliation{rec}, nil
	}
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		return nil, domain.NewError(domain.ErrTenantRealmNotFound, "not found")
	}
	var markFailedID uuid.UUID
	var markFailedErr string
	recon.MarkFailedFn = func(ctx context.Context, id uuid.UUID, lastErr string, maxAttempts int) (domain.ReconStatus, error) {
		markFailedID, markFailedErr = id, lastErr
		return domain.ReconStatusPending, nil
	}

	res, err := RealmConfigReconcile(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	assert.Equal(t, rec.ID, markFailedID)
	assert.NotEmpty(t, markFailedErr)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

func TestRealmConfigReconcile_ApplyRealmConfigFails_LeftPendingViaMarkFailed(t *testing.T) {
	jctx, kc, realms, _, _, logger := newTestContext()
	tenantID := uuid.New()
	rec := newPendingReconciliation(tenantID, "local_accounts_enabled", json.RawMessage(`false`))
	recon := &mockReconciliationRepository{}
	jctx.Reconciliations = recon
	recon.ListPendingFn = func(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error) {
		return []*domain.RealmConfigReconciliation{rec}, nil
	}
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		return &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x"}, nil
	}
	kc.ApplyRealmConfigFn = func(ctx context.Context, realmName, settingKey string, desiredValue any) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}
	var markFailedCalled bool
	recon.MarkFailedFn = func(ctx context.Context, id uuid.UUID, lastErr string, maxAttempts int) (domain.ReconStatus, error) {
		markFailedCalled = true
		assert.Equal(t, rec.ID, id)
		return domain.ReconStatusPending, nil
	}
	recon.MarkAppliedFn = func(ctx context.Context, id uuid.UUID) error {
		t.Fatal("must not mark applied when the Keycloak apply itself failed")
		return nil
	}

	res, err := RealmConfigReconcile(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	assert.True(t, markFailedCalled)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

func TestRealmConfigReconcile_InvalidDesiredValueJSON_SkipsKeycloak(t *testing.T) {
	jctx, kc, realms, _, _, _ := newTestContext()
	tenantID := uuid.New()
	rec := newPendingReconciliation(tenantID, "local_accounts_enabled", json.RawMessage(`not-json{`))
	recon := &mockReconciliationRepository{}
	jctx.Reconciliations = recon
	recon.ListPendingFn = func(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error) {
		return []*domain.RealmConfigReconciliation{rec}, nil
	}
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		return &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x"}, nil
	}
	kc.ApplyRealmConfigFn = func(ctx context.Context, realmName, settingKey string, desiredValue any) error {
		t.Fatal("must not call Keycloak when desired_value JSON is invalid")
		return nil
	}
	var markFailedErr string
	recon.MarkFailedFn = func(ctx context.Context, id uuid.UUID, lastErr string, maxAttempts int) (domain.ReconStatus, error) {
		markFailedErr = lastErr
		return domain.ReconStatusPending, nil
	}

	res, err := RealmConfigReconcile(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	assert.Contains(t, markFailedErr, "invalid desired_value JSON")
}

func TestRealmConfigReconcile_MarkAppliedFails_CountsAsFailed(t *testing.T) {
	jctx, kc, realms, _, _, _ := newTestContext()
	tenantID := uuid.New()
	rec := newPendingReconciliation(tenantID, "local_accounts_enabled", json.RawMessage(`true`))
	recon := &mockReconciliationRepository{}
	jctx.Reconciliations = recon
	recon.ListPendingFn = func(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error) {
		return []*domain.RealmConfigReconciliation{rec}, nil
	}
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		return &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x"}, nil
	}
	kc.ApplyRealmConfigFn = func(ctx context.Context, realmName, settingKey string, desiredValue any) error { return nil }
	recon.MarkAppliedFn = func(ctx context.Context, id uuid.UUID) error {
		return errors.New("ledger write failed")
	}

	res, err := RealmConfigReconcile(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
}

// markReconciliationFailed's own MarkFailed write failing is best-effort —
// logged, not escalated; the row simply stays pending for the next tick.
func TestRealmConfigReconcile_MarkFailedItselfFails_LoggedNotEscalated(t *testing.T) {
	jctx, kc, realms, _, _, logger := newTestContext()
	tenantID := uuid.New()
	rec := newPendingReconciliation(tenantID, "local_accounts_enabled", json.RawMessage(`true`))
	recon := &mockReconciliationRepository{}
	jctx.Reconciliations = recon
	recon.ListPendingFn = func(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error) {
		return []*domain.RealmConfigReconciliation{rec}, nil
	}
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		return &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x"}, nil
	}
	kc.ApplyRealmConfigFn = func(ctx context.Context, realmName, settingKey string, desiredValue any) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}
	recon.MarkFailedFn = func(ctx context.Context, id uuid.UUID, lastErr string, maxAttempts int) (domain.ReconStatus, error) {
		return "", errors.New("ledger write failed")
	}

	res, err := RealmConfigReconcile(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	assert.GreaterOrEqual(t, logger.warnCalls, 2, "both the apply failure and MarkFailed's own failure should log a WARN")
}

// RP-C4 poison-pill cap (§8.10): once MarkFailed reports the row moved to
// the terminal 'failed' status (attempts reached MaxReconciliationAttempts),
// the job must count it as Abandoned and log distinctly from an ordinary
// still-pending failure, so a permanently-broken setting stops silently
// consuming a ListPending slot every tick forever.
func TestRealmConfigReconcile_MaxAttemptsReached_CountsAbandoned(t *testing.T) {
	jctx, kc, realms, _, _, logger := newTestContext()
	tenantID := uuid.New()
	rec := newPendingReconciliation(tenantID, "local_accounts_enabled", json.RawMessage(`true`))
	rec.Attempts = 19
	recon := &mockReconciliationRepository{}
	jctx.Reconciliations = recon
	jctx.MaxReconciliationAttempts = 20
	recon.ListPendingFn = func(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error) {
		return []*domain.RealmConfigReconciliation{rec}, nil
	}
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		return &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-x"}, nil
	}
	kc.ApplyRealmConfigFn = func(ctx context.Context, realmName, settingKey string, desiredValue any) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	}
	var gotMaxAttempts int
	recon.MarkFailedFn = func(ctx context.Context, id uuid.UUID, lastErr string, maxAttempts int) (domain.ReconStatus, error) {
		gotMaxAttempts = maxAttempts
		return domain.ReconStatusFailed, nil
	}

	res, err := RealmConfigReconcile(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, 20, gotMaxAttempts)
	assert.Equal(t, Result{Attempted: 1, Failed: 1, Abandoned: 1}, res)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

func TestRealmConfigReconcile_ListPendingFails_Propagates(t *testing.T) {
	jctx, _, _, _, _, _ := newTestContext()
	recon := &mockReconciliationRepository{
		ListPendingFn: func(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error) {
			return nil, errors.New("db down")
		},
	}
	jctx.Reconciliations = recon

	_, err := RealmConfigReconcile(context.Background(), jctx)
	require.Error(t, err)
}

func TestRealmConfigReconcile_NoPending_ZeroResult(t *testing.T) {
	jctx, _, _, _, _, _ := newTestContext()
	recon := &mockReconciliationRepository{
		ListPendingFn: func(ctx context.Context, limit int) ([]*domain.RealmConfigReconciliation, error) {
			return nil, nil
		},
	}
	jctx.Reconciliations = recon

	res, err := RealmConfigReconcile(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{}, res)
}
