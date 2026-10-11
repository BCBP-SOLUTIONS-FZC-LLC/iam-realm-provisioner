package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

type recordingLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *recordingLogger) Debug(string, map[string]any) {}
func (l *recordingLogger) Info(string, map[string]any)  {}
func (l *recordingLogger) Warn(msg string, _ map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}
func (l *recordingLogger) Error(string, map[string]any) {}

// A run's metrics are PUT to the Pushgateway group for its cronjob, so the
// next run replaces (not accumulates onto) the previous one.
func TestPushMetrics_PutsToCronjobGroup(t *testing.T) {
	var method, path string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer gw.Close()
	t.Setenv("PROMETHEUS_PUSHGATEWAY_URL", gw.URL)
	log := &recordingLogger{}

	pushMetrics(context.Background(), "trial-expiry", port.NewSlogStyleLogger(log))

	assert.Equal(t, http.MethodPut, method)
	assert.Equal(t, "/metrics/job/iam-realm-provisioner-reconciler/cronjob/trial-expiry", path)
	assert.Empty(t, log.warns)
}

// A failed push is logged and never fails the job — even on a context the
// job itself already canceled.
func TestPushMetrics_GatewayError_LoggedNotFatal(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer gw.Close()
	t.Setenv("PROMETHEUS_PUSHGATEWAY_URL", gw.URL)
	log := &recordingLogger{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	pushMetrics(ctx, "ledger-prune", port.NewSlogStyleLogger(log))

	assert.Len(t, log.warns, 1)
}

func TestPushMetrics_URLUnset_Skips(t *testing.T) {
	t.Setenv("PROMETHEUS_PUSHGATEWAY_URL", "")
	log := &recordingLogger{}
	pushMetrics(context.Background(), "trial-expiry", port.NewSlogStyleLogger(log))
	assert.Empty(t, log.warns)
}
