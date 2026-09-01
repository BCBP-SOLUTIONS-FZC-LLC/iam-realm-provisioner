//go:build integration

// Package postgres_test covers LoggerAdapter — the pkg/domain.Logger shim
// that routes platform-pgcommon's slow-query/migration log output into this
// service's own port.Logger sink. Pure in-process adaptation, no database
// required.
package postgres_test

import (
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	pgcommondomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordedLog struct {
	level  string
	msg    string
	fields map[string]any
}

type fakePortLogger struct {
	calls []recordedLog
}

func (f *fakePortLogger) Debug(msg string, fields map[string]any) {
	f.calls = append(f.calls, recordedLog{"debug", msg, fields})
}

func (f *fakePortLogger) Info(msg string, fields map[string]any) {
	f.calls = append(f.calls, recordedLog{"info", msg, fields})
}

func (f *fakePortLogger) Warn(msg string, fields map[string]any) {
	f.calls = append(f.calls, recordedLog{"warn", msg, fields})
}

func (f *fakePortLogger) Error(msg string, fields map[string]any) {
	f.calls = append(f.calls, recordedLog{"error", msg, fields})
}

func TestLoggerAdapter(t *testing.T) {
	t.Parallel()
	fake := &fakePortLogger{}
	adapter := pgadapter.NewLoggerAdapter(fake)

	adapter.Debug("debug msg", pgcommondomain.Field{Key: "a", Value: 1})
	adapter.Info("info msg", pgcommondomain.Field{Key: "b", Value: "two"})
	adapter.Warn("warn msg", pgcommondomain.Field{Key: "c", Value: true})
	adapter.Error("error msg", pgcommondomain.Field{Key: "d", Value: 4.5}, pgcommondomain.Field{Key: "e", Value: "five"})

	require.Len(t, fake.calls, 4)

	assert.Equal(t, "debug", fake.calls[0].level)
	assert.Equal(t, "debug msg", fake.calls[0].msg)
	assert.Equal(t, map[string]any{"a": 1}, fake.calls[0].fields)

	assert.Equal(t, "info", fake.calls[1].level)
	assert.Equal(t, map[string]any{"b": "two"}, fake.calls[1].fields)

	assert.Equal(t, "warn", fake.calls[2].level)
	assert.Equal(t, map[string]any{"c": true}, fake.calls[2].fields)

	assert.Equal(t, "error", fake.calls[3].level)
	assert.Equal(t, map[string]any{"d": 4.5, "e": "five"}, fake.calls[3].fields)
}

func TestLoggerAdapter_NoFields_ProducesEmptyMap(t *testing.T) {
	t.Parallel()
	fake := &fakePortLogger{}
	adapter := pgadapter.NewLoggerAdapter(fake)

	adapter.Info("no fields")

	require.Len(t, fake.calls, 1)
	assert.Empty(t, fake.calls[0].fields)
}
