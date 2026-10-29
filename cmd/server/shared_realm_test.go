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

// scriptedEnsurer fails its first `failures` calls, then succeeds.
type scriptedEnsurer struct {
	calls    int32
	failures int32
	realm    atomic.Value
}

func (e *scriptedEnsurer) EnsureSharedRealm(_ context.Context, realm string) error {
	e.realm.Store(realm)
	if atomic.AddInt32(&e.calls, 1) <= e.failures {
		return errors.New("keycloak down")
	}
	return nil
}

// A Keycloak outage at boot is retried until the realm is ensured, then
// the loop stops.
func TestEnsureSharedRealm_RetriesUntilSuccessThenStops(t *testing.T) {
	kc := &scriptedEnsurer{failures: 2}
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
	kc := &scriptedEnsurer{failures: 1 << 30}
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
	kc := &scriptedEnsurer{}
	startSharedRealmEnsure(context.Background(), kc, "trial", &fakeLogger{})
	require.Eventually(t, func() bool { return atomic.LoadInt32(&kc.calls) == 1 }, time.Second, time.Millisecond)
}
