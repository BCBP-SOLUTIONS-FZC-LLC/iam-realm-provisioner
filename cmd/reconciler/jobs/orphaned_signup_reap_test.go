package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The candidate selection, FOR UPDATE race re-check, Keycloak-failure defer
// and GDPR scrub need a real sysPool — they are covered against Postgres in
// test/postgres/reconciler_jobs_test.go (TestOrphanedSignupReap), the same
// split TrialHardDelete uses. These cover the guards that run before any
// database access.

func TestOrphanedSignupReap_WindowBelowFloor_RefusesBeforeTouchingAnything(t *testing.T) {
	for _, after := range []time.Duration{0, -time.Hour, 59 * time.Minute} {
		jctx, kc, _, _, _, _ := newTestContext()
		kc.HardDeleteUsersInRealmFn = func(context.Context, string, uuid.UUID) error {
			t.Fatal("a refused run must never reach Keycloak")
			return nil
		}
		jctx.OrphanSignupAfter = after

		res, err := OrphanedSignupReap(context.Background(), jctx)
		require.Error(t, err, after.String())
		assert.Contains(t, err.Error(), "floor")
		assert.Equal(t, Result{}, res)
	}
}

func TestOrphanedSignupReap_NoSysPool_Errors(t *testing.T) {
	jctx, _, _, _, _, _ := newTestContext()
	jctx.OrphanSignupAfter = DefaultOrphanSignupAfter

	res, err := OrphanedSignupReap(context.Background(), jctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SysPool")
	assert.Equal(t, Result{}, res)
}

func TestOrphanSignupAfterDefaults(t *testing.T) {
	assert.Equal(t, 48*time.Hour, DefaultOrphanSignupAfter)
	assert.GreaterOrEqual(t, DefaultOrphanSignupAfter, MinOrphanSignupAfter)
}
