package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedPlatformEnsurer implements platformRealmEnsurer using the shared
// scriptedCalls counter (defined in shared_realm_test.go, same package).
type scriptedPlatformEnsurer struct{ scriptedCalls }

func (e *scriptedPlatformEnsurer) EnsurePlatformRealm(_ context.Context, realm string) error {
	return e.attempt(realm)
}

// A Keycloak outage at boot is retried until the realm is ensured.
func TestEnsurePlatformRealm_RetriesUntilSuccessThenStops(t *testing.T) {
	kc := &scriptedPlatformEnsurer{scriptedCalls: scriptedCalls{failures: 2}}
	done := make(chan struct{})
	go func() {
		ensurePlatformRealm(context.Background(), kc, "platform", time.Millisecond, &fakeLogger{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ensurePlatformRealm did not return after success")
	}
	assert.Equal(t, int32(3), atomic.LoadInt32(&kc.calls))
	assert.Equal(t, "platform", kc.realm.Load())
}

// Shutdown stops a loop that is still waiting to retry.
func TestEnsurePlatformRealm_StopsOnCancel(t *testing.T) {
	kc := &scriptedPlatformEnsurer{scriptedCalls: scriptedCalls{failures: 1 << 30}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ensurePlatformRealm(ctx, kc, "platform", time.Hour, &fakeLogger{}); close(done) }()

	require.Eventually(t, func() bool { return atomic.LoadInt32(&kc.calls) == 1 }, time.Second, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ensurePlatformRealm ignored cancellation")
	}
}

func TestStartPlatformRealmEnsure_RunsInBackground(t *testing.T) {
	// metrics.PlatformRealmReady must be registered before the goroutine reads it.
	ensureMetricsRegistered(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	kc := &scriptedPlatformEnsurer{}
	startPlatformRealmEnsure(ctx, kc, "platform", &fakeLogger{})
	require.Eventually(t, func() bool { return atomic.LoadInt32(&kc.calls) == 1 }, time.Second, time.Millisecond)
}
