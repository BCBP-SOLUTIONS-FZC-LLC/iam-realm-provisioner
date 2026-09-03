//go:build integration

package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/test/dbseed"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GaugeRepository backs the three §11.2 DB-state gauges, and its queries are
// the only place in the service that reads across tenants purely to count.
// They are exercised against the real schema because every subtle part of
// them is schema-coupled: the realm_strategy enum name in enum_range, the
// realm_status enum members in the status filter, and the soft-delete
// column. A unit test with a fake pool would pin none of it.

// seedRealm inserts one tenant_realms row through the superuser pool.
func seedRealm(t testing.TB, ctx context.Context, rawPool *dbseed.Pool,
	realmName, strategy, status string, templateVersion *string,
	deleted bool,
) {
	t.Helper()
	shard := "shard-a"
	var shardArg *string
	if strategy == "dedicated" {
		shardArg = &shard
	}
	_, err := rawPool.Exec(ctx, `
		INSERT INTO tenant_realms
			(tenant_id, realm_name, strategy, status, realm_template_version, keycloak_shard, deleted_at)
		VALUES ($1, $2, $3::realm_strategy, $4::realm_status, $5, $6,
		        CASE WHEN $7::boolean THEN now() ELSE NULL END)`,
		uuid.New(), realmName, strategy, status, templateVersion, shardArg, deleted)
	require.NoError(t, err)
}

func strptr(s string) *string { return &s }

// seedRLSViolation inserts one rls_violation_log row directly (bypassing the
// 1%-sampled log_rls_violation() function) at a caller-controlled
// occurred_at, so window-boundary behavior is deterministic rather than
// probabilistic.
func seedRLSViolation(t testing.TB, ctx context.Context, rawPool *dbseed.Pool,
	violationType string, occurredAt time.Time,
) {
	t.Helper()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO rls_violation_log (table_name, row_tenant_id, violation_type, occurred_at)
		VALUES ('tenant_realms', $1, $2, $3)`,
		uuid.New(), violationType, occurredAt)
	require.NoError(t, err)
}

func TestGaugeRepository_ActiveRealmsByStrategy(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	gauges := pgadapter.NewGaugeRepository(setupSysPool(t, rawPool))

	// Only the first three count: one shared + two dedicated actives.
	seedRealm(t, ctx, rawPool, "trial", "shared", "active", strptr("v1"), false)
	seedRealm(t, ctx, rawPool, "acme", "dedicated", "active", strptr("v1"), false)
	seedRealm(t, ctx, rawPool, "beta", "dedicated", "active", strptr("v1"), false)
	seedRealm(t, ctx, rawPool, "gamma", "dedicated", "provisioning", strptr("v1"), false)
	seedRealm(t, ctx, rawPool, "delta", "dedicated", "active", strptr("v1"), true)

	got, err := gauges.ActiveRealmsByStrategy(ctx)
	require.NoError(t, err)

	assert.Equal(t, map[string]int64{"shared": 1, "dedicated": 2}, got,
		"non-active and soft-deleted realms must be excluded")
}

// TestGaugeRepository_ActiveRealmsByStrategyReportsZeroNotAbsent is the
// reason the query LEFT JOINs enum_range instead of a plain GROUP BY: a
// strategy that drops to zero has to report 0, otherwise the gauge keeps
// exporting the last non-zero count for that label forever.
func TestGaugeRepository_ActiveRealmsByStrategyReportsZeroNotAbsent(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	gauges := pgadapter.NewGaugeRepository(setupSysPool(t, rawPool))

	seedRealm(t, ctx, rawPool, "acme", "dedicated", "active", strptr("v1"), false)

	got, err := gauges.ActiveRealmsByStrategy(ctx)
	require.NoError(t, err)

	assert.Equal(t, map[string]int64{"shared": 0, "dedicated": 1}, got,
		"every realm_strategy enum member must be present, including the empty one")
}

func TestGaugeRepository_ActiveRealmsByStrategyOnEmptyTable(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	gauges := pgadapter.NewGaugeRepository(setupSysPool(t, rawPool))

	got, err := gauges.ActiveRealmsByStrategy(context.Background())
	require.NoError(t, err)

	assert.Equal(t, map[string]int64{"shared": 0, "dedicated": 0}, got)
}

func TestGaugeRepository_PendingRealmConfigs(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	gauges := pgadapter.NewGaugeRepository(setupSysPool(t, rawPool))

	n, err := gauges.PendingRealmConfigs(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "an empty table must read 0, not error")

	for _, status := range []string{"pending", "pending", "applied", "failed"} {
		_, err := rawPool.Exec(ctx, `
			INSERT INTO realm_config_reconciliations
				(tenant_id, setting_key, desired_value, status)
			VALUES ($1, 'mfa_required', '"true"'::jsonb, $2::recon_status)`,
			uuid.New(), status)
		require.NoError(t, err)
	}

	n, err = gauges.PendingRealmConfigs(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n, "only status=pending rows count (§11.6 alert source)")
}

func TestGaugeRepository_TemplateDriftByVersion(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	gauges := pgadapter.NewGaugeRepository(setupSysPool(t, rawPool))

	seedRealm(t, ctx, rawPool, "current", "dedicated", "active", strptr("v2"), false)
	seedRealm(t, ctx, rawPool, "drift-a", "dedicated", "active", strptr("v1"), false)
	seedRealm(t, ctx, rawPool, "drift-b", "dedicated", "disabled", strptr("v1"), false)
	// A lapsed tenant on an old template is still drift to be fixed.
	seedRealm(t, ctx, rawPool, "drift-c", "dedicated", "suspended", strptr("v0"), false)
	// Never stamped with a version — must surface, not vanish via NULL.
	seedRealm(t, ctx, rawPool, "drift-d", "dedicated", "active", nil, false)
	// Excluded: mid-creation, terminal, soft-deleted, and shared-strategy.
	seedRealm(t, ctx, rawPool, "inflight", "dedicated", "provisioning", strptr("v1"), false)
	seedRealm(t, ctx, rawPool, "gone", "dedicated", "exported", strptr("v1"), false)
	seedRealm(t, ctx, rawPool, "deleted", "dedicated", "active", strptr("v1"), true)
	seedRealm(t, ctx, rawPool, "trial", "shared", "active", strptr("v1"), false)

	got, err := gauges.TemplateDriftByVersion(ctx, "v2")
	require.NoError(t, err)

	assert.Equal(t, map[string]int64{"v1": 2, "v0": 1, "unset": 1}, got)
}

// TestGaugeRepository_TemplateDriftByVersionEmptyWhenAligned pins the empty
// (not zero-valued) result that lets the exporter Reset the vec and drop
// stale version labels entirely.
func TestGaugeRepository_TemplateDriftByVersionEmptyWhenAligned(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	gauges := pgadapter.NewGaugeRepository(setupSysPool(t, rawPool))

	seedRealm(t, ctx, rawPool, "acme", "dedicated", "active", strptr("v2"), false)

	got, err := gauges.TemplateDriftByVersion(ctx, "v2")
	require.NoError(t, err)

	assert.Empty(t, got, "no drift must yield no label rows at all")
}

// TestGaugeRepository_ReadsAcrossTenants is the invariant that makes these
// gauges correct: the repository must be handed the BYPASSRLS sysPool, and
// it must see every tenant's rows without any app.tenant_id GUC bound.
func TestGaugeRepository_ReadsAcrossTenants(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	gauges := pgadapter.NewGaugeRepository(setupSysPool(t, rawPool))

	for _, name := range []string{"t1", "t2", "t3"} {
		seedRealm(t, ctx, rawPool, name, "dedicated", "active", strptr("v1"), false)
	}

	got, err := gauges.ActiveRealmsByStrategy(ctx)
	require.NoError(t, err)

	assert.EqualValues(t, 3, got["dedicated"],
		"three different tenants' realms must all be visible with no GUC set")
}

// TestGaugeRepository_RLSViolationCounts_GroupsByTypeWithinWindow pins both
// the grouping and the window boundary: a row exactly at the window edge is
// out (occurred_at >= cutoff, and the cutoff is now-window, so a row from
// slightly before that is excluded), and unrelated old rows never leak in.
// TestGaugeRepository_RLSViolationCounts_SeedsWatermarkAndExcludesPriorHistory
// pins the first-ever-run behavior: with no exporter_watermarks row yet,
// the mark is seeded at now-initialWindow, so a row older than that is
// excluded from this very first count (not scraped as "ancient history").
func TestGaugeRepository_RLSViolationCounts_SeedsWatermarkAndExcludesPriorHistory(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	gauges := pgadapter.NewGaugeRepository(setupSysPool(t, rawPool))

	now := time.Now().UTC()
	seedRLSViolation(t, ctx, rawPool, "cross_tenant_access", now.Add(-1*time.Minute))
	seedRLSViolation(t, ctx, rawPool, "cross_tenant_access", now.Add(-2*time.Minute))
	seedRLSViolation(t, ctx, rawPool, "missing_or_invalid_guc", now.Add(-3*time.Minute))
	seedRLSViolation(t, ctx, rawPool, "missing_or_invalid_guc", now.Add(-10*time.Minute)) // older than the seeded mark

	got, err := gauges.RLSViolationCounts(ctx, 5*time.Minute)
	require.NoError(t, err)

	assert.Equal(t, map[string]int64{
		"cross_tenant_access":    2,
		"missing_or_invalid_guc": 1,
	}, got, "the 10-minute-old row predates the freshly-seeded now-5m watermark and must be excluded")
}

// TestGaugeRepository_RLSViolationCounts_SecondTickOnlyCountsNewRows is the
// direct proof for the bug this watermark design fixes: a pod restart (or
// another concurrent server replica) re-invoking RLSViolationCounts must
// never re-count a row an earlier call already tallied. Simulates two
// exporter ticks by calling the method twice, seeding new violations only
// in between — a fixed-trailing-window implementation would double-count
// the first batch on the second call; this one must not.
func TestGaugeRepository_RLSViolationCounts_SecondTickOnlyCountsNewRows(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	gauges := pgadapter.NewGaugeRepository(setupSysPool(t, rawPool))

	seedRLSViolation(t, ctx, rawPool, "cross_tenant_access", time.Now().UTC())
	first, err := gauges.RLSViolationCounts(ctx, 5*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"cross_tenant_access": 1}, first)

	// A second tick with nothing new must report nothing — not the first
	// row again, which is exactly what a trailing-window re-query would do.
	second, err := gauges.RLSViolationCounts(ctx, 5*time.Minute)
	require.NoError(t, err)
	assert.Empty(t, second, "the row counted by the first tick must never be counted again")

	// A new violation arriving after the second tick's snapshot is picked
	// up cleanly by a third tick, proving the mark genuinely advanced
	// rather than getting stuck.
	seedRLSViolation(t, ctx, rawPool, "missing_or_invalid_guc", time.Now().UTC())
	third, err := gauges.RLSViolationCounts(ctx, 5*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"missing_or_invalid_guc": 1}, third)
}

// TestGaugeRepository_RLSViolationCounts_ConcurrentRepositoriesNeverDoubleCount
// proves the other half of the watermark design's purpose: this service's
// HPA runs multiple server replicas, and every replica runs the same
// exporter against the same table. Two independent GaugeRepository
// instances (standing in for two pods, each with its own connection pool)
// racing RLSViolationCounts against the same watermark row must serialize
// via the row's SELECT ... FOR UPDATE lock — one sees the real rows, the
// other (blocked until the first commits) sees an already-drained window —
// so the sum across both calls equals the true count exactly once, never
// double.
func TestGaugeRepository_RLSViolationCounts_ConcurrentRepositoriesNeverDoubleCount(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	// Two separate sysPool connections, mirroring two separate server pods.
	gaugesA := pgadapter.NewGaugeRepository(setupSysPool(t, rawPool))
	gaugesB := pgadapter.NewGaugeRepository(setupSysPool(t, rawPool))

	const violationCount = 10
	for i := 0; i < violationCount; i++ {
		seedRLSViolation(t, ctx, rawPool, "cross_tenant_access", time.Now().UTC())
	}

	var wg sync.WaitGroup
	countsA := make(map[string]int64)
	countsB := make(map[string]int64)
	wg.Add(2)
	go func() {
		defer wg.Done()
		c, err := gaugesA.RLSViolationCounts(ctx, 5*time.Minute)
		require.NoError(t, err)
		countsA = c
	}()
	go func() {
		defer wg.Done()
		c, err := gaugesB.RLSViolationCounts(ctx, 5*time.Minute)
		require.NoError(t, err)
		countsB = c
	}()
	wg.Wait()

	total := countsA["cross_tenant_access"] + countsB["cross_tenant_access"]
	assert.Equal(t, int64(violationCount), total,
		"the two concurrent callers must together count every violation exactly once — never both counting the same rows, never dropping any")
}

// TestGaugeRepository_RLSViolationCounts_EmptyWhenNoViolations pins the
// absent-not-zero result: the exporter's Add-delta loop must not synthesize
// a 0 for either violation_type when the table has nothing to report.
func TestGaugeRepository_RLSViolationCounts_EmptyWhenNoViolations(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	gauges := pgadapter.NewGaugeRepository(setupSysPool(t, rawPool))

	got, err := gauges.RLSViolationCounts(ctx, 5*time.Minute)
	require.NoError(t, err)

	assert.Empty(t, got)
}
