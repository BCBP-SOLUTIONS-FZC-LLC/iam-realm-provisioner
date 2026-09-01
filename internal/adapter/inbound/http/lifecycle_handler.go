package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// ── RP-11: GET realm (read-only) ─────────────────────────────────────────

// GetRealm handles RP-11 (GET /tenants/:id/realm).
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
