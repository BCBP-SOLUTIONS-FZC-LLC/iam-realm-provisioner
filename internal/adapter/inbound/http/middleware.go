package http

import (
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// bridgedIdentity is the primitive-typed view of the gateway-injected
// identity used by the bridge helper.
type bridgedIdentity struct {
	UserIDStr   string
	TenantIDStr string
	Roles       []string
	ClientIP    string
	UserAgent   string
}

// parseBridgedIdentity turns primitive gateway header values into a typed
// requestctx.RequestContext. Returns a non-nil error code if either
// identity header is malformed. The reserved system-principal user ID is
// honored specially (§18.3).
func parseBridgedIdentity(in bridgedIdentity) (rc *requestctx.RequestContext, errMsg string) {
	var userID uuid.UUID
	if in.UserIDStr == domain.SystemActorID || in.UserIDStr == "iam-system" {
		userID = uuid.Nil
	} else {
		id, err := uuid.Parse(in.UserIDStr)
		if err != nil {
			return nil, "x-user-id header is not a valid UUID"
		}
		userID = id
	}
	tenantID, err := uuid.Parse(in.TenantIDStr)
	if err != nil {
		return nil, "x-tenant-id header is not a valid UUID"
	}
	return &requestctx.RequestContext{
		UserID: userID, TenantID: tenantID,
		Roles: in.Roles, ClientIP: in.ClientIP, UserAgent: in.UserAgent,
	}, ""
}

// GUCBridgeMiddleware runs after gincommon.ProtectedMiddlewares. It parses
// the gateway-injected identity into typed uuid.UUID values, stores a
// requestctx.RequestContext for handlers, and writes pgcommon.GUCSet so
// every checked-out connection binds `SET LOCAL app.tenant_id` inside the
// transaction (RLS-6). A session-scoped SET would leak across pooled
// backends and defeat RLS.
func GUCBridgeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		platformRc, ok := gincommon.RequestContext(c)
		if !ok {
			c.Next()
			return
		}
		rc, errMsg := parseBridgedIdentity(bridgedIdentity{
			UserIDStr:   platformRc.UserID,
			TenantIDStr: platformRc.TenantID,
			Roles:       platformRc.Roles,
			ClientIP:    platformRc.ClientIP,
			UserAgent:   c.Request.Header.Get("User-Agent"),
		})
		if errMsg != "" {
			er := newErrorResponse(c, "missing_identity_headers", map[string]any{"reason": errMsg})
			er.Status = http.StatusUnauthorized
			c.AbortWithStatusJSON(http.StatusUnauthorized, er)
			return
		}
		ctx := requestctx.WithContext(c.Request.Context(), rc)

		g, _ := pgcommon.GUCSetFromContext(ctx)
		g.UserID = platformRc.UserID
		g.TenantID = platformRc.TenantID
		g.TenantRoles = platformRc.Roles
		ctx = pgcommon.WithGUCSet(ctx, g)

		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// RequireJSONContentType rejects POST/PUT/PATCH requests without
// application/json.
func RequireJSONContentType() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
		default:
			c.Next()
			return
		}
		if c.Request.ContentLength == 0 {
			c.Next()
			return
		}
		if c.ContentType() != "application/json" {
			er := newErrorResponse(c, "unsupported_media_type", nil)
			er.Status = http.StatusUnsupportedMediaType
			c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, er)
			return
		}
		c.Next()
	}
}

// RequireSystemRole gates /api/v1/internal/* to the reserved iam-system
// principal. NetworkPolicy + mTLS is the primary defense; this middleware
// is defense-in-depth (§10.3, §10.6).
func RequireSystemRole() gin.HandlerFunc {
	return func(c *gin.Context) {
		rc, ok := requestctx.FromContext(c.Request.Context())
		if !ok || !rc.IsSystem() {
			er := newErrorResponse(c, "insufficient_role", map[string]any{"reason": "internal route requires iam-system role"})
			er.Status = http.StatusForbidden
			c.AbortWithStatusJSON(http.StatusForbidden, er)
			return
		}
		c.Next()
	}
}

// RequireOperatorRole gates RP-13/RP-14 to the platform-operator role
// (§10.6).
func RequireOperatorRole() gin.HandlerFunc {
	return func(c *gin.Context) {
		rc, ok := requestctx.FromContext(c.Request.Context())
		if !ok || !rc.IsOperator() {
			er := newErrorResponse(c, "insufficient_role", map[string]any{"reason": "operator route requires platform_operator role"})
			er.Status = http.StatusForbidden
			c.AbortWithStatusJSON(http.StatusForbidden, er)
			return
		}
		c.Next()
	}
}

// RequireIdempotencyKey enforces the §5/§9.2 rule that every mutating route
// carries a non-empty Idempotency-Key header (≤256 chars, §10.4).
func RequireIdempotencyKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader("Idempotency-Key")
		if key == "" || len(key) > 256 {
			er := newErrorResponse(c, "missing_idempotency_key", nil)
			er.Status = http.StatusBadRequest
			c.AbortWithStatusJSON(http.StatusBadRequest, er)
			return
		}
		c.Next()
	}
}
