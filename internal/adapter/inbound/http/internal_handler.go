package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

// InternalHandler implements Handlers (RP-1..RP-16) by delegating to the
// core services. Each service dependency is filled in as its phase lands;
// a nil service falls back to 501 not_implemented so the router can be
// wired incrementally without breaking already-shipped endpoints.
type InternalHandler struct {
	Provisioning *service.ProvisioningService
	Lifecycle    *service.LifecycleService
	Mfa          *service.MfaService
	Idp          *service.IdpService
	RealmConfig  *service.RealmConfigService
	fallback     StubHandlers
}

// NewInternalHandler wires the core services into an InternalHandler.
func NewInternalHandler(
	provisioning *service.ProvisioningService,
	lifecycle *service.LifecycleService,
	mfa *service.MfaService,
	idp *service.IdpService,
	realmConfig *service.RealmConfigService,
) *InternalHandler {
	return &InternalHandler{
		Provisioning: provisioning, Lifecycle: lifecycle, Mfa: mfa, Idp: idp, RealmConfig: realmConfig,
		fallback: NewStubHandlers(),
	}
}

var _ Handlers = (*InternalHandler)(nil)

// ── RP-1 ─────────────────────────────────────────────────────────────────

type provisionTrialRequest struct {
	TenantID           uuid.UUID `json:"tenant_id" binding:"required"`
	TenantName         string    `json:"tenant_name" binding:"required"`
	TenantSlug         string    `json:"tenant_slug" binding:"required"`
	AdminEmail         string    `json:"admin_email" binding:"required,email"`
	AdminName          string    `json:"admin_name" binding:"required"`
	HashedPasswordSeed string    `json:"hashed_password_seed"`
	Plan               string    `json:"plan" binding:"required"`
}

// ProvisionTrial handles RP-1 (POST /tenants/provision-trial).
func (h *InternalHandler) ProvisionTrial(c *gin.Context) {
	if h.Provisioning == nil {
		h.fallback.ProvisionTrial(c)
		return
	}
	var req provisionTrialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrInvalidRequest, err.Error()))
		return
	}
	result, err := h.Provisioning.ProvisionTrial(c.Request.Context(), idempotencyKey(c), service.ProvisionTrialRequest{
		TenantID: req.TenantID, TenantName: req.TenantName, TenantSlug: req.TenantSlug,
		AdminEmail: req.AdminEmail, AdminName: req.AdminName,
		HashedPasswordSeed: req.HashedPasswordSeed, Plan: req.Plan,
	})
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusCreated, result)
}

// ── RP-2 ─────────────────────────────────────────────────────────────────

type provisionDirectRequest struct {
	TenantID              uuid.UUID `json:"tenant_id" binding:"required"`
	TenantName            string    `json:"tenant_name" binding:"required"`
	TenantSlug            string    `json:"tenant_slug" binding:"required"`
	AdminEmail            string    `json:"admin_email" binding:"required,email"`
	AdminName             string    `json:"admin_name" binding:"required"`
	HashedPasswordSeed    string    `json:"hashed_password_seed"`
	Plan                  string    `json:"plan" binding:"required"`
	BillingSubscriptionID string    `json:"billing_subscription_id"`
}

// ProvisionDirect handles RP-2 (POST /tenants/provision-direct).
func (h *InternalHandler) ProvisionDirect(c *gin.Context) {
	if h.Provisioning == nil {
		h.fallback.ProvisionDirect(c)
		return
	}
	var req provisionDirectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrInvalidRequest, err.Error()))
		return
	}
	result, err := h.Provisioning.ProvisionDirect(c.Request.Context(), idempotencyKey(c), service.ProvisionDirectRequest{
		TenantID: req.TenantID, TenantName: req.TenantName, TenantSlug: req.TenantSlug,
		AdminEmail: req.AdminEmail, AdminName: req.AdminName,
		HashedPasswordSeed: req.HashedPasswordSeed, Plan: req.Plan,
		BillingSubscriptionID: req.BillingSubscriptionID,
	})
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// ── RP-3 ─────────────────────────────────────────────────────────────────

type convertRequest struct {
	Plan                  string `json:"plan" binding:"required"`
	BillingSubscriptionID string `json:"billing_subscription_id"`
	PaymentMethodID       string `json:"payment_method_id"` // ignored — Billing/PCI domain, §5.2.1
}

// Convert handles RP-3 (POST /tenants/:id/convert).
func (h *InternalHandler) Convert(c *gin.Context) {
	if h.Provisioning == nil {
		h.fallback.Convert(c)
		return
	}
	tenantID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrInvalidRequest, "invalid tenant id"))
		return
	}
	var req convertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrInvalidRequest, err.Error()))
		return
	}
	result, err := h.Provisioning.Convert(c.Request.Context(), idempotencyKey(c), tenantID, service.ConvertRequest{
		Plan: req.Plan, BillingSubscriptionID: req.BillingSubscriptionID,
	})
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// ── RP-4 ─────────────────────────────────────────────────────────────────

type revertConversionRequest struct {
	Reason string `json:"reason"`
}

// RevertConversion handles RP-4 (POST /tenants/:id/revert-conversion).
func (h *InternalHandler) RevertConversion(c *gin.Context) {
	if h.Provisioning == nil {
		h.fallback.RevertConversion(c)
		return
	}
	tenantID, err := parseTenantID(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	var req revertConversionRequest
	_ = c.ShouldBindJSON(&req) //nolint:errcheck // body is optional (reason only) — a missing/empty body just leaves req zero-valued
	result, err := h.Provisioning.RevertConversion(c.Request.Context(), idempotencyKey(c), tenantID, req.Reason, 7, 14)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// ── RP-5 ─────────────────────────────────────────────────────────────────

type createInvitedUserRequest struct {
	Email           string   `json:"email" binding:"required,email"`
	FullName        string   `json:"full_name" binding:"required"`
	RequiredActions []string `json:"required_actions"`
}

// CreateInvitedUser handles RP-5 (POST /tenants/:id/users).
func (h *InternalHandler) CreateInvitedUser(c *gin.Context) {
	if h.Provisioning == nil {
		h.fallback.CreateInvitedUser(c)
		return
	}
	tenantID, err := parseTenantID(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	var req createInvitedUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrInvalidRequest, err.Error()))
		return
	}
	result, err := h.Provisioning.CreateInvitedUser(c.Request.Context(), idempotencyKey(c), tenantID, service.CreateInvitedUserRequest{
		Email: req.Email, FullName: req.FullName, RequiredActions: req.RequiredActions,
	})
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusCreated, result)
}

// ── RP-9: MFA reset (fail-closed, idempotent) ────────────────────────────

// ResetMFA handles RP-9 (POST /tenants/:id/users/:keycloak_user_id/mfa-reset).
func (h *InternalHandler) ResetMFA(c *gin.Context) {
	if h.Mfa == nil {
		h.fallback.ResetMFA(c)
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
	if err := h.Mfa.ResetMFA(c.Request.Context(), tenantID, keycloakUserID); err != nil {
		HandleError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ── helpers ────────────────────────────────────────────────────────────

func idempotencyKey(c *gin.Context) string { return c.GetHeader("Idempotency-Key") }

func parseTenantID(c *gin.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return uuid.Nil, domain.NewError(domain.ErrInvalidRequest, "invalid tenant id")
	}
	return id, nil
}
