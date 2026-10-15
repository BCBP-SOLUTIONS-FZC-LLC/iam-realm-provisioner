package http

import (
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
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
	switch in.UserIDStr {
	case domain.SystemActorID, "iam-system":
		// iam-system sentinel (…00a1) → uuid.Nil (§18.3). Role in x-tenant-roles
		// (IsSystem) is the authoritative discriminator for per-route enforcement.
		userID = uuid.Nil
	case domain.CommercialActorID, "commercial-system":
		// commercial-system (CD-19, …00c1) → canonical CommercialActorID UUID.
		// Unlike iam-system, commercial-system maps to its actual UUID so that
		// audit entries and structured logs record the correct actor (not uuid.Nil).
		// IsCommercial() checks the role in x-tenant-roles, not the UUID, so this
		// change does not affect route-level allow-list enforcement.
		userID = uuid.MustParse(domain.CommercialActorID)
	default:
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
// All routes use this except RP-3/RP-4/RP-11 which use RequireSystemOrCommercial.
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

// RequireSystemOrCommercial gates the three Commercial-caller routes (CD-19):
//
//	POST /api/v1/internal/tenants/:id/convert            (RP-3)
//	POST /api/v1/internal/tenants/:id/revert-conversion  (RP-4)
//	GET  /api/v1/internal/tenants/:id/realm              (RP-11)
//
// It accepts iam-system (existing callers) OR commercial-system (Billing,
// CD-19). All other /internal/* routes remain gated by RequireSystemRole
// (iam-system only) — commercial-system callers hitting those routes receive
// 403 insufficient_role. NetworkPolicy + mesh AuthorizationPolicy are the
// primary enforcement layer; this middleware is defense-in-depth.
func RequireSystemOrCommercial() gin.HandlerFunc {
	return func(c *gin.Context) {
		rc, ok := requestctx.FromContext(c.Request.Context())
		if !ok || (!rc.IsSystem() && !rc.IsCommercial()) {
			er := newErrorResponse(c, "insufficient_role", map[string]any{
				"reason": "route requires iam-system or commercial-system role (CD-19 allow-list: RP-3, RP-4, RP-11)",
			})
			er.Status = http.StatusForbidden
			c.AbortWithStatusJSON(http.StatusForbidden, er)
			return
		}
		c.Next()
	}
}

// RequireTenantMatch rejects a /tenants/:id request whose path tenant is not
// the caller's x-tenant-id. GUCBridgeMiddleware binds the RLS GUC from the
// header while handlers act on the path tenant: with the two disagreeing,
// RLS still blocks the database writes, but the Keycloak-effect-first flows
// call Keycloak BEFORE their first write — a mismatched caller could
// create or change a realm whose local commit is then rejected, leaving
// Keycloak and the local mirror diverged. Callers (O&M, Billing, the
// Signup BFF) always send the target tenant (RLS-5), so this only ever
// rejects a misbehaving or forged request. A malformed :id passes through
// to the handler's own 400.
func RequireTenantMatch() gin.HandlerFunc {
	return func(c *gin.Context) {
		pathTenant, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.Next()
			return
		}
		if !tenantMatchesCaller(c, pathTenant) {
			return
		}
		c.Next()
	}
}

// tenantMatchesCaller reports whether tenantID is the caller's x-tenant-id,
// aborting with 403 tenant_mismatch when it is not.
func tenantMatchesCaller(c *gin.Context, tenantID uuid.UUID) bool {
	rc, ok := requestctx.FromContext(c.Request.Context())
	if ok && rc.TenantID == tenantID {
		return true
	}
	er := newErrorResponse(c, "tenant_mismatch", map[string]any{"reason": "the request's tenant must match x-tenant-id (RLS-5)"})
	er.Status = http.StatusForbidden
	c.AbortWithStatusJSON(http.StatusForbidden, er)
	return false
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
