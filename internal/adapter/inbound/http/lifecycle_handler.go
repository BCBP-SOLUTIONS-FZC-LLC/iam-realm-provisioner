package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"
)

// ── RP-11: GET realm (read-only) ─────────────────────────────────────────

// GetRealm handles RP-11 (GET /tenants/:id/realm).
//
// @Summary      RP-11 — Get realm info including active IdP count
// @Description  Returns the tenant's current realm record: realm name, strategy (shared/dedicated), lifecycle status, Keycloak shard, record_version, and idp_count. idp_count is the number of active (status=active) SAML/OIDC IdP configurations — excludes disabled and pending_validation. idp_count is always 0 for shared-strategy (trial) tenants (RP-6 blocks IdP configuration there). On IdP repository failure the endpoint returns 503 db_unavailable or 503 request_timeout — never a silent 0 that could permit an SSO-losing plan downgrade (CD-20). Named callers: Core (iam-org-membership, iam-system principal, I-18 plan-change-check — reads idp_count to block an SSO-losing plan downgrade, CD-20), Billing (commercial-system principal, CD-19 allow-list — interim SSO guard), Signup BFF, Org & Membership (both iam-system). CD-19 NetworkPolicy and mesh AuthorizationPolicy (billing namespace → this route) are implemented in deploy/helm/templates/. Read-only; no side effects.
// @Tags         Lifecycle
// @Produce      json
// @Param        id  path      string  true  "Tenant UUID"  format(uuid)
// @Success      200 {object}  service.GetRealmResult
// @Failure      400 {object}  ProblemResponse  "INVALID_REQUEST"
// @Failure      404 {object}  ProblemResponse  "TENANT_REALM_NOT_FOUND"
// @Failure      503 {object}  ProblemResponse  "DB_UNAVAILABLE or REQUEST_TIMEOUT (IdP count query failed — never returns silent zero)"
// @Security     SystemRole
// @Security     TenantID
// @Router       /tenants/{id}/realm [get]
func (h *InternalHandler) GetRealm(c *gin.Context) {
	if h.Lifecycle == nil {
		h.fallback.GetRealm(c)
		return
	}
	tenantID, perr := parseTenantID(c)
	if perr != nil {
		gincommon.AbortWithProblem(c, perr)
		return
	}
	result, err := h.Lifecycle.GetRealm(c.Request.Context(), tenantID)
	if err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
		return
	}
	c.JSON(http.StatusOK, result)
}

// ── RP-10: reactivate ─────────────────────────────────────────────────────

type reactivateRequest struct {
	Reason string `json:"reason"`
}

// Reactivate handles RP-10 (POST /tenants/:id/reactivate).
//
// @Summary      RP-10 — Reactivate suspended tenant realm
// @Description  Reverses both a billing-lapse suspension and an operator (RP-14) suspension from one endpoint. Re-enables the Keycloak realm and clears the suspended status. Body is optional; reason is informational only. Idempotent on Idempotency-Key reuse.
// @Tags         Lifecycle
// @Accept       json
// @Produce      json
// @Param        id       path      string             false  "Tenant UUID"  format(uuid)
// @Param        request  body      reactivateRequest  false  "Optional reactivation reason"
// @Success      200      {object}  service.ReactivateResult
// @Failure      400      {object}  ProblemResponse  "INVALID_REQUEST OR MISSING_IDEMPOTENCY_KEY"
// @Failure      404      {object}  ProblemResponse  "TENANT_REALM_NOT_FOUND"
// @Failure      409      {object}  ProblemResponse  "OPERATION_IN_PROGRESS OR TENANT_OFFBOARDED OR OPTIMISTIC_LOCK_CONFLICT"
// @Failure      500      {object}  ProblemResponse
// @Security     SystemRole
// @Security     TenantID
// @Security     IdempotencyKey
// @Router       /tenants/{id}/reactivate [post]
func (h *InternalHandler) Reactivate(c *gin.Context) {
	if h.Lifecycle == nil {
		h.fallback.Reactivate(c)
		return
	}
	tenantID, perr := parseTenantID(c)
	if perr != nil {
		gincommon.AbortWithProblem(c, perr)
		return
	}
	var req reactivateRequest
	_ = c.ShouldBindJSON(&req) //nolint:errcheck // body is optional (reason only) — a missing/empty body just leaves req zero-valued
	result, err := h.Lifecycle.Reactivate(c.Request.Context(), idempotencyKey(c), tenantID, domain.ReactivationSourceOperator, truncateReason(req.Reason, 500), nil)
	if err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
		return
	}
	c.JSON(http.StatusOK, result)
}

// ── RP-12: expire-trial (reconciler-only) ────────────────────────────────

// ExpireTrial handles RP-12 (POST /tenants/:id/expire-trial).
//
// @Summary      RP-12 — Expire trial tenant
// @Description  Transitions a trial tenant to expired/disabled status after the grace period. Called by the RP-C1 reconciler CronJob; not intended for direct operator use. Disables the Keycloak realm and records the expiry timestamp. Idempotent on Idempotency-Key reuse.
// @Tags         Lifecycle
// @Produce      json
// @Param        id  path      string  true  "Tenant UUID"  format(uuid)
// @Success      200 {object}  service.ExpireTrialResult
// @Failure      400 {object}  ProblemResponse  "INVALID_REQUEST OR MISSING_IDEMPOTENCY_KEY"
// @Failure      404 {object}  ProblemResponse  "TENANT_REALM_NOT_FOUND"
// @Failure      409 {object}  ProblemResponse  "OPERATION_IN_PROGRESS OR REALM_ALREADY_DEDICATED OR TENANT_OFFBOARDED"
// @Failure      500 {object}  ProblemResponse
// @Security     SystemRole
// @Security     TenantID
// @Security     IdempotencyKey
// @Router       /tenants/{id}/expire-trial [post]
func (h *InternalHandler) ExpireTrial(c *gin.Context) {
	if h.Lifecycle == nil {
		h.fallback.ExpireTrial(c)
		return
	}
	tenantID, perr := parseTenantID(c)
	if perr != nil {
		gincommon.AbortWithProblem(c, perr)
		return
	}
	result, err := h.Lifecycle.ExpireTrial(c.Request.Context(), idempotencyKey(c), tenantID, 30)
	if err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
		return
	}
	c.JSON(http.StatusOK, result)
}

// ── RP-14: emergency operator suspension ─────────────────────────────────

type suspendRequest struct {
	Reason string `json:"reason" binding:"required,max=500"`
}

// Suspend handles RP-14 (POST /tenants/:id/suspend).
//
// @Summary      RP-14 — Emergency operator suspension
// @Description  Operator-only. Immediately disables the tenant's Keycloak realm and transitions status to suspended. Reason is required for the audit trail. Reversed by RP-10 Reactivate. Idempotent on Idempotency-Key reuse.
// @Tags         Lifecycle
// @Accept       json
// @Produce      json
// @Param        id       path      string          true  "Tenant UUID"  format(uuid)
// @Param        request  body      suspendRequest  true  "Suspension reason (required)"
// @Success      200      {object}  service.SuspendResult
// @Failure      400      {object}  ProblemResponse  "INVALID_REQUEST OR MISSING_IDEMPOTENCY_KEY"
// @Failure      403      {object}  ProblemResponse  "INSUFFICIENT_ROLE (operator role required)"
// @Failure      404      {object}  ProblemResponse  "TENANT_REALM_NOT_FOUND"
// @Failure      409      {object}  ProblemResponse  "OPERATION_IN_PROGRESS OR TENANT_OFFBOARDED"
// @Failure      500      {object}  ProblemResponse
// @Security     SystemRole
// @Security     TenantID
// @Security     IdempotencyKey
// @Router       /tenants/{id}/suspend [post]
func (h *InternalHandler) Suspend(c *gin.Context) {
	if h.Lifecycle == nil {
		h.fallback.Suspend(c)
		return
	}
	tenantID, perr := parseTenantID(c)
	if perr != nil {
		gincommon.AbortWithProblem(c, perr)
		return
	}
	var req suspendRequest
	if err := gincommon.ShouldBindJSON(c, &req); err != nil {
		gincommon.AbortWithProblem(c, err)
		return
	}
	result, err := h.Lifecycle.Suspend(c.Request.Context(), idempotencyKey(c), tenantID, req.Reason)
	if err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
		return
	}
	c.JSON(http.StatusOK, result)
}

// ── RP-15: delete user (idempotent) ──────────────────────────────────────

// DeleteUser handles RP-15 (DELETE /tenants/:id/users/:keycloak_user_id).
//
// @Summary      RP-15 — Delete user from tenant realm
// @Description  Permanently deletes the Keycloak user from the tenant's realm. Idempotent: deleting an already-absent user returns 204. Part of the GDPR erasure flow — called by Org & Membership on membership deletion.
// @Tags         Users
// @Produce      json
// @Param        id                path  string  true  "Tenant UUID"        format(uuid)
// @Param        keycloak_user_id  path  string  true  "Keycloak user UUID"  format(uuid)
// @Success      204               "No Content"
// @Failure      400               {object}  ProblemResponse  "INVALID_REQUEST"
// @Failure      404               {object}  ProblemResponse  "TENANT_REALM_NOT_FOUND"
// @Failure      500               {object}  ProblemResponse
// @Security     SystemRole
// @Security     TenantID
// @Router       /tenants/{id}/users/{keycloak_user_id} [delete]
func (h *InternalHandler) DeleteUser(c *gin.Context) {
	if h.Lifecycle == nil {
		h.fallback.DeleteUser(c)
		return
	}
	tenantID, perr := parseTenantID(c)
	if perr != nil {
		gincommon.AbortWithProblem(c, perr)
		return
	}
	keycloakUserID, err := uuid.Parse(c.Param("keycloak_user_id"))
	if err != nil {
		gincommon.AbortWithProblem(c, problem.NewError(realmInvalidRequest, "invalid keycloak_user_id"))
		return
	}
	if err := h.Lifecycle.DeleteUser(c.Request.Context(), tenantID, keycloakUserID); err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
		return
	}
	c.Status(http.StatusNoContent)
}

// ── RP-16: revoke sessions (idempotent, best-effort/fail-open on O&M's
// side) ───────────────────────────────────────────────────────────────────

// RevokeSessions handles RP-16 (POST /tenants/:id/users/:keycloak_user_id/revoke-sessions).
//
// @Summary      RP-16 — Revoke all active sessions for a user
// @Description  Logs out the Keycloak user from all active sessions across all clients. Idempotent: revoking an already-session-free user returns 204. Called on membership suspension or offboarding.
// @Tags         Users
// @Produce      json
// @Param        id                path  string  true  "Tenant UUID"        format(uuid)
// @Param        keycloak_user_id  path  string  true  "Keycloak user UUID"  format(uuid)
// @Success      204               "No Content"
// @Failure      400               {object}  ProblemResponse  "INVALID_REQUEST"
// @Failure      404               {object}  ProblemResponse  "TENANT_REALM_NOT_FOUND OR KEYCLOAK_USER_NOT_FOUND"
// @Failure      500               {object}  ProblemResponse
// @Security     SystemRole
// @Security     TenantID
// @Router       /tenants/{id}/users/{keycloak_user_id}/revoke-sessions [post]
func (h *InternalHandler) RevokeSessions(c *gin.Context) {
	if h.Lifecycle == nil {
		h.fallback.RevokeSessions(c)
		return
	}
	tenantID, perr := parseTenantID(c)
	if perr != nil {
		gincommon.AbortWithProblem(c, perr)
		return
	}
	keycloakUserID, err := uuid.Parse(c.Param("keycloak_user_id"))
	if err != nil {
		gincommon.AbortWithProblem(c, problem.NewError(realmInvalidRequest, "invalid keycloak_user_id"))
		return
	}
	if err := h.Lifecycle.RevokeSessions(c.Request.Context(), tenantID, keycloakUserID); err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
		return
	}
	c.Status(http.StatusNoContent)
}
