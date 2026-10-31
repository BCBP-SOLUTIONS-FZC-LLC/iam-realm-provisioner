// The service-layer suite lives in test/unit as black-box tests against the
// exported API (see CONTRIBUTING "Test layout"). This file holds the few
// tests that cannot: they assert on unexported fields, call unexported
// methods, or exercise unexported helpers, so they must stay in package
// service. Keep it that way — a test that only needs the exported surface
// belongs in test/unit with the rest.
package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// countsRepo is a minimal port.TenantRealmRepository: the embedded interface
// is nil, so any method other than CountsByShard panics if called — which is
// the point, since selectShard must read nothing else.
type countsRepo struct {
	counts map[string]int
	err    error
}

func (r countsRepo) CountsByShard(context.Context) (map[string]int, error) {
	return r.counts, r.err
}

// warnCountingLogger is a local stand-in for the shared mock in test/unit;
// the only white-box logging assertion here needs nothing but a warn tally.
type warnCountingLogger struct {
	warnCalls int
}

var _ port.Logger = (*warnCountingLogger)(nil)

func (l *warnCountingLogger) Debug(string, map[string]any) {}
func (l *warnCountingLogger) Info(string, map[string]any)  {}
func (l *warnCountingLogger) Warn(string, map[string]any)  { l.warnCalls++ }
func (l *warnCountingLogger) Error(string, map[string]any) {}

// ── Constructor defaults (unexported fields) ──────────────────────────────

// Both constructors only assign, so nil collaborators are safe here and keep
// the assertion focused on the defaulting itself.

func TestNewLifecycleService_Defaults(t *testing.T) {
	svc := NewLifecycleService(nil, nil, nil, nil, "")
	assert.Equal(t, "trial", svc.sharedRealm)
	assert.Equal(t, 7, svc.revertDays)
}

func TestNewProvisioningService_Defaults(t *testing.T) {
	svc := NewProvisioningService(nil, nil, nil, nil, nil, nil, "", nil)
	assert.Equal(t, "trial", svc.sharedRealm)
	assert.Equal(t, []string{"kc-shard-1"}, svc.shardIDs)
}

// ── selectShard (unexported method) ───────────────────────────────────────

// Least-loaded wins; equal counts break lexicographically, reading counts
// from the local mirror only.
func TestSelectShard_LeastLoadedWithLexicographicTieBreak(t *testing.T) {
	svc := NewProvisioningService(
		nil, nil, nil, nil, nil, nil, "trial",
		[]string{"kc-shard-2", "kc-shard-1", "kc-shard-3"},
	).WithShardLoads(countsRepo{counts: map[string]int{"kc-shard-1": 2, "kc-shard-2": 1, "kc-shard-3": 1}})

	// Least-loaded ties between shard-2 and shard-3; the tie-break picks 2.
	got, err := svc.selectShard(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "kc-shard-2", got)
}

// With no load recorded anywhere every shard ties at zero, so the
// lexicographic tie-break alone decides — independent of shardIDs order.
func TestSelectShard_NoLoadYet_TieBreaksLexicographically(t *testing.T) {
	svc := NewProvisioningService(
		nil, nil, nil, nil, nil, nil, "trial",
		[]string{"kc-shard-9", "kc-shard-1"},
	).WithShardLoads(countsRepo{counts: map[string]int{}})

	got, err := svc.selectShard(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "kc-shard-1", got)
}

func TestSelectShard_CountsByShardFailure_Propagates(t *testing.T) {
	svc := NewProvisioningService(
		nil, nil, nil, nil, nil, nil, "trial", nil,
	).WithShardLoads(countsRepo{err: errors.New("db down")})

	_, err := svc.selectShard(context.Background())
	require.Error(t, err)
}

// Placement never falls back to the tenant realm repository: on the app pool
// it sees only the request tenant's own row, so every realm would land on
// the first shard and each hidden row would log an RLS violation.
func TestSelectShard_NoShardLoads_Fails(t *testing.T) {
	svc := NewProvisioningService(nil, nil, nil, nil, nil, nil, "trial", nil)

	_, err := svc.selectShard(context.Background())
	require.ErrorContains(t, err, "shard loads not configured")
}

// ── marshalIdempotencyResult (unexported helper) ──────────────────────────

// marshalIdempotencyResult's own marshal failure is logged, not propagated
// — the caller still gets whatever (possibly empty) bytes result.
func TestMarshalIdempotencyResult_MarshalFailure_LoggedNotPropagated(t *testing.T) {
	logger := &warnCountingLogger{}
	sl := port.NewSlogStyleLogger(logger)
	body := marshalIdempotencyResult(context.Background(), sl, "test_op", make(chan int))
	assert.Empty(t, body)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}
