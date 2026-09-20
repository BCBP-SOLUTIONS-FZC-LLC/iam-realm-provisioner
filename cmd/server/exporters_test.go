package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLogger is a minimal port.Logger test double recording Warn calls —
// tick's only observable side effect on emit failure. mu guards lastWarn
// since tick's goroutine writes it concurrently with the test goroutine's
// read after require.Eventually observes warnCalls advance.
type fakeLogger struct {
	warnCalls int32
	mu        sync.Mutex
	lastWarn  map[string]any
}

func (f *fakeLogger) Debug(string, map[string]any) {}
func (f *fakeLogger) Info(string, map[string]any)  {}
func (f *fakeLogger) Warn(msg string, fields map[string]any) {
	f.mu.Lock()
	f.lastWarn = fields
	f.mu.Unlock()
	atomic.AddInt32(&f.warnCalls, 1)
}
func (f *fakeLogger) Error(string, map[string]any) {}
func (f *fakeLogger) getLastWarn() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastWarn
}

func gaugeVecValue(t *testing.T, vec *prometheus.GaugeVec, labelValues ...string) (float64, bool) {
	t.Helper()
	g, err := vec.GetMetricWithLabelValues(labelValues...)
	require.NoError(t, err)
	var m dto.Metric
	require.NoError(t, g.Write(&m))
	return m.GetGauge().GetValue(), true
}

// TestTick_EmitsOnceBeforeFirstTickerFire pins the "populate before the
// first Prometheus scrape" contract the doc comment on runBusinessExporters
// promises — before these exporters existed, the gauges were registered but
// permanently empty because nothing ever populated them ahead of a scrape.
func TestTick_EmitsOnceBeforeFirstTickerFire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int32
	done := make(chan struct{})
	go func() {
		tick(ctx, "test_gauge", &fakeLogger{}, func() error {
			atomic.AddInt32(&calls, 1)
			return nil
		})
		close(done)
	}()

	require.Eventually(t, func() bool { return atomic.LoadInt32(&calls) >= 1 }, time.Second, time.Millisecond,
		"emit must run once immediately, without waiting for the first ticker interval")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tick did not return after ctx was canceled")
	}
}

// TestTick_ReturnsPromptlyOnContextCancellation pins that tick doesn't block
// past ctx cancellation waiting on the multi-minute exporterInterval ticker.
func TestTick_ReturnsPromptlyOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tick(ctx, "test_gauge", &fakeLogger{}, func() error { return nil })
		close(done)
	}()

	// Let the initial synchronous emit complete, then cancel — tick must
	// return promptly rather than sitting in the select waiting on
	// exporterInterval (5 minutes) to next fire.
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tick blocked well past context cancellation — it must return on <-ctx.Done(), not wait for the next ticker fire")
	}
}

// TestTick_QueryFailureLogsWarnAndKeepsRunning pins the "a stale gauge beats
// a crashed pod" contract: an emit error must be logged at WARN, identify
// which gauge failed, and must not stop the goroutine or propagate a panic.
func TestTick_QueryFailureLogsWarnAndKeepsRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &fakeLogger{}
	done := make(chan struct{})

	go func() {
		tick(ctx, "test_gauge", log, func() error { return errors.New("boom") })
		close(done)
	}()

	require.Eventually(t, func() bool { return atomic.LoadInt32(&log.warnCalls) >= 1 }, time.Second, time.Millisecond)
	lastWarn := log.getLastWarn()
	assert.Equal(t, "test_gauge", lastWarn["gauge"])
	assert.Equal(t, "boom", lastWarn["error"])

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tick did not return after ctx was canceled, even after an emit failure")
	}
}

// TestTick_ReFiresOnTicker pins the periodic re-fire itself, distinct from
// the "populate before the first scrape" initial emit above — shrinks
// exporterInterval for this one case so the ticker fires in well under a
// second instead of the real 5 minutes.
func TestTick_ReFiresOnTicker(t *testing.T) {
	old := exporterInterval
	exporterInterval = 5 * time.Millisecond
	t.Cleanup(func() { exporterInterval = old })

	ctx, cancel := context.WithCancel(context.Background())
	var calls int32
	done := make(chan struct{})
	go func() {
		tick(ctx, "test_gauge", &fakeLogger{}, func() error {
			atomic.AddInt32(&calls, 1)
			return nil
		})
		close(done)
	}()

	require.Eventually(t, func() bool { return atomic.LoadInt32(&calls) >= 3 }, time.Second, time.Millisecond,
		"the ticker must re-fire emit repeatedly, not just the initial pre-scrape call")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tick did not return after ctx was canceled")
	}
}

// TestSetGaugeVec_NoReset_RetainsLabelsAbsentFromLatestCounts pins
// realm_active_gauge's own contract: a plain re-poll must never drop a
// label the query didn't return this time, because the exporter relies on
// the query itself (a LEFT JOIN over the full enum) to report explicit
// zeros — Reset here would defeat that and race the Set calls, transiently
// zeroing every label on every tick.
func TestSetGaugeVec_NoReset_RetainsLabelsAbsentFromLatestCounts(t *testing.T) {
	vec := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "test_no_reset_gauge"}, []string{"label"})

	setGaugeVec(vec, map[string]int64{"shared": 3, "dedicated": 5}, false)
	v, _ := gaugeVecValue(t, vec, "shared")
	assert.Equal(t, float64(3), v)

	// A later tick's result doesn't mention "shared" at all (as if its own
	// query had a transient hiccup collecting that one row) — the previous
	// value must survive untouched, not silently reset to 0/disappear.
	setGaugeVec(vec, map[string]int64{"dedicated": 7}, false)
	v, _ = gaugeVecValue(t, vec, "shared")
	assert.Equal(t, float64(3), v, "a label absent from this tick's counts must retain its prior value when reset=false")
	v, _ = gaugeVecValue(t, vec, "dedicated")
	assert.Equal(t, float64(7), v)
}

// TestSetGaugeVec_Reset_DropsLabelsAbsentFromLatestCounts pins
// template_drift's own contract: its label set (drifted realm_template
// versions) genuinely shrinks as realms are upgraded, and a version that no
// longer drifts must stop being reported rather than freeze at its last
// nonzero count forever.
func TestSetGaugeVec_Reset_DropsLabelsAbsentFromLatestCounts(t *testing.T) {
	vec := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "test_reset_gauge"}, []string{"label"})

	setGaugeVec(vec, map[string]int64{"v1": 2, "v2": 1}, true)
	v, _ := gaugeVecValue(t, vec, "v1")
	assert.Equal(t, float64(2), v)

	// "v1" no longer drifts (every realm on it was upgraded) — it must
	// disappear from the vec entirely, not linger at its last count of 2.
	setGaugeVec(vec, map[string]int64{"v2": 1}, true)
	metricCh := make(chan prometheus.Metric, 10)
	vec.Collect(metricCh)
	close(metricCh)
	var labels []string
	for m := range metricCh {
		var dm dto.Metric
		require.NoError(t, m.Write(&dm))
		for _, l := range dm.GetLabel() {
			if l.GetName() == "label" {
				labels = append(labels, l.GetValue())
			}
		}
	}
	assert.ElementsMatch(t, []string{"v2"}, labels, "reset=true must drop a label entirely once it's absent from the latest counts")
}
