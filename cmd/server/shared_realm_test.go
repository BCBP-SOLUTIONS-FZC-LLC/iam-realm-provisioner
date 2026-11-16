package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedCalls is the shared counter logic for realm-ensurer test fakes.
// Embed it in a type that implements the specific ensurer interface.
type scriptedCalls struct {
	calls    int32
	failures int32
	realm    atomic.Value
}

func (e *scriptedCalls) attempt(realm string) error {
	e.realm.Store(realm)
	if atomic.AddInt32(&e.calls, 1) <= e.failures {
		return errors.New("keycloak down")
	}
	return nil
}

// scriptedEnsurer implements sharedRealmEnsurer, failing its first `failures`
// calls then succeeding.
type scriptedEnsurer struct{ scriptedCalls }

func (e *scriptedEnsurer) EnsureSharedRealm(_ context.Context, realm string) error {
	return e.attempt(realm)
}

// A Keycloak outage at boot is retried until the realm is ensured, then
// the loop stops.
func TestEnsureSharedRealm_RetriesUntilSuccessThenStops(t *testing.T) {
	kc := &scriptedEnsurer{scriptedCalls: scriptedCalls{failures: 2}}
	done := make(chan struct{})
	go func() {
		ensureSharedRealm(context.Background(), kc, "trial", time.Millisecond, &fakeLogger{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ensureSharedRealm did not return after success")
	}
	assert.Equal(t, int32(3), atomic.LoadInt32(&kc.calls))
	assert.Equal(t, "trial", kc.realm.Load())
}

// Shutdown stops a loop that is still waiting to retry.
func TestEnsureSharedRealm_StopsOnCancel(t *testing.T) {
	kc := &scriptedEnsurer{scriptedCalls: scriptedCalls{failures: 1 << 30}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ensureSharedRealm(ctx, kc, "trial", time.Hour, &fakeLogger{}); close(done) }()

	require.Eventually(t, func() bool { return atomic.LoadInt32(&kc.calls) == 1 }, time.Second, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ensureSharedRealm ignored cancellation")
	}
}

func TestStartSharedRealmEnsure_RunsInBackground(t *testing.T) {
	// metrics.SharedRealmReady must be registered before the goroutine reads it.
	ensureMetricsRegistered(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	kc := &scriptedEnsurer{}
	startSharedRealmEnsure(ctx, kc, "trial", &fakeLogger{})
	require.Eventually(t, func() bool { return atomic.LoadInt32(&kc.calls) == 1 }, time.Second, time.Millisecond)
}
