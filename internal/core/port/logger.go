package port

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
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

// SlogStyleLogger adapts a Logger to log/slog's call conventions — both the
// plain (msg, "key", val, ...) and *Context (ctx, msg, "key", val, ...)
// variants. The zero value is safe to use directly and falls back to the
// top-level slog functions.
type SlogStyleLogger struct {
	log Logger
}

// NewSlogStyleLogger wraps log for slog-style call sites. A nil log behaves
// exactly like the zero value (falls back to top-level slog).
func NewSlogStyleLogger(log Logger) SlogStyleLogger {
	return SlogStyleLogger{log: log}
}

// Debug logs at debug level with slog-style alternating key/value args.
func (s SlogStyleLogger) Debug(msg string, args ...any) {
	s.log4(context.Background(), slog.LevelDebug, msg, args, false)
}

// Info logs at info level with slog-style alternating key/value args.
func (s SlogStyleLogger) Info(msg string, args ...any) {
	s.log4(context.Background(), slog.LevelInfo, msg, args, false)
}

// Warn logs at warn level with slog-style alternating key/value args.
func (s SlogStyleLogger) Warn(msg string, args ...any) {
	s.log4(context.Background(), slog.LevelWarn, msg, args, false)
}

// Error logs at error level with slog-style alternating key/value args.
func (s SlogStyleLogger) Error(msg string, args ...any) {
	s.log4(context.Background(), slog.LevelError, msg, args, false)
}

// DebugContext logs at debug level, adding a trace_id field from ctx's span
// when present.
func (s SlogStyleLogger) DebugContext(ctx context.Context, msg string, args ...any) {
	s.log4(ctx, slog.LevelDebug, msg, args, true)
}

// InfoContext logs at info level, adding a trace_id field from ctx's span
// when present.
func (s SlogStyleLogger) InfoContext(ctx context.Context, msg string, args ...any) {
	s.log4(ctx, slog.LevelInfo, msg, args, true)
}

// WarnContext logs at warn level, adding a trace_id field from ctx's span
// when present.
func (s SlogStyleLogger) WarnContext(ctx context.Context, msg string, args ...any) {
	s.log4(ctx, slog.LevelWarn, msg, args, true)
}

// ErrorContext logs at error level, adding a trace_id field from ctx's span
// when present.
func (s SlogStyleLogger) ErrorContext(ctx context.Context, msg string, args ...any) {
	s.log4(ctx, slog.LevelError, msg, args, true)
}

func (s SlogStyleLogger) log4(ctx context.Context, level slog.Level, msg string, args []any, withCtx bool) {
	if withCtx {
		if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
			args = append(append([]any{}, args...), "trace_id", span.SpanContext().TraceID().String())
		}
	}
	if s.log == nil {
		slog.Default().Log(ctx, level, msg, args...)
		return
	}
	fields := kvToFields(args)
	switch level {
	case slog.LevelDebug:
		s.log.Debug(msg, fields)
	case slog.LevelInfo:
		s.log.Info(msg, fields)
	case slog.LevelWarn:
		s.log.Warn(msg, fields)
	case slog.LevelError:
		s.log.Error(msg, fields)
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
