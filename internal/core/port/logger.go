package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

// Logger is the structured logging port this service funnels through: HTTP
// middleware (via platform-gincommon), core services, the SQS consumers,
// outbound HTTP/Keycloak clients, and the reconciler binary all end up
// writing through the same sink. It matches platform-gincommon's own
// port.Logger shape exactly (Debug/Info/Warn/Error(msg, fields)), so the
// single Zap-backed logger built once via platform-gincommon's
// logger.NewLogger (cmd/server/main.go, cmd/reconciler/main.go) satisfies
// it directly — no adapter needed at the call site that builds it.
type Logger interface {
	Debug(msg string, fields map[string]any)
	Info(msg string, fields map[string]any)
	Warn(msg string, fields map[string]any)
	Error(msg string, fields map[string]any)
}

// logLevel discriminates the four log severities used by SlogStyleLogger.
// The numeric values mirror slog.Level so call sites that need to branch on
// level can use integer comparison, but this package never imports log/slog.
type logLevel int

const (
	levelDebug logLevel = -4
	levelInfo  logLevel = 0
	levelWarn  logLevel = 4
	levelError logLevel = 8
)

// SlogStyleLogger adapts a Logger to slog-style call conventions — both the
// plain (msg, "key", val, ...) and *Context (ctx, msg, "key", val, ...)
// variants. The zero value is safe: a nil underlying Logger is a no-op
// (it does NOT fall back to stdlib slog, which would route log output
// outside platform-gincommon's governed sink).
type SlogStyleLogger struct {
	log Logger
}

// NewSlogStyleLogger wraps log for slog-style call sites. A nil log produces
// a no-op logger (entries are silently dropped; no fallback to stdlib slog).
func NewSlogStyleLogger(log Logger) SlogStyleLogger {
	return SlogStyleLogger{log: log}
}

// Debug logs at debug level with slog-style alternating key/value args.
func (s SlogStyleLogger) Debug(msg string, args ...any) {
	s.log4(context.Background(), levelDebug, msg, args, false)
}

// Info logs at info level with slog-style alternating key/value args.
func (s SlogStyleLogger) Info(msg string, args ...any) {
	s.log4(context.Background(), levelInfo, msg, args, false)
}

// Warn logs at warn level with slog-style alternating key/value args.
func (s SlogStyleLogger) Warn(msg string, args ...any) {
	s.log4(context.Background(), levelWarn, msg, args, false)
}

// Error logs at error level with slog-style alternating key/value args.
func (s SlogStyleLogger) Error(msg string, args ...any) {
	s.log4(context.Background(), levelError, msg, args, false)
}

// DebugContext logs at debug level, adding a trace_id field from ctx's span
// when present.
func (s SlogStyleLogger) DebugContext(ctx context.Context, msg string, args ...any) {
	s.log4(ctx, levelDebug, msg, args, true)
}

// InfoContext logs at info level, adding a trace_id field from ctx's span
// when present.
func (s SlogStyleLogger) InfoContext(ctx context.Context, msg string, args ...any) {
	s.log4(ctx, levelInfo, msg, args, true)
}

// WarnContext logs at warn level, adding a trace_id field from ctx's span
// when present.
func (s SlogStyleLogger) WarnContext(ctx context.Context, msg string, args ...any) {
	s.log4(ctx, levelWarn, msg, args, true)
}

// ErrorContext logs at error level, adding a trace_id field from ctx's span
// when present.
func (s SlogStyleLogger) ErrorContext(ctx context.Context, msg string, args ...any) {
	s.log4(ctx, levelError, msg, args, true)
}

func (s SlogStyleLogger) log4(ctx context.Context, level logLevel, msg string, args []any, withCtx bool) {
	if s.log == nil {
		return // no-op: nil logger; never falls back to stdlib slog
	}
	if withCtx {
		if traceID := gincommon.SpanTraceID(ctx); traceID != "" {
			args = append(append([]any{}, args...), "trace_id", traceID)
		}
	}
	fields := kvToFields(args)
	switch level {
	case levelDebug:
		s.log.Debug(msg, fields)
	case levelInfo:
		s.log.Info(msg, fields)
	case levelWarn:
		s.log.Warn(msg, fields)
	default:
		s.log.Error(msg, fields)
	}
}

// kvToFields pairs alternating string-key/value slog-style args into a map.
// A trailing unpaired argument, or one whose key isn't a string, is dropped
// rather than panicking.
func kvToFields(args []any) map[string]any {
	fields := make(map[string]any, len(args)/2)
	for i := 0; i+1 < len(args); i += 2 {
		if key, ok := args[i].(string); ok {
			fields[key] = args[i+1]
		}
	}
	return fields
}
