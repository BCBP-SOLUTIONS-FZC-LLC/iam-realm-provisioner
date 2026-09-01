package postgres

import (
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/domain"
)

// LoggerAdapter implements platform-pgcommon's pkg/domain.Logger on top of
// port.Logger, so pgcommon's slow-query and migration log output routes
// into this service's own gincommon-backed sink.
type LoggerAdapter struct {
	log port.Logger
}

var _ domain.Logger = LoggerAdapter{}

// NewLoggerAdapter wraps log as a pgcommon domain.Logger.
func NewLoggerAdapter(log port.Logger) LoggerAdapter {
	return LoggerAdapter{log: log}
}

// Debug logs at debug level, converting pgcommon's Field slice to port.Logger's map shape.
func (a LoggerAdapter) Debug(msg string, fields ...domain.Field) { a.log.Debug(msg, fieldMap(fields)) }

// Info logs at info level, converting pgcommon's Field slice to port.Logger's map shape.
func (a LoggerAdapter) Info(msg string, fields ...domain.Field) { a.log.Info(msg, fieldMap(fields)) }

// Warn logs at warn level, converting pgcommon's Field slice to port.Logger's map shape.
func (a LoggerAdapter) Warn(msg string, fields ...domain.Field) { a.log.Warn(msg, fieldMap(fields)) }

// Error logs at error level, converting pgcommon's Field slice to port.Logger's map shape.
func (a LoggerAdapter) Error(msg string, fields ...domain.Field) { a.log.Error(msg, fieldMap(fields)) }

func fieldMap(fields []domain.Field) map[string]any {
	m := make(map[string]any, len(fields))
	for _, f := range fields {
		m[f.Key] = f.Value
	}
	return m
}
