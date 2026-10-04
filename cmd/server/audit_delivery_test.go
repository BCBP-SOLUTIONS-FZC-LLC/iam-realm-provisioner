package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedDrainer returns results[i] on its i-th call, then 0 forever.
type scriptedDrainer struct {
	mu      sync.Mutex
	calls   int32
	results []int
	errs    []error
}

func (d *scriptedDrainer) DrainOnce(context.Context, int) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := int(atomic.AddInt32(&d.calls, 1)) - 1
	var err error
	if i < len(d.errs) {
		err = d.errs[i]
	}
	if i < len(d.results) {
		return d.results[i], err
	}
	return 0, err
}

// A full batch is drained again immediately, without waiting for a tick.
func TestRunAuditDelivery_FullBatchDrainsAgainImmediately(t *testing.T) {
	d := &scriptedDrainer{results: []int{5, 5, 2}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runAuditDelivery(ctx, d, time.Hour, 5, &fakeLogger{}); close(done) }()

	require.Eventually(t, func() bool { return atomic.LoadInt32(&d.calls) == 3 }, time.Second, 5*time.Millisecond)
	cancel()
	<-done
	assert.Equal(t, int32(3), atomic.LoadInt32(&d.calls), "a short batch must stop the burst until the next tick")
}

func TestRunAuditDelivery_DrainErrorLoggedAndRetriedNextTick(t *testing.T) {
	d := &scriptedDrainer{errs: []error{errors.New("db down")}}
	log := &fakeLogger{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runAuditDelivery(ctx, d, 10*time.Millisecond, 5, log)

	require.Eventually(t, func() bool { return atomic.LoadInt32(&d.calls) >= 2 }, time.Second, 5*time.Millisecond)
	assert.GreaterOrEqual(t, atomic.LoadInt32(&log.warnCalls), int32(1))
}

func TestStartAuditDelivery_UnsetBaseURL_WarnsAndDoesNotStart(t *testing.T) {
	t.Setenv("AUDIT_LOG_BASE_URL", "")
	log := &fakeLogger{}
	startAuditDelivery(context.Background(), nil, log)
	assert.Equal(t, int32(1), atomic.LoadInt32(&log.warnCalls))
}

// With a base URL set the drainer is wired and started; a canceled
// context stops it before its first drain touches the (nil) pool.
func TestStartAuditDelivery_BaseURLSet_StartsDrainer(t *testing.T) {
	t.Setenv("AUDIT_LOG_BASE_URL", "http://audit-log.invalid")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	log := &fakeLogger{}
	startAuditDelivery(ctx, nil, log)
	assert.Equal(t, int32(0), atomic.LoadInt32(&log.warnCalls), "a configured URL is not the disabled warning")
}
