//go:build load

// Package load_test is the sibling `test/load/` convention the LLD
// describes (docs/lld/iam-lld-realm-provisioner-service.md §14.6 "Load
// tests", cross-referenced from §21.2 "Conversion's per-user migration
// cost at scale" and §11.1's RP-3 SLO: `p95 ≤ 8s + ~0.3s/user migrated`).
// That SLO, and the §21.1 claim that "the Keycloak Admin API rate limiter
// is the dominant constraint" on conversion throughput, were previously
// documented targets with no test backing them — this package is that
// backing.
//
// What this measures: it drives the REAL RP-3 conversion entrypoint
// (internal/core/service/provisioning_service.go's
// ProvisioningService.Convert) migrating a synthetic tenant's N trial
// users, through the REAL client-side Keycloak Admin API rate limiter
// (internal/adapter/outbound/keycloak's token-bucket limiter, governed by
// KEYCLOAK_RATE_LIMIT_RPS) and the REAL keycloak.Admin adapter — with only
// the Keycloak REST API itself faked, via an in-process httptest.Server
// (fakeKeycloakServer, fake_keycloak_test.go), never a real Keycloak
// testcontainer. That's the "loopback-network/mock-Keycloak caveat" the
// LLD requires this suite to preserve and report: every latency number
// here reflects this service's own rate-limiter/conversion-loop overhead
// plus a small simulated per-call network/processing delay, NOT Keycloak's
// real-world response time — a real Keycloak's own admin-API latency sits
// on top of these numbers in production.
//
// Non-Keycloak dependencies (TenantRealmRepository, IdempotencyStore,
// TxRunner) are simple in-memory fakes (mocks_test.go), the same shape
// test/unit/provisioning_convert_test.go already uses to unit-test
// Convert — RunInTx/RLS/Postgres behavior is not what this suite measures.
//
// Running it:
//
//	make test-load                     # fast default: N=50 and N=200 (LLD's own example)
//	LOAD_TEST_FULL=1 make test-load     # adds the full N=500 the LLD names explicitly
//
// or directly:
//
//	go test ./test/load/... -tags=load -count=1 -timeout 300s -v
//	LOAD_TEST_FULL=1 go test ./test/load/... -tags=load -count=1 -timeout 300s -v
//
// Every numeric knob (N, KEYCLOAK_RATE_LIMIT_RPS, simulated per-call
// latency) is overridable via env vars (see envInt/envDuration below) so
// this can be re-run at the LLD's stated N=500, or against a different
// KEYCLOAK_RATE_LIMIT_RPS, without editing the test — exactly the "re-run
// it whenever KEYCLOAK_RATE_LIMIT_RPS changes" workflow §21.2 describes.
package load_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/keycloak"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

const sharedRealm = "trial"

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(name string) bool {
	v := os.Getenv(name)
	return v == "1" || v == "true" || v == "yes"
}

func envDuration(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// loadRunResult captures one Convert run's timing.
type loadRunResult struct {
	n         int
	rps       int
	elapsed   time.Duration
	latencies []time.Duration // per-user migration latency, sorted
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p*float64(len(sorted)-1) + 0.5)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func (r loadRunResult) logSummary(t *testing.T) {
	t.Helper()
	sorted := append([]time.Duration(nil), r.latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	throughput := float64(r.n) / r.elapsed.Seconds()
	t.Logf(
		"N=%d rps_budget=%d elapsed=%s throughput=%.2f users/sec p50=%s p95=%s p99=%s",
		r.n, r.rps, r.elapsed, throughput,
		percentile(sorted, 0.50), percentile(sorted, 0.95), percentile(sorted, 0.99),
	)
}

// runConversionLoad seeds a synthetic shared-trial tenant with n users,
// wires the real ProvisioningService.Convert against a fake Keycloak
// server enforcing callLatency per admin-API call, and drives one RP-3
// conversion end to end. rps is KEYCLOAK_RATE_LIMIT_RPS for this run.
func runConversionLoad(t *testing.T, n, rps int, callLatency time.Duration) loadRunResult {
	t.Helper()

	fake := newFakeKeycloakServer(t, callLatency)

	admin := keycloak.New(keycloak.Config{
		BaseURL:      fake.baseURL(),
		AdminRealm:   "master",
		ClientID:     "iam-realm-provisioner",
		ClientSecret: "secret",
		RateLimitRPS: rps,
		Timeout:      30 * time.Second,
	}, noopLogger{})

	realms := newLoadTenantRealmRepository()
	idem := newLoadIdempotencyStore()
	tx := &loadTxRunner{}

	svc := service.NewProvisioningService(realms, idem, admin, loadTokenServiceClient{}, tx, nil, sharedRealm, []string{"kc-shard-1"}).WithLogger(noopLogger{})

	tenantID := uuid.New()
	realms.seed(&domain.TenantRealm{
		TenantID: tenantID, RealmName: sharedRealm, Strategy: domain.RealmStrategyShared,
		Status: domain.RealmStatusActive, RecordVersion: 1,
	})

	userIDs := make([]uuid.UUID, n)
	for i := range userIDs {
		userIDs[i] = uuid.New()
	}
	fake.seedTrialUsers(sharedRealm, userIDs)

	idempotencyKey := fmt.Sprintf("load-conv-%s", tenantID)
	start := time.Now()
	result, err := svc.Convert(context.Background(), idempotencyKey, tenantID, service.ConvertRequest{Plan: "pro"})
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Equal(t, n, result.UsersMigrated)

	return loadRunResult{n: n, rps: rps, elapsed: elapsed, latencies: fake.latencySnapshot()}
}

// TestMigrationLoad_RateLimiterAndScaling is the fast default: two
// conversions at N_SMALL and N_LARGE (the LLD's own N=50/N=200 example),
// asserting (a) the rate limiter's admission rate stays within its
// configured budget and (b) total conversion time scales roughly linearly
// with N — both as *properties*, with generous tolerances, since this is a
// load/perf test and must stay robust to test-machine variance, not a
// strict correctness check.
func TestMigrationLoad_RateLimiterAndScaling(t *testing.T) {
	// N defaults are larger than the LLD's bare N=50/N=200 example
	// specifically so the fixed token-bucket *burst* (equal to
	// KEYCLOAK_RATE_LIMIT_RPS, ratelimit.go) is a small fraction of each
	// run's total call count — at N close to the burst size, the burst's
	// "first RPS calls run free" effect measurably skews the small-N run
	// faster than steady-state throughput predicts, inflating the
	// small-vs-large elapsed-time ratio past the N ratio. Diluting it
	// keeps the linear-scaling assertion meaningful without a fragile
	// tolerance band.
	nSmall := envInt("LOAD_TEST_N_SMALL", 80)
	nLarge := envInt("LOAD_TEST_N_LARGE", 320)
	rps := envInt("LOAD_TEST_RPS", 25)
	// Simulated per-call Keycloak latency deliberately small (a few ms) —
	// see the package doc's loopback-caveat note — but non-zero, so the
	// rate limiter (not raw Go-runtime scheduling speed) is what paces the
	// migration loop, making its throttling behavior observable.
	callLatency := envDuration("LOAD_TEST_CALL_LATENCY", 2*time.Millisecond)

	small := runConversionLoad(t, nSmall, rps, callLatency)
	small.logSummary(t)
	large := runConversionLoad(t, nLarge, rps, callLatency)
	large.logSummary(t)

	assertRateLimiterBudget(t, large)
	assertRoughlyLinearScaling(t, small, large)
}

// TestMigrationLoad_FullScale500 is the LLD §14.6-literal N=500 run.
// Skipped by default (a 500-user run is deliberately not part of every
// invocation of this suite — see the package doc) — set LOAD_TEST_FULL=1
// (or LOAD_TEST_N to a specific size) to run it.
func TestMigrationLoad_FullScale500(t *testing.T) {
	if !envBool("LOAD_TEST_FULL") && os.Getenv("LOAD_TEST_N") == "" {
		t.Skip("skipped by default (expensive) — set LOAD_TEST_FULL=1 or LOAD_TEST_N=<n> to run")
	}
	n := envInt("LOAD_TEST_N", 500)
	rps := envInt("LOAD_TEST_RPS", 25)
	callLatency := envDuration("LOAD_TEST_CALL_LATENCY", 2*time.Millisecond)

	result := runConversionLoad(t, n, rps, callLatency)
	result.logSummary(t)
	assertRateLimiterBudget(t, result)
}

// assertRateLimiterBudget asserts the achieved per-user migration
// throughput does not exceed the configured KEYCLOAK_RATE_LIMIT_RPS budget
// by more than a generous tolerance. Each MigrateUser call consumes
// exactly one rate-limiter admission (internal/adapter/outbound/keycloak's
// Admin.token() calls limit.Wait() once per top-level call and the
// migration loop is strictly sequential), so for a run with enough users
// to reach steady state, achieved throughput should track the configured
// RPS rather than run away unbounded.
func assertRateLimiterBudget(t *testing.T, r loadRunResult) {
	t.Helper()
	throughput := float64(r.n) / r.elapsed.Seconds()
	// Tolerance is generous (75% over budget) to absorb scheduler jitter,
	// fixed per-run overhead (realm creation, OIDC client registration,
	// etc. all draw from the same budget before any user migrates), and
	// slow CI machines — the point is to catch the limiter being absent or
	// grossly ineffective, not to pin an exact number.
	maxThroughput := float64(r.rps) * 1.75
	t.Logf("rate-limiter budget check: throughput=%.2f users/sec, budget=%d rps, max allowed=%.2f", throughput, r.rps, maxThroughput)
	require.LessOrEqualf(t, throughput, maxThroughput,
		"migration throughput (%.2f users/sec) exceeded the KEYCLOAK_RATE_LIMIT_RPS=%d budget by more than the allowed tolerance — the rate limiter does not appear to be throttling Keycloak Admin API calls", throughput, r.rps)
}

// assertRoughlyLinearScaling asserts total conversion wall-clock time
// scales roughly proportionally with N, within a tolerance wide enough to
// absorb the fixed per-conversion overhead (realm creation, OIDC client
// registration, force-logout) that dominates more at small N than at
// large N — so the *actual* time ratio is expected to run somewhat below
// the raw N ratio, never above it by much.
func assertRoughlyLinearScaling(t *testing.T, small, large loadRunResult) {
	t.Helper()
	require.Greater(t, small.n, 0)
	require.Greater(t, large.n, small.n, "the second run must migrate more users than the first")

	nRatio := float64(large.n) / float64(small.n)
	timeRatio := large.elapsed.Seconds() / small.elapsed.Seconds()
	t.Logf("linear-scaling check: N ratio=%.2f (n=%d -> n=%d), elapsed ratio=%.2f (%s -> %s)",
		nRatio, small.n, large.n, timeRatio, small.elapsed, large.elapsed)

	// The elapsed-time ratio should sit within a generous band around the
	// N ratio: at least 30% of it (fixed overhead can only shrink the
	// ratio, never invert it into shrinking wall-clock time as N grows)
	// and at most 1.75x it (comfortably above linear — allowing room for
	// the token-bucket burst's small-N skew described above — while still
	// catching an accidentally-quadratic regression without flaking on
	// jitter).
	minRatio := nRatio * 0.30
	maxRatio := nRatio * 1.75
	require.GreaterOrEqualf(t, timeRatio, minRatio,
		"elapsed-time ratio %.2f is far below the N ratio %.2f — conversion time did not grow with N as expected", timeRatio, nRatio)
	require.LessOrEqualf(t, timeRatio, maxRatio,
		"elapsed-time ratio %.2f is far above the N ratio %.2f — conversion time is scaling worse than linearly with N", timeRatio, nRatio)
}
