package port

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// recordingHandler is a slog.Handler test double that captures every emitted
// slog.Record, so assertions can inspect level/message/attrs directly instead
// of parsing formatted log lines.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) last(t *testing.T) slog.Record {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) == 0 {
		t.Fatal("no records captured")
	}
	return h.records[len(h.records)-1]
}

func attrMap(r slog.Record) map[string]any {
	m := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	return m
}

// mockLogger implements the Logger port and records every call so tests can
// assert exactly which level/msg/fields SlogStyleLogger forwarded.
type mockLogger struct {
	mu    sync.Mutex
	calls []mockCall
}

type mockCall struct {
	level  string
	msg    string
	fields map[string]any
}

func (m *mockLogger) record(level, msg string, fields map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, mockCall{level: level, msg: msg, fields: fields})
}

func (m *mockLogger) Debug(msg string, fields map[string]any) { m.record("debug", msg, fields) }
func (m *mockLogger) Info(msg string, fields map[string]any)  { m.record("info", msg, fields) }
func (m *mockLogger) Warn(msg string, fields map[string]any)  { m.record("warn", msg, fields) }
func (m *mockLogger) Error(msg string, fields map[string]any) { m.record("error", msg, fields) }

func (m *mockLogger) last(t *testing.T) mockCall {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		t.Fatal("no calls recorded")
	}
	return m.calls[len(m.calls)-1]
}

// validSpanContext builds a non-zero, valid trace.SpanContext so
// span.SpanContext().IsValid() reports true without needing a real tracer.
func validSpanContext() trace.SpanContext {
	var traceID trace.TraceID
	traceID[0] = 1
	var spanID trace.SpanID
	spanID[0] = 1
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
}

func TestNewSlogStyleLogger(t *testing.T) {
	m := &mockLogger{}
	s := NewSlogStyleLogger(m)
	s.Info("hello")
	call := m.last(t)
	if call.level != "info" || call.msg != "hello" {
		t.Errorf("got %+v, want level=info msg=hello", call)
	}
}

func TestSlogStyleLogger_LevelMethods_ForwardToLogger(t *testing.T) {
	cases := []struct {
		name string
		call func(s SlogStyleLogger, msg string, args ...any)
		want string
	}{
		{"Debug", SlogStyleLogger.Debug, "debug"},
		{"Info", SlogStyleLogger.Info, "info"},
		{"Warn", SlogStyleLogger.Warn, "warn"},
		{"Error", SlogStyleLogger.Error, "error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockLogger{}
			s := NewSlogStyleLogger(m)
			tc.call(s, "msg", "key", "val")
			got := m.last(t)
			if got.level != tc.want {
				t.Errorf("level = %q, want %q", got.level, tc.want)
			}
			if got.msg != "msg" {
				t.Errorf("msg = %q, want %q", got.msg, "msg")
			}
			if got.fields["key"] != "val" {
				t.Errorf("fields[key] = %v, want %q", got.fields["key"], "val")
			}
		})
	}
}

func TestSlogStyleLogger_ContextMethods_ForwardToLogger(t *testing.T) {
	cases := []struct {
		name string
		call func(s SlogStyleLogger, ctx context.Context, msg string, args ...any)
		want string
	}{
		{"DebugContext", SlogStyleLogger.DebugContext, "debug"},
		{"InfoContext", SlogStyleLogger.InfoContext, "info"},
		{"WarnContext", SlogStyleLogger.WarnContext, "warn"},
		{"ErrorContext", SlogStyleLogger.ErrorContext, "error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockLogger{}
			s := NewSlogStyleLogger(m)
			ctx := context.Background()
			tc.call(s, ctx, "msg", "key", "val")
			got := m.last(t)
			if got.level != tc.want {
				t.Errorf("level = %q, want %q", got.level, tc.want)
			}
			if got.fields["key"] != "val" {
				t.Errorf("fields[key] = %v, want %q", got.fields["key"], "val")
			}
			if _, ok := got.fields["trace_id"]; ok {
				t.Error("trace_id should not be set without a valid span in ctx")
			}
		})
	}
}

func TestSlogStyleLogger_ContextMethods_AddTraceIDWhenSpanValid(t *testing.T) {
	sc := validSpanContext()
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	m := &mockLogger{}
	s := NewSlogStyleLogger(m)
	s.InfoContext(ctx, "msg", "key", "val")

	got := m.last(t)
	if got.fields["trace_id"] != sc.TraceID().String() {
		t.Errorf("fields[trace_id] = %v, want %v", got.fields["trace_id"], sc.TraceID().String())
	}
	if got.fields["key"] != "val" {
		t.Errorf("fields[key] = %v, want %q", got.fields["key"], "val")
	}
}

func TestSlogStyleLogger_ZeroValue_FallsBackToSlogDefault(t *testing.T) {
	h := &recordingHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(old)

	var s SlogStyleLogger // zero value: log == nil
	s.Warn("fallback", "key", "val")

	rec := h.last(t)
	if rec.Level != slog.LevelWarn {
		t.Errorf("Level = %v, want %v", rec.Level, slog.LevelWarn)
	}
	if rec.Message != "fallback" {
		t.Errorf("Message = %q, want %q", rec.Message, "fallback")
	}
	attrs := attrMap(rec)
	if attrs["key"] != "val" {
		t.Errorf("attrs[key] = %v, want %q", attrs["key"], "val")
	}
}

func TestSlogStyleLogger_ZeroValue_ContextFallbackAddsTraceID(t *testing.T) {
	h := &recordingHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(old)

	sc := validSpanContext()
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	var s SlogStyleLogger
	s.ErrorContext(ctx, "fallback-ctx")

	rec := h.last(t)
	if rec.Level != slog.LevelError {
		t.Errorf("Level = %v, want %v", rec.Level, slog.LevelError)
	}
	attrs := attrMap(rec)
	if attrs["trace_id"] != sc.TraceID().String() {
		t.Errorf("attrs[trace_id] = %v, want %v", attrs["trace_id"], sc.TraceID().String())
	}
}

// log4's default branch (an out-of-range level) is unreachable through the
// public API, which only ever passes the four named levels — but it exists
// as a defensive fallback, so exercise it directly (white-box, same package).
func TestLog4_UnknownLevel_FallsBackToError(t *testing.T) {
	m := &mockLogger{}
	s := NewSlogStyleLogger(m)

	s.log4(context.Background(), slog.Level(99), "weird", []any{"k", "v"}, false)

	got := m.last(t)
	if got.level != "error" {
		t.Errorf("level = %q, want %q", got.level, "error")
	}
	if got.fields["k"] != "v" {
		t.Errorf("fields[k] = %v, want %q", got.fields["k"], "v")
	}
}

func TestKvToFields(t *testing.T) {
	cases := []struct {
		name string
		args []any
		want map[string]any
	}{
		{"nil args", nil, map[string]any{}},
		{"empty args", []any{}, map[string]any{}},
		{"even paired args", []any{"a", 1, "b", 2}, map[string]any{"a": 1, "b": 2}},
		{"trailing unpaired arg is dropped", []any{"a", 1, "b"}, map[string]any{"a": 1}},
		{"non-string key pair is dropped, others kept", []any{1, "a", "b", 2}, map[string]any{"b": 2}},
		{"single unpaired arg", []any{"a"}, map[string]any{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := kvToFields(tc.args)
			if len(got) != len(tc.want) {
				t.Fatalf("kvToFields(%v) = %v, want %v", tc.args, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("kvToFields(%v)[%q] = %v, want %v", tc.args, k, got[k], v)
				}
			}
		})
	}
}
