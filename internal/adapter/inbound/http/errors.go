package http

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"
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
		span := trace.SpanFromContext(c.Request.Context())
		if span.SpanContext().IsValid() {
			er.TraceID = span.SpanContext().TraceID().String()
		}
		if rid := gincommon.RequestIDFromContext(c); rid != "" {
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
	if errorLogger != nil {
		errorLogger.Error("unhandled 500 error", map[string]any{"error_type": fmt.Sprintf("%T", err), "error": err.Error()})
	}
	er := newErrorResponse(c, "internal_error", nil)
	er.Status = http.StatusInternalServerError
	c.AbortWithStatusJSON(http.StatusInternalServerError, er)
}
