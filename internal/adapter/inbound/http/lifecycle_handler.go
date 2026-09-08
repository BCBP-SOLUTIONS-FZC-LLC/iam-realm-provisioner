package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// ── RP-11: GET realm (read-only) ─────────────────────────────────────────

// GetRealm handles RP-11 (GET /tenants/:id/realm).
//
// @Summary      RP-11 — Get realm info
// @Description  Returns the current realm record for the tenant: realm name, strategy (shared/dedicated), lifecycle status, Keycloak shard, and key timestamps. Read-only; no side effects.
// @Tags         Lifecycle
// @Produce      json
// @Param        id  path      string  true  "Tenant UUID"  format(uuid)
// @Success      200 {object}  service.GetRealmResult
// @Failure      400 {object}  ErrorResponse  "invalid_request"
// @Failure      404 {object}  ErrorResponse  "realm_not_found"
// @Failure      500 {object}  ErrorResponse
// @Security     SystemRole
// @Security     TenantID
// @Router       /tenants/{id}/realm [get]
func (h *InternalHandler) GetRealm(c *gin.Context) {
	if h.Lifecycle == nil {
		h.fallback.GetRealm(c)
		return
	}
	tenantID, err := parseTenantID(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	result, err := h.Lifecycle.GetRealm(c.Request.Context(), tenantID)
	if err != nil {
		HandleError(c, err)
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
// @Failure      400      {object}  ErrorResponse  "invalid_request OR missing_idempotency_key"
// @Failure      404      {object}  ErrorResponse  "realm_not_found"
// @Failure      409      {object}  ErrorResponse  "operation_in_progress OR invalid_realm_state"
// @Failure      500      {object}  ErrorResponse
// @Security     SystemRole
// @Security     TenantID
// @Security     IdempotencyKey
// @Router       /tenants/{id}/reactivate [post]
func (h *InternalHandler) Reactivate(c *gin.Context) {
	if h.Lifecycle == nil {
		h.fallback.Reactivate(c)
		return
	}
	tenantID, err := parseTenantID(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	var req reactivateRequest
	_ = c.ShouldBindJSON(&req) //nolint:errcheck // body is optional (reason only) — a missing/empty body just leaves req zero-valued
	result, err := h.Lifecycle.Reactivate(c.Request.Context(), idempotencyKey(c), tenantID, domain.ReactivationSourceOperator, req.Reason, nil)
	if err != nil {
		HandleError(c, err)
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
// @Failure      400 {object}  ErrorResponse  "invalid_request OR missing_idempotency_key"
// @Failure      404 {object}  ErrorResponse  "realm_not_found"
// @Failure      409 {object}  ErrorResponse  "operation_in_progress OR invalid_realm_state"
// @Failure      500 {object}  ErrorResponse
// @Security     SystemRole
// @Security     TenantID
// @Security     IdempotencyKey
// @Router       /tenants/{id}/expire-trial [post]
func (h *InternalHandler) ExpireTrial(c *gin.Context) {
	if h.Lifecycle == nil {
		h.fallback.ExpireTrial(c)
		return
	}
	tenantID, err := parseTenantID(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	result, err := h.Lifecycle.ExpireTrial(c.Request.Context(), idempotencyKey(c), tenantID, 30)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// ── RP-14: emergency operator suspension ─────────────────────────────────

type suspendRequest struct {
	Reason string `json:"reason" binding:"required"`
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
// @Failure      400      {object}  ErrorResponse  "invalid_request OR missing_idempotency_key"
// @Failure      403      {object}  ErrorResponse  "insufficient_role (operator role required)"
// @Failure      404      {object}  ErrorResponse  "realm_not_found"
// @Failure      409      {object}  ErrorResponse  "operation_in_progress OR invalid_realm_state"
// @Failure      500      {object}  ErrorResponse
// @Security     SystemRole
// @Security     TenantID
// @Security     IdempotencyKey
// @Router       /tenants/{id}/suspend [post]
func (h *InternalHandler) Suspend(c *gin.Context) {
	if h.Lifecycle == nil {
		h.fallback.Suspend(c)
		return
	}
	tenantID, err := parseTenantID(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	var req suspendRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrInvalidRequest, err.Error()))
		return
	}
	result, err := h.Lifecycle.Suspend(c.Request.Context(), idempotencyKey(c), tenantID, req.Reason)
	if err != nil {
		HandleError(c, err)
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
// @Failure      400               {object}  ErrorResponse  "invalid_request"
// @Failure      404               {object}  ErrorResponse  "realm_not_found"
// @Failure      500               {object}  ErrorResponse
// @Security     SystemRole
// @Security     TenantID
// @Router       /tenants/{id}/users/{keycloak_user_id} [delete]
func (h *InternalHandler) DeleteUser(c *gin.Context) {
	if h.Lifecycle == nil {
		h.fallback.DeleteUser(c)
		return
	}
	tenantID, err := parseTenantID(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	keycloakUserID, err := uuid.Parse(c.Param("keycloak_user_id"))
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrInvalidRequest, "invalid keycloak_user_id"))
		return
	}
	if err := h.Lifecycle.DeleteUser(c.Request.Context(), tenantID, keycloakUserID); err != nil {
		HandleError(c, err)
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
// @Failure      400               {object}  ErrorResponse  "invalid_request"
// @Failure      404               {object}  ErrorResponse  "realm_not_found OR user_not_found"
// @Failure      500               {object}  ErrorResponse
// @Security     SystemRole
// @Security     TenantID
// @Router       /tenants/{id}/users/{keycloak_user_id}/revoke-sessions [post]
func (h *InternalHandler) RevokeSessions(c *gin.Context) {
	if h.Lifecycle == nil {
		h.fallback.RevokeSessions(c)
		return
	}
	tenantID, err := parseTenantID(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	keycloakUserID, err := uuid.Parse(c.Param("keycloak_user_id"))
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrInvalidRequest, "invalid keycloak_user_id"))
		return
	}
	if err := h.Lifecycle.RevokeSessions(c.Request.Context(), tenantID, keycloakUserID); err != nil {
		HandleError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
