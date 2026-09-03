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

// ── SubscriptionLapses == nil: suspend phase skipped, offboard phase still
// runs (this is the behavior change from the RP-C3 rewrite). ──────────────

func TestSubscriptionLapse_NilReader_SkipsSuspendPhase_OffboardStillRuns(t *testing.T) {
	jctx, _, realms, _, _, logger := newTestContext()
	jctx.SubscriptionLapses = nil
	var offboardCalled bool
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		offboardCalled = true
		return nil, nil
	}

	res, err := SubscriptionLapse(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{}, res)
	assert.True(t, offboardCalled, "the offboard phase must run even when SubscriptionLapses is nil")
	require.NotEmpty(t, logger.warnMsgs)
	assert.Contains(t, logger.warnMsgs[0], "no SubscriptionLapseReader wired")
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
	jctx.SubscriptionLapses = nil
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
	jctx.SubscriptionLapses = nil
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
	jctx.SubscriptionLapses = nil // suspend phase not under test here
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

func TestSubscriptionLapse_Offboard_ExportFails(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	jctx.SubscriptionLapses = nil
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
	jctx.SubscriptionLapses = nil
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
	jctx.SubscriptionLapses = nil
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

func TestSubscriptionLapse_Offboard_IdempotentReplay(t *testing.T) {
	jctx, kc, realms, idem, pub, _ := newTestContext()
	jctx.SubscriptionLapses = nil
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
	jctx.SubscriptionLapses = nil
	realms.ListSuspendedPastRetentionFn = func(ctx context.Context, retentionDays, limit int) ([]*domain.TenantRealm, error) {
		return nil, errors.New("db down")
	}

	_, err := SubscriptionLapse(context.Background(), jctx)
	require.Error(t, err)
}
