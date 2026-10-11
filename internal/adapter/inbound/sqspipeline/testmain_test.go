package sqspipeline

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// TestMain registers business metrics once per test binary (gincommon
// rejects a second identity), so ConsumedSchemaViolationsTotal is live.
func TestMain(m *testing.M) {
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		Domain: "iam", Environment: "test", ServiceName: "iam-realm-provisioner", BuildVersion: "test",
	})
	metrics.Register()
	os.Exit(m.Run())
}

// fakeLogger is a port.Logger that records Warn calls.
type fakeLogger struct {
	warnCalls int32
	mu        sync.Mutex
	lastWarn  map[string]any
}

func (f *fakeLogger) Debug(string, map[string]any) {}
func (f *fakeLogger) Info(string, map[string]any)  {}
func (f *fakeLogger) Warn(_ string, fields map[string]any) {
	f.mu.Lock()
	f.lastWarn = fields
	f.mu.Unlock()
	atomic.AddInt32(&f.warnCalls, 1)
}
func (f *fakeLogger) Error(string, map[string]any) {}
