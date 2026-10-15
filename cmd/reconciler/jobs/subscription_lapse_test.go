package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

func newLapsed(tenantID uuid.UUID, realmType string) port.LapsedSubscription {
	return port.LapsedSubscription{
		TenantID:    tenantID,
		RealmID:     "kc-" + tenantID.String(),
		RealmType:   realmType,
		CancelledAt: time.Now().UTC().Add(-45 * 24 * time.Hour),
	}
}

func newActiveDedicatedRealm(tenantID uuid.UUID, realmName string) *domain.TenantRealm {
	shard := "shard-1"
	return &domain.TenantRealm{
		TenantID: tenantID, RealmName: realmName, Strategy: domain.RealmStrategyDedicated,
		KeycloakShard: &shard, Status: domain.RealmStatusActive,
	}
}

func newSuspendedForOffboard(tenantID uuid.UUID, realmName string) *domain.TenantRealm {
	shard := "shard-1"
	past := time.Now().UTC().Add(-40 * 24 * time.Hour)
	source := domain.SuspensionSourceBillingLapse
	return &domain.TenantRealm{
		TenantID: tenantID, RealmName: realmName, Strategy: domain.RealmStrategyDedicated,
		KeycloakShard: &shard, Status: domain.RealmStatusSuspended,
		DisabledAt: &past, SuspensionSource: &source,
	}
}

func noOffboardCandidates(jctx *Context) {
	jctx.TenantRealms.(*mockTenantRealmRepository).ListSuspendedPastRetentionFn =
		func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
			return nil, nil
		}
}

// ── SubscriptionLapses == nil: suspend phase is skipped with a WARN, but
// the offboard phase runs unconditionally (documented invariant §8.8/§8.9).
// A missing ORG_MEMBERSHIP_BASE_URL must not block GDPR-retention offboarding
// of tenants already past their suspension window. ──────────────────────────

func TestSubscriptionLapse_NilReader_SkipsSuspendRunsOffboard(t *testing.T) {
	jctx, _, _, _, _, _ := newTestContext()
	jctx.SubscriptionLapses = nil
	// Wire the offboard query to return empty so the phase completes cleanly.
	noOffboardCandidates(jctx)

	_, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err, "nil SubscriptionLapses must not block the offboard phase")
}

// ── Suspend phase ──────────────────────────────────────────────────────────

func TestSubscriptionLapse_Suspend_DedicatedProcessed_SharedSkipped(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	dedicatedA := uuid.New()
	dedicatedB := uuid.New()
	shared := uuid.New()
	lapses := &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return []port.LapsedSubscription{
				newLapsed(dedicatedA, "dedicated"),
				newLapsed(shared, "shared"),
				newLapsed(dedicatedB, "dedicated"),
			}, nil
		},
	}
	jctx.SubscriptionLapses = lapses
	realmsByTenant := map[uuid.UUID]*domain.TenantRealm{
		dedicatedA: newActiveDedicatedRealm(dedicatedA, "tenant-a"),
		dedicatedB: newActiveDedicatedRealm(dedicatedB, "tenant-b"),
	}
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		r, ok := realmsByTenant[tid]
		require.True(t, ok, "shared tenant must never be looked up")
		return r, nil
	}
	var disabled []string
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error {
		disabled = append(disabled, realmName)
		return nil
	}
	noOffboardCandidates(jctx)

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 2, Succeeded: 2, Skipped: 1}, res)
	assert.ElementsMatch(t, []string{"tenant-a", "tenant-b"}, disabled)

	assert.Equal(t, domain.RealmStatusSuspended, realmsByTenant[dedicatedA].Status)
	require.NotNil(t, realmsByTenant[dedicatedA].SuspensionSource)
	assert.Equal(t, domain.SuspensionSourceBillingLapse, *realmsByTenant[dedicatedA].SuspensionSource)

	require.Len(t, pub.events, 2)
	for _, ev := range pub.events {
		assert.Equal(t, domain.EventTenantSuspended, ev.Type)
		payload := ev.Data.(domain.TenantSuspendedPayload)
		assert.Equal(t, domain.SuspensionSourceBillingLapse, payload.Source)
	}
	assert.Contains(t, idem.rows, "lapse-suspend:"+dedicatedA.String())
	assert.Contains(t, idem.rows, "lapse-suspend:"+dedicatedB.String())
}

// TestSubscriptionLapse_Suspend_UnexpectedRealmType_SkippedWithDistinguishingWarn
// pins the fail-safe behavior for a realm_type O&M has never sent before —
// never suspend on ambiguous input — while also proving it's distinguishable
// from a routine "shared" skip in the logs, so a genuinely-dedicated tenant
// misreported by schema drift doesn't vanish silently into the Skipped count.
func TestSubscriptionLapse_Suspend_UnexpectedRealmType_SkippedWithDistinguishingWarn(t *testing.T) {
	jctx, kc, realms, _, _, logger := newTestContext()
	weird := uuid.New()
	lapses := &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return []port.LapsedSubscription{newLapsed(weird, "")}, nil
		},
	}
	jctx.SubscriptionLapses = lapses
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		t.Fatal("an unexpected realm_type must never be looked up or suspended")
		return nil, nil
	}
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error {
		t.Fatal("an unexpected realm_type must never reach Keycloak")
		return nil
	}
	noOffboardCandidates(jctx)

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Skipped: 1}, res)
	require.NotEmpty(t, logger.warnMsgs)
	assert.Contains(t, logger.warnMsgs[0], "unexpected realm_type")
}

func TestSubscriptionLapse_Suspend_AlreadySuspendedOrOffboarded_NoOp(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	alreadySuspended := uuid.New()
	alreadyOffboarded := uuid.New()
	lapses := &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return []port.LapsedSubscription{
				newLapsed(alreadySuspended, "dedicated"),
				newLapsed(alreadyOffboarded, "dedicated"),
			}, nil
		},
	}
	jctx.SubscriptionLapses = lapses
	suspendedRow := newActiveDedicatedRealm(alreadySuspended, "tenant-sus")
	suspendedRow.Status = domain.RealmStatusSuspended
	offboardedRow := newActiveDedicatedRealm(alreadyOffboarded, "tenant-off")
	offboardedRow.Status = domain.RealmStatusDeleted
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		if tid == alreadySuspended {
			return suspendedRow, nil
		}
		return offboardedRow, nil
	}
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error {
		t.Fatal("must not call Keycloak for an already-acted-on tenant")
		return nil
	}
	noOffboardCandidates(jctx)

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 2, Succeeded: 2}, res)
	assert.Equal(t, 0, idem.BeginCalls, "a no-op on an already-acted-on tenant must never touch the idempotency ledger")
	assert.Empty(t, pub.events)
}

func TestSubscriptionLapse_Suspend_IdempotentReplay(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	tenantID := uuid.New()
	lapses := &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return []port.LapsedSubscription{newLapsed(tenantID, "dedicated")}, nil
		},
	}
	jctx.SubscriptionLapses = lapses
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		return newActiveDedicatedRealm(tenantID, "tenant-x"), nil
	}
	key := "lapse-suspend:" + tenantID.String()
	idem.rows[key] = &domain.ProvisioningOperation{IdempotencyKey: key, Status: domain.OpStatusSucceeded}
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error {
		t.Fatal("must not call Keycloak on an idempotency replay")
		return nil
	}
	noOffboardCandidates(jctx)

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res)
	assert.Empty(t, pub.events)
}

func TestSubscriptionLapse_Suspend_KeycloakFailure(t *testing.T) {
	jctx, kc, realms, idem, pub, logger := newTestContext()
	tenantID := uuid.New()
	lapses := &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return []port.LapsedSubscription{newLapsed(tenantID, "dedicated")}, nil
		},
	}
	jctx.SubscriptionLapses = lapses
	row := newActiveDedicatedRealm(tenantID, "tenant-x")
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) { return row, nil }
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	noOffboardCandidates(jctx)

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	assert.Equal(t, domain.RealmStatusActive, row.Status, "the row must not have transitioned")
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["lapse-suspend:"+tenantID.String()].Status)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

// A DB failure inside the local commit (after DisableRealm already
// succeeded) must mark the idempotency key failed, same as a Keycloak-side
// failure — Keycloak-effect-first (§9.1) means a committed row always
// reflects real Keycloak state, so a local-commit failure here must not be
// silently swallowed.
func TestSubscriptionLapse_Suspend_UpdateFails_MarksIdempotencyFailed(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	tenantID := uuid.New()
	lapses := &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return []port.LapsedSubscription{newLapsed(tenantID, "dedicated")}, nil
		},
	}
	jctx.SubscriptionLapses = lapses
	row := newActiveDedicatedRealm(tenantID, "tenant-x")
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) { return row, nil }
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error { return nil }
	realms.UpdateFn = func(ctx context.Context, r *domain.TenantRealm) error { return errors.New("db down") }
	noOffboardCandidates(jctx)

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["lapse-suspend:"+tenantID.String()].Status)
}

// When the TxRunner wires no event publisher into ctx, the local commit
// still lands and the tenant still counts as succeeded — just with no
// TenantSuspended event enqueued.
func TestSubscriptionLapse_Suspend_NoEventPublisherInContext_StillCommitsLocally(t *testing.T) {
	jctx, kc, realms, _, pub, _ := newTestContext()
	jctx.TxRunner = &mockTxRunner{pub: pub, SkipPublisher: true}
	tenantID := uuid.New()
	lapses := &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return []port.LapsedSubscription{newLapsed(tenantID, "dedicated")}, nil
		},
	}
	jctx.SubscriptionLapses = lapses
	row := newActiveDedicatedRealm(tenantID, "tenant-x")
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) { return row, nil }
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error { return nil }
	realms.UpdateFn = func(ctx context.Context, r *domain.TenantRealm) error { return nil }
	noOffboardCandidates(jctx)

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res)
	assert.Empty(t, pub.events, "no event publisher in context means no event, but the tenant still succeeds")
}

// TestSubscriptionLapse_Suspend_RetriedAfterPriorDayFailure pins the fix for
// a real bug: a tenant still returned by O&M's lapse list on a later day
// must actually be retried, not permanently skipped because a prior run's
// failed idempotency row makes every future Begin() on the same key look
// "already handled." idempotency_key is provisioning_operations' PRIMARY
// KEY and nothing ever prunes that table, so without IdempotencyRepository
// reclaiming a `failed` row, this tenant's realm would never actually get
// suspended — while the job's own telemetry kept reporting it as handled.
func TestSubscriptionLapse_Suspend_RetriedAfterPriorDayFailure(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	tenantID := uuid.New()
	key := "lapse-suspend:" + tenantID.String()
	// Simulate yesterday's run: Begin succeeded, DisableRealm failed, Fail
	// was called — exactly what TestSubscriptionLapse_Suspend_KeycloakFailure
	// above leaves behind.
	idem.rows[key] = &domain.ProvisioningOperation{IdempotencyKey: key, Status: domain.OpStatusFailed}

	lapses := &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return []port.LapsedSubscription{newLapsed(tenantID, "dedicated")}, nil
		},
	}
	jctx.SubscriptionLapses = lapses
	row := newActiveDedicatedRealm(tenantID, "tenant-x")
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) { return row, nil }
	var disabled bool
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error { disabled = true; return nil }
	noOffboardCandidates(jctx)

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res, "today's retry must actually run, not be silently skipped as already-handled")
	assert.True(t, disabled, "DisableRealm must actually be called on the retry")
	assert.Equal(t, domain.RealmStatusSuspended, row.Status)
	assert.Equal(t, domain.OpStatusSucceeded, idem.rows[key].Status)
	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTenantSuspended, pub.events[0].Type)
}

func TestSubscriptionLapse_Suspend_FindByTenantIDFails_LoggedAsFailure(t *testing.T) {
	jctx, kc, realms, _, pub, logger := newTestContext()
	tenantID := uuid.New()
	jctx.SubscriptionLapses = &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return []port.LapsedSubscription{newLapsed(tenantID, "dedicated")}, nil
		},
	}
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		return nil, errors.New("not found")
	}
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error {
		t.Fatal("must not call Keycloak when the tenant_realms lookup itself failed")
		return nil
	}
	noOffboardCandidates(jctx)

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	assert.Empty(t, pub.events)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

func TestSubscriptionLapse_Suspend_BeginHardError_LoggedAsFailure(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	tenantID := uuid.New()
	jctx.SubscriptionLapses = &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return []port.LapsedSubscription{newLapsed(tenantID, "dedicated")}, nil
		},
	}
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		return newActiveDedicatedRealm(tid, "tenant-x"), nil
	}
	idem.BeginErr = errors.New("ledger unavailable")
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error {
		t.Fatal("must not call Keycloak when Begin itself hard-fails")
		return nil
	}
	noOffboardCandidates(jctx)

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	assert.Empty(t, pub.events)
}

func TestSubscriptionLapse_Offboard_BeginHardError_LoggedAsFailure(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	tenantID := uuid.New()
	row := newSuspendedForOffboard(tenantID, "tenant-x")
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{row}, nil
	}
	idem.BeginErr = errors.New("ledger unavailable")
	kc.ExportRealmFn = func(ctx context.Context, realmName string) ([]byte, error) {
		t.Fatal("must not export when Begin itself hard-fails")
		return nil, nil
	}

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.RealmStatusSuspended, row.Status)
}

// ── BatchLimit shares its budget across both phases ─────────────────────────

func TestSubscriptionLapse_BatchLimit_ExhaustedBySuspend_OffboardNeverQueried(t *testing.T) {
	jctx, kc, realms, _, _, _ := newTestContext()
	jctx.BatchLimit = 2
	tenants := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	lapses := &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return []port.LapsedSubscription{
				newLapsed(tenants[0], "dedicated"),
				newLapsed(tenants[1], "dedicated"),
				newLapsed(tenants[2], "dedicated"),
			}, nil
		},
	}
	jctx.SubscriptionLapses = lapses
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		return newActiveDedicatedRealm(tid, "tenant-"+tid.String()), nil
	}
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error { return nil }
	// ListSuspendedPastRetentionFn deliberately left unset — calling it
	// would panic, proving the offboard phase's `remaining <= 0` guard
	// short-circuited before ever querying.

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 2, Succeeded: 2}, res, "the third lapsed tenant must never be attempted once BatchLimit is hit")
}

func TestSubscriptionLapse_BatchLimit_RemainingBudgetPassedToOffboard(t *testing.T) {
	jctx, kc, realms, _, _, _ := newTestContext()
	jctx.BatchLimit = 3
	dedicatedA, shared, dedicatedC := uuid.New(), uuid.New(), uuid.New()
	lapses := &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return []port.LapsedSubscription{
				newLapsed(dedicatedA, "dedicated"),
				newLapsed(shared, "shared"),
				newLapsed(dedicatedC, "dedicated"),
			}, nil
		},
	}
	jctx.SubscriptionLapses = lapses
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		return newActiveDedicatedRealm(tid, "tenant-"+tid.String()), nil
	}
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error { return nil }
	var gotRetention, gotLimit int
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		gotRetention, gotLimit = retentionDays, limit
		return nil, nil
	}
	jctx.RetentionDays = 30

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 2, Succeeded: 2, Skipped: 1}, res)
	assert.Equal(t, 30, gotRetention)
	assert.Equal(t, 1, gotLimit, "offboard must receive BatchLimit(3) - suspend's Attempted(2) = 1")
}

func TestSubscriptionLapse_Offboard_BatchLimitBreaksMidPhase(t *testing.T) {
	jctx, kc, realms, _, _, _ := newTestContext()
	jctx.BatchLimit = 1
	rowA := newSuspendedForOffboard(uuid.New(), "tenant-a")
	rowB := newSuspendedForOffboard(uuid.New(), "tenant-b")
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		assert.Equal(t, 1, limit, "remaining budget (BatchLimit - 0 suspended) must be passed through")
		// A real repository query already respects LIMIT, but a defensive
		// job-side break must still hold if it ever returned more than asked.
		return []*domain.TenantRealm{rowA, rowB}, nil
	}
	var exported []string
	kc.ExportRealmFn = func(ctx context.Context, realmName string) ([]byte, error) {
		exported = append(exported, realmName)
		return []byte("x"), nil
	}
	jctx.ObjectStore = &mockObjectStore{PutRealmExportFn: func(ctx context.Context, key string, data []byte) error { return nil }}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }
	realms.UpdateFn = func(ctx context.Context, realm *domain.TenantRealm) error { return nil }

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res)
	assert.Equal(t, []string{"tenant-a"}, exported, "the offboard loop's own BatchLimit break must stop after the first row")
}

// ── Offboard phase ─────────────────────────────────────────────────────────

func TestSubscriptionLapse_Offboard_HappyPath_CallOrderAndEvent(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	tenantID := uuid.New()
	row := newSuspendedForOffboard(tenantID, "tenant-x")
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{row}, nil
	}
	var callOrder []string
	exportBytes := []byte(`{"realm":"tenant-x"}`)
	kc.ExportRealmFn = func(ctx context.Context, realmName string) ([]byte, error) {
		callOrder = append(callOrder, "export")
		assert.Equal(t, "tenant-x", realmName)
		return exportBytes, nil
	}
	objStore := &mockObjectStore{}
	var putKey string
	var putData []byte
	objStore.PutRealmExportFn = func(ctx context.Context, key string, data []byte) error {
		callOrder = append(callOrder, "put")
		putKey, putData = key, data
		return nil
	}
	jctx.ObjectStore = objStore
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		callOrder = append(callOrder, "delete")
		assert.Equal(t, "tenant-x", realmName)
		return nil
	}
	realms.UpdateFn = func(ctx context.Context, realm *domain.TenantRealm) error {
		callOrder = append(callOrder, "update")
		return nil
	}

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res)
	assert.Equal(t, []string{"export", "put", "delete", "update"}, callOrder, "RP-EVT-4: export and durable write must precede delete")
	assert.True(t, strings.HasPrefix(putKey, "realm-exports/tenant-x/"))
	assert.True(t, strings.HasSuffix(putKey, ".json"))
	assert.Equal(t, exportBytes, putData)

	assert.Equal(t, domain.RealmStatusDeleted, row.Status)
	require.NotNil(t, row.ExportedAt)
	require.NotNil(t, row.ExportS3Key)
	assert.Equal(t, putKey, *row.ExportS3Key)

	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTenantOffboarded, pub.events[0].Type)
	payload := pub.events[0].Data.(domain.TenantOffboardedPayload)
	assert.Equal(t, tenantID, payload.TenantID)
	assert.Equal(t, putKey, payload.ExportS3Key)
	assert.Contains(t, idem.rows, "offboard:"+tenantID.String())
}

// TestSubscriptionLapse_Offboard_GDPRScrubFnCalled verifies that offboardTenant
// invokes GDPRScrubFn for the correct tenant ID before committing the
// realm-status update — a refactor that drops the call must be caught here.
func TestSubscriptionLapse_Offboard_GDPRScrubFnCalled(t *testing.T) {
	jctx, kc, realms, _, _, _ := newTestContext()
	tenantID := uuid.New()
	row := newSuspendedForOffboard(tenantID, "tenant-x")
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{row}, nil
	}
	kc.ExportRealmFn = func(ctx context.Context, realmName string) ([]byte, error) { return []byte("x"), nil }
	jctx.ObjectStore = &mockObjectStore{PutRealmExportFn: func(ctx context.Context, key string, data []byte) error { return nil }}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }
	realms.UpdateFn = func(ctx context.Context, realm *domain.TenantRealm) error { return nil }

	var scrubbedTenantIDs []uuid.UUID
	jctx.GDPRScrubFn = func(_ context.Context, tid uuid.UUID) error {
		scrubbedTenantIDs = append(scrubbedTenantIDs, tid)
		return nil
	}

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res)
	assert.Equal(t, []uuid.UUID{tenantID}, scrubbedTenantIDs, "GDPRScrubFn must be called exactly once for the offboarded tenant")
}

// TestSubscriptionLapse_Offboard_NilGDPRScrubFn_FailsHard verifies that
// offboardTenant returns a hard error (and fails the idempotency key) when
// GDPRScrubFn is nil — rather than silently committing TenantOffboarded with
// PII still in the database.
func TestSubscriptionLapse_Offboard_NilGDPRScrubFn_FailsHard(t *testing.T) {
	jctx, kc, realms, idem, _, _ := newTestContext()
	jctx.GDPRScrubFn = nil // simulate misconfigured Context
	tenantID := uuid.New()
	row := newSuspendedForOffboard(tenantID, "tenant-x")
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{row}, nil
	}
	kc.ExportRealmFn = func(ctx context.Context, realmName string) ([]byte, error) { return []byte("x"), nil }
	jctx.ObjectStore = &mockObjectStore{PutRealmExportFn: func(ctx context.Context, key string, data []byte) error { return nil }}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }
	// UpdateFn intentionally unset — should never be reached when scrub fails.

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err, "outer SubscriptionLapse must not propagate offboard errors (per-tenant deferral)")
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	// The idempotency key must be marked failed so no one replays a partial commit.
	require.Contains(t, idem.rows, "offboard:"+tenantID.String())
	assert.Equal(t, domain.OpStatusFailed, idem.rows["offboard:"+tenantID.String()].Status)
}

// When the TxRunner wires no event publisher into ctx, the local commit
// still lands and the tenant still counts as succeeded — just with no
// TenantOffboarded event enqueued.
func TestSubscriptionLapse_Offboard_NoEventPublisherInContext_StillCommitsLocally(t *testing.T) {
	jctx, kc, realms, _, pub, _ := newTestContext()
	jctx.TxRunner = &mockTxRunner{pub: pub, SkipPublisher: true}
	tenantID := uuid.New()
	row := newSuspendedForOffboard(tenantID, "tenant-x")
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{row}, nil
	}
	kc.ExportRealmFn = func(ctx context.Context, realmName string) ([]byte, error) { return []byte("x"), nil }
	jctx.ObjectStore = &mockObjectStore{PutRealmExportFn: func(ctx context.Context, key string, data []byte) error { return nil }}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }
	realms.UpdateFn = func(ctx context.Context, realm *domain.TenantRealm) error { return nil }

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res)
	assert.Empty(t, pub.events, "no event publisher in context means no event, but the tenant still succeeds")
}

func TestSubscriptionLapse_Offboard_ExportFails(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	tenantID := uuid.New()
	row := newSuspendedForOffboard(tenantID, "tenant-x")
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{row}, nil
	}
	before := testutil.ToFloat64(metrics.RealmExportTotal.WithLabelValues("fail"))
	kc.ExportRealmFn = func(ctx context.Context, realmName string) ([]byte, error) {
		return nil, domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	// ObjectStore and DeleteRealmIfExists left unset — calling either would
	// panic, proving delete never runs before a successful export (RP-EVT-4).
	jctx.ObjectStore = &mockObjectStore{}

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	assert.Equal(t, domain.RealmStatusSuspended, row.Status)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["offboard:"+tenantID.String()].Status)
	assert.Equal(t, before+1, testutil.ToFloat64(metrics.RealmExportTotal.WithLabelValues("fail")))
}

func TestSubscriptionLapse_Offboard_PutRealmExportFails(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	tenantID := uuid.New()
	row := newSuspendedForOffboard(tenantID, "tenant-x")
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{row}, nil
	}
	before := testutil.ToFloat64(metrics.RealmExportTotal.WithLabelValues("fail"))
	kc.ExportRealmFn = func(ctx context.Context, realmName string) ([]byte, error) { return []byte("x"), nil }
	jctx.ObjectStore = &mockObjectStore{
		PutRealmExportFn: func(ctx context.Context, key string, data []byte) error {
			return domain.NewError(domain.ErrObjectStoreUnavailable, "s3 down")
		},
	}
	// DeleteRealmIfExists left unset — must never be called after a failed
	// durable write.

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	assert.Equal(t, domain.RealmStatusSuspended, row.Status)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["offboard:"+tenantID.String()].Status)
	assert.Equal(t, before+1, testutil.ToFloat64(metrics.RealmExportTotal.WithLabelValues("fail")))
}

func TestSubscriptionLapse_Offboard_DeleteRealmIfExistsFails(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	tenantID := uuid.New()
	row := newSuspendedForOffboard(tenantID, "tenant-x")
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{row}, nil
	}
	beforeSuccess := testutil.ToFloat64(metrics.RealmExportTotal.WithLabelValues("success"))
	kc.ExportRealmFn = func(ctx context.Context, realmName string) ([]byte, error) { return []byte("x"), nil }
	jctx.ObjectStore = &mockObjectStore{
		PutRealmExportFn: func(ctx context.Context, key string, data []byte) error { return nil },
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "down")
	}
	// TenantRealms.UpdateFn left unset — a delete failure must never reach
	// the local commit.

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	assert.Equal(t, domain.RealmStatusSuspended, row.Status)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["offboard:"+tenantID.String()].Status)
	assert.Equal(t, beforeSuccess+1, testutil.ToFloat64(metrics.RealmExportTotal.WithLabelValues("success")), "export+put succeeded before the delete failure")
}

// A DB failure inside the local commit (after export+delete already
// succeeded) must mark the idempotency key failed — the realm is already
// gone from Keycloak at this point (RP-EVT-4: export-then-delete already
// happened), so this is a "local row now lags reality" failure mode, not a
// prevented mutation, but it must still be surfaced as Failed, not silently
// counted as Succeeded.
func TestSubscriptionLapse_Offboard_UpdateFails_MarksIdempotencyFailed(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	tenantID := uuid.New()
	row := newSuspendedForOffboard(tenantID, "tenant-x")
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{row}, nil
	}
	kc.ExportRealmFn = func(ctx context.Context, realmName string) ([]byte, error) { return []byte("x"), nil }
	jctx.ObjectStore = &mockObjectStore{
		PutRealmExportFn: func(ctx context.Context, key string, data []byte) error { return nil },
	}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }
	realms.UpdateFn = func(ctx context.Context, r *domain.TenantRealm) error { return errors.New("db down") }

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	assert.Empty(t, pub.events)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["offboard:"+tenantID.String()].Status)
}

func TestSubscriptionLapse_Offboard_IdempotentReplay(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	tenantID := uuid.New()
	row := newSuspendedForOffboard(tenantID, "tenant-x")
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		return []*domain.TenantRealm{row}, nil
	}
	key := "offboard:" + tenantID.String()
	idem.rows[key] = &domain.ProvisioningOperation{IdempotencyKey: key, Status: domain.OpStatusSucceeded}
	kc.ExportRealmFn = func(ctx context.Context, realmName string) ([]byte, error) {
		t.Fatal("must not export on an idempotency replay")
		return nil, nil
	}

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res)
	assert.Equal(t, domain.RealmStatusSuspended, row.Status)
	assert.Empty(t, pub.events)
}

// ── ListLapsedSubscriptions / ListSuspendedPastRetention hard failures ──────

func TestSubscriptionLapse_ListLapsedSubscriptionsFails_Propagates(t *testing.T) {
	jctx, _, _, _, _, _ := newTestContext()
	jctx.SubscriptionLapses = &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return nil, errors.New("O&M unavailable")
		},
	}

	_, err := SubscriptionLapse(context.Background(), jctx)
	require.Error(t, err)
}

func TestSubscriptionLapse_ListSuspendedPastRetentionFails_Propagates(t *testing.T) {
	jctx, _, realms, _, _, _ := newTestContext()
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		return nil, errors.New("db down")
	}

	_, err := SubscriptionLapse(context.Background(), jctx)
	require.Error(t, err)
}

// ── CD-23: RETENTION_DAYS boundary tests (offboard phase, TL decision 2026-10-15) ──
//
// RETENTION_DAYS = 60 (suspended → offboarded); total from cancellation is
// 90 days (CANCELLED_GRACE_DAYS 30 + RETENTION_DAYS 60).
//
// The actual SQL predicate (disabled_at < now() - retentionDays AND
// suspension_source='billing_lapse') is exercised in the postgres integration
// suite; these unit tests pin the contract that RetentionDays is forwarded
// correctly to ListSuspendedPastRetention, and that the phase acts on
// whatever the query returns.

// At day 59, the SQL query returns no rows (tenant is still within the 60-day
// window) — the offboard phase must attempt nothing.
func TestSubscriptionLapse_RetentionBoundary_Day59_NotOffboarded(t *testing.T) {
	jctx, kc, _, _, _, _ := newTestContext()
	jctx.RetentionDays = 60
	jctx.SubscriptionLapses = nil // suspend phase skipped; offboard phase is the focus
	var gotRetention int
	// Simulate SQL returning no rows: the tenant was disabled 59 days ago,
	// which is within the 60-day retention window (disabled_at >= now() - 60d).
	jctx.TenantRealms.(*mockTenantRealmRepository).ListSuspendedPastRetentionFn =
		func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
			gotRetention = retentionDays
			return nil, nil
		}
	kc.ExportRealmFn = func(ctx context.Context, realmName string) ([]byte, error) {
		t.Fatal("ExportRealm must not be called when no candidates are past retention")
		return nil, nil
	}

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{}, res, "tenant disabled 59 days ago is within RETENTION_DAYS=60 — nothing to offboard")
	assert.Equal(t, 60, gotRetention, "RetentionDays=60 (CD-23 TL decision) must be passed to ListSuspendedPastRetention")
}

// At day 60 the SQL query returns the tenant (disabled_at < now() - 60d) and
// the offboard phase must process it.
func TestSubscriptionLapse_RetentionBoundary_Day60_Offboarded(t *testing.T) {
	jctx, kc, realms, _, pub, _ := newTestContext()
	jctx.RetentionDays = 60
	jctx.SubscriptionLapses = nil
	tenantID := uuid.New()
	past := time.Now().UTC().Add(-60 * 24 * time.Hour)
	source := domain.SuspensionSourceBillingLapse
	row := &domain.TenantRealm{
		TenantID: tenantID, RealmName: "tenant-x", Strategy: domain.RealmStrategyDedicated,
		Status: domain.RealmStatusSuspended, DisabledAt: &past, SuspensionSource: &source,
	}
	// SQL: disabled_at < now() - 60d → this row qualifies.
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		assert.Equal(t, 60, retentionDays)
		return []*domain.TenantRealm{row}, nil
	}
	kc.ExportRealmFn = func(ctx context.Context, realmName string) ([]byte, error) { return []byte("x"), nil }
	jctx.ObjectStore = &mockObjectStore{PutRealmExportFn: func(ctx context.Context, key string, data []byte) error { return nil }}
	kc.DeleteRealmIfExistsFn = func(ctx context.Context, realmName string) error { return nil }

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res, "tenant disabled 60 days ago is past RETENTION_DAYS=60 and must be offboarded")
	assert.Equal(t, domain.RealmStatusDeleted, row.Status)
	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTenantOffboarded, pub.events[0].Type)
}

// ── CD-23: CANCELLED_GRACE_DAYS boundary tests (suspend phase) ──────────────
//
// O&M owns the 30-day cancelled grace period (§8.8) — RP defers entirely to
// O&M's I-16 list and never re-derives the threshold. These tests document the
// integration contract: O&M must not report a tenant before CANCELLED_GRACE_DAYS
// have elapsed, and RP must act only on what O&M reports.

// At cancelled_at + 29 days O&M's I-16 correctly returns an empty list.
// RP must not suspend anything.
func TestSubscriptionLapse_CancelledGrace_Day29_NotSuspended(t *testing.T) {
	jctx, kc, _, _, _, _ := newTestContext()
	// O&M: at day 29 the tenant is still within the 30-day cancelled grace.
	jctx.SubscriptionLapses = &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return nil, nil
		},
	}
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error {
		t.Fatal("DisableRealm must not be called when O&M reports no lapsed subscriptions")
		return nil
	}
	noOffboardCandidates(jctx)

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{}, res, "O&M returns nothing at day 29 — RP must not suspend")
}

// At cancelled_at + 30 days O&M's I-16 returns the tenant (past the 30-day
// grace). RP must disable the dedicated realm and emit TenantSuspended.
func TestSubscriptionLapse_CancelledGrace_Day30_Suspended(t *testing.T) {
	jctx, kc, realms, _, pub, _ := newTestContext()
	tenantID := uuid.New()
	cancelledAt := time.Now().UTC().Add(-30 * 24 * time.Hour)
	// O&M: at day 30 the tenant is past the cancelled grace period.
	jctx.SubscriptionLapses = &mockSubscriptionLapseReader{
		ListLapsedSubscriptionsFn: func(ctx context.Context) ([]port.LapsedSubscription, error) {
			return []port.LapsedSubscription{{
				TenantID:    tenantID,
				RealmID:     "kc-" + tenantID.String(),
				RealmType:   "dedicated",
				CancelledAt: cancelledAt,
			}}, nil
		},
	}
	realms.FindByTenantIDFn = func(ctx context.Context, tid uuid.UUID) (*domain.TenantRealm, error) {
		return newActiveDedicatedRealm(tenantID, "tenant-x"), nil
	}
	var disableRealmCalled bool
	kc.DisableRealmFn = func(ctx context.Context, realmName string) error { disableRealmCalled = true; return nil }
	noOffboardCandidates(jctx)

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res, "O&M reports tenant as lapsed at day 30 — RP must suspend")
	assert.True(t, disableRealmCalled, "realm must be disabled when O&M confirms the tenant is past cancelled grace")
	require.Len(t, pub.events, 1)
	payload := pub.events[0].Data.(domain.TenantSuspendedPayload)
	assert.Equal(t, domain.SuspensionSourceBillingLapse, payload.Source)
}

// ── CD-23: Operator-suspended tenants must never be auto-offboarded ──────────
//
// The SQL partial index on tenant_realms (WHERE suspension_source='billing_lapse')
// makes this a structural guarantee — an operator-suspended row never matches
// ListSuspendedPastRetention's predicate regardless of elapsed time.
// This test documents that structural guarantee by having the mock (which
// correctly simulates the SQL filter) return no candidates.
func TestSubscriptionLapse_OperatorSuspension_NeverAutoOffboarded(t *testing.T) {
	jctx, kc, _, _, _, _ := newTestContext()
	jctx.RetentionDays = 60
	jctx.SubscriptionLapses = nil
	// SQL partial index (suspension_source='billing_lapse') excludes the
	// operator-suspended tenant — ListSuspendedPastRetention returns empty.
	jctx.TenantRealms.(*mockTenantRealmRepository).ListSuspendedPastRetentionFn =
		func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
			return nil, nil
		}
	kc.ExportRealmFn = func(ctx context.Context, realmName string) ([]byte, error) {
		t.Fatal("ExportRealm must never be called for an operator-suspended tenant — excluded by the SQL index predicate (suspension_source='billing_lapse')")
		return nil, nil
	}

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{}, res, "operator-suspended tenants are excluded from the billing-lapse offboard sweep")
}
