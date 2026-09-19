package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

func newExpiredTrialRow(tenantID uuid.UUID, trialEndsAt time.Time) *domain.TenantRealm {
	return &domain.TenantRealm{
		TenantID:    tenantID,
		RealmName:   "trial",
		Strategy:    domain.RealmStrategyShared,
		Status:      domain.RealmStatusActive,
		TrialEndsAt: &trialEndsAt,
	}
}

func TestTrialExpiry_HappyPath_DisablesAndEmits(t *testing.T) {
	jctx, kc, realms, _, pub, _ := newTestContext()
	tenantA := newExpiredTrialRow(uuid.New(), time.Now().UTC().Add(-2*time.Hour))
	tenantB := newExpiredTrialRow(uuid.New(), time.Now().UTC().Add(-1*time.Hour))

	var gotLimit int
	realms.ListExpiredTrialsFn = func(ctx context.Context, limit int) ([]*domain.TenantRealm, error) {
		gotLimit = limit
		return []*domain.TenantRealm{tenantA, tenantB}, nil
	}
	var disabledTenants []uuid.UUID
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error {
		assert.Equal(t, "trial", realmName)
		disabledTenants = append(disabledTenants, tid)
		return nil
	}
	jctx.BatchLimit = 50
	jctx.TrialGraceDays = 14

	res, err := TrialExpiry(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, 50, gotLimit)
	assert.Equal(t, Result{Attempted: 2, Succeeded: 2, Failed: 0, Skipped: 0}, res)
	assert.ElementsMatch(t, []uuid.UUID{tenantA.TenantID, tenantB.TenantID}, disabledTenants)

	assert.Equal(t, domain.RealmStatusDisabled, tenantA.Status)
	assert.NotNil(t, tenantA.DisabledAt)
	assert.Equal(t, domain.RealmStatusDisabled, tenantB.Status)

	require.Len(t, pub.events, 2)
	for _, ev := range pub.events {
		assert.Equal(t, domain.EventTrialExpired, ev.Type)
		assert.Equal(t, domain.SystemActorID, ev.Actor)
		payload := ev.Data.(domain.TrialExpiredPayload)
		assert.NotEmpty(t, payload.TrialEndsAt)
		assert.NotEmpty(t, payload.GraceEndsAt)
	}
}

// When the TxRunner wires no event publisher into ctx, the local commit
// still lands and the tenant still counts as succeeded — just with no
// TrialExpired event enqueued.
func TestTrialExpiry_NoEventPublisherInContext_StillCommitsLocally(t *testing.T) {
	jctx, kc, realms, _, pub, _ := newTestContext()
	jctx.TxRunner = &mockTxRunner{pub: pub, SkipPublisher: true}
	row := newExpiredTrialRow(uuid.New(), time.Now().UTC().Add(-1*time.Hour))
	realms.ListExpiredTrialsFn = func(ctx context.Context, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{row}, nil
	}
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error { return nil }
	jctx.BatchLimit = 50
	jctx.TrialGraceDays = 14

	res, err := TrialExpiry(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res)
	assert.Empty(t, pub.events, "no event publisher in context means no event, but the tenant still succeeds")
}

func TestTrialExpiry_IdempotencyReplay_SkipsKeycloak(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	row := newExpiredTrialRow(uuid.New(), time.Now().UTC().Add(-1*time.Hour))
	realms.ListExpiredTrialsFn = func(ctx context.Context, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{row}, nil
	}
	key := "trial-expiry:" + row.TenantID.String() + ":" + row.TrialEndsAt.UTC().Format(time.RFC3339)
	idem.rows[key] = &domain.ProvisioningOperation{IdempotencyKey: key, Status: domain.OpStatusSucceeded}
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error {
		t.Fatal("must not call Keycloak on an idempotency replay")
		return nil
	}

	res, err := TrialExpiry(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res)
	assert.Equal(t, domain.RealmStatusActive, row.Status, "the row must not have been mutated on replay")
	assert.Empty(t, pub.events)
}

func TestTrialExpiry_KeycloakFailureMidLoop_OthersStillProcessed(t *testing.T) {
	jctx, kc, realms, _, pub, logger := newTestContext()
	failing := newExpiredTrialRow(uuid.New(), time.Now().UTC().Add(-2*time.Hour))
	ok := newExpiredTrialRow(uuid.New(), time.Now().UTC().Add(-1*time.Hour))
	realms.ListExpiredTrialsFn = func(ctx context.Context, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{failing, ok}, nil
	}
	before := testutil.ToFloat64(metrics.TrialExpiryDeferredTotal)
	beforeOK := testutil.ToFloat64(metrics.TrialExpiryProcessedTotal)
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error {
		if tid == failing.TenantID {
			return domain.NewError(domain.ErrKeycloakUnavailable, "down")
		}
		return nil
	}

	res, err := TrialExpiry(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 2, Succeeded: 1, Failed: 1}, res)
	assert.Equal(t, domain.RealmStatusActive, failing.Status, "the failing tenant's row must not have been mutated")
	assert.Equal(t, domain.RealmStatusDisabled, ok.Status)
	require.Len(t, pub.events, 1)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
	assert.Equal(t, before+1, testutil.ToFloat64(metrics.TrialExpiryDeferredTotal))
	assert.Equal(t, beforeOK+1, testutil.ToFloat64(metrics.TrialExpiryProcessedTotal))
}

func TestTrialExpiry_ListExpiredTrialsFails_Propagates(t *testing.T) {
	jctx, _, realms, _, _, _ := newTestContext()
	realms.ListExpiredTrialsFn = func(ctx context.Context, limit int) ([]*domain.TenantRealm, error) {
		return nil, errors.New("db down")
	}

	_, err := TrialExpiry(context.Background(), jctx)
	require.Error(t, err)
}

// A hard ledger failure from idempotency.Begin itself (not a `found`
// replay) must be counted as a per-tenant failure, not abort the whole
// sweep — the same defer-and-continue shape every other per-tenant failure
// in this job uses.
func TestTrialExpiry_BeginHardError_CountedAsFailed(t *testing.T) {
	jctx, _, realms, idem, pub, logger := newTestContext()
	row := newExpiredTrialRow(uuid.New(), time.Now().UTC().Add(-1*time.Hour))
	realms.ListExpiredTrialsFn = func(ctx context.Context, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{row}, nil
	}
	idem.BeginErr = errors.New("ledger unavailable")

	res, err := TrialExpiry(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	assert.Empty(t, pub.events)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

func TestTrialExpiry_UpdateFails_MarksIdempotencyFailed(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	row := newExpiredTrialRow(uuid.New(), time.Now().UTC().Add(-1*time.Hour))
	realms.ListExpiredTrialsFn = func(ctx context.Context, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{row}, nil
	}
	realms.UpdateFn = func(ctx context.Context, realm *domain.TenantRealm) error {
		return errors.New("db down")
	}
	kc.DisableUsersInRealmFn = func(ctx context.Context, realmName string, tid uuid.UUID) error { return nil }

	res, err := TrialExpiry(context.Background(), jctx)
	require.NoError(t, err) // TrialExpiry itself never returns a hard error mid-loop
	assert.Equal(t, Result{Attempted: 1, Succeeded: 0, Failed: 1}, res)
	key := "trial-expiry:" + row.TenantID.String() + ":" + row.TrialEndsAt.UTC().Format(time.RFC3339)
	assert.Equal(t, domain.OpStatusFailed, idem.rows[key].Status)
	assert.Empty(t, pub.events)
}

func TestTrialExpiry_NoRows_ZeroResult(t *testing.T) {
	jctx, _, realms, _, _, _ := newTestContext()
	realms.ListExpiredTrialsFn = func(ctx context.Context, limit int) ([]*domain.TenantRealm, error) {
		return nil, nil
	}
	res, err := TrialExpiry(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{}, res)
}
