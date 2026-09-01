// Package requestctx carries the validated caller identity extracted from
// trusted gateway headers (x-user-id, x-tenant-id, x-tenant-roles) through
// the request scope. Handlers and services read this struct rather than
// reaching back into gin.Context, keeping core packages framework-free.
package requestctx

import (
	"context"
	"slices"

	"github.com/google/uuid"
)

type contextKey struct{}

// RequestContext holds the validated caller identity for a single request.
type RequestContext struct {
	UserID    uuid.UUID
	TenantID  uuid.UUID
	Roles     []string
	ClientIP  string
	UserAgent string
}

// WithContext returns a new context carrying rc.
func WithContext(ctx context.Context, rc *RequestContext) context.Context {
	return context.WithValue(ctx, contextKey{}, rc)
}

// FromContext retrieves the RequestContext set by the auth middleware.
func FromContext(ctx context.Context) (*RequestContext, bool) {
	rc, ok := ctx.Value(contextKey{}).(*RequestContext)
	return rc, ok && rc != nil
}

// HasRole reports whether the caller was granted the given role by the gateway.
func (rc *RequestContext) HasRole(role string) bool {
	return slices.Contains(rc.Roles, role)
}

// IsOperator reports whether the caller carries the platform-level operator
// role (§10.6) — required for RP-13/RP-14.
func (rc *RequestContext) IsOperator() bool {
	return rc.HasRole("platform_operator")
}

// IsSystem reports whether the caller is the reserved iam-system principal
// (§18.3: x-user-id=…00a1) — the identity O&M/Billing use on RP→this
// service internal calls.
func (rc *RequestContext) IsSystem() bool {
	return rc.HasRole("iam-system")
}
