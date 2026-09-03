package http

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/gin-gonic/gin"
)

// errorLogger is the shared gincommon-backed Logger, set once by NewRouter.
var errorLogger port.Logger

// ErrorResponse is the flat wire shape (§17): {error, status, trace_id,
// request_id, details?}.
type ErrorResponse struct {
	Error     string         `json:"error"`
	Status    int            `json:"status"`
	TraceID   string         `json:"trace_id,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

func newErrorResponse(c *gin.Context, code string, details map[string]any) ErrorResponse {
	er := ErrorResponse{Error: code, Details: details}
	if c != nil {
		// Same correlation helpers O&M uses: IDs TracingMiddleware /
		// RequestIDMiddleware stored on the Gin context, not a raw OTel span
		// read that would bypass gincommon when those middlewares have run.
		er.TraceID = gincommon.TraceIDFromContext(c)
		if rid := gincommon.RequestIDFromContext(c); rid != "" {
			er.RequestID = rid
		} else if rid := c.GetHeader(gincommon.HeaderRequestID); rid != "" {
			er.RequestID = rid
		} else if rid := c.Writer.Header().Get(gincommon.HeaderRequestIDResponse); rid != "" {
			er.RequestID = rid
		}
	}
	return er
}

// HandleError maps a *domain.Error to its frozen §17 status/code, or falls
// back to 500 internal_error for anything unclassified (logged so an
// unhandled error path is never silent).
func HandleError(c *gin.Context, err error) {
	var de *domain.Error
	if ok := errors.As(err, &de); ok {
		er := newErrorResponse(c, string(de.Code), de.Details)
		er.Status = de.Status()
		c.AbortWithStatusJSON(er.Status, er)
		return
	}
	// Raw pgconn.PgError that was not caught and translated by the service
	// layer. wrapConnErr maps classes 08/53/57/58 to ErrDBUnavailable, so
	// the DomainError branch above is the production path. This fallback
	// still classifies a leaked PgError (tests, future missed wrap) into
	// 503 db_unavailable per §17 without importing pgconn: classes 08/53
	// via pgcommon helpers, 57/58 via the SQLSTATE text pgconn puts in
	// Error().
	if pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) || isOperatorOrSystemErrorSQLState(err) {
		er := newErrorResponse(c, string(domain.ErrDBUnavailable), nil)
		er.Status = http.StatusServiceUnavailable
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, er)
		return
	}
	if errorLogger != nil {
		errorLogger.Error("unhandled 500 error", map[string]any{"error_type": fmt.Sprintf("%T", err), "error": err.Error()})
	}
	er := newErrorResponse(c, "internal_error", nil)
	er.Status = http.StatusInternalServerError
	c.AbortWithStatusJSON(http.StatusInternalServerError, er)
}

// isOperatorOrSystemErrorSQLState reports whether err is a Postgres error
// in SQLSTATE class 57 (operator_intervention) or 58 (system_error).
// pgcommon v1.3.0 has no dedicated helper for these two, so we match the
// "(SQLSTATE 57…)" / "(SQLSTATE 58…)" text pgconn puts in Error() instead
// of type-asserting *pgconn.PgError (inbound HTTP must not import pgconn).
func isOperatorOrSystemErrorSQLState(err error) bool {
	if !pgcommon.IsPgError(err) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLSTATE 57") || strings.Contains(msg, "SQLSTATE 58")
}
