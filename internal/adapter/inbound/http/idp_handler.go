package http

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

// ── RP-6 ─────────────────────────────────────────────────────────────────

type configureIdpRequest struct {
	IdpAlias         string         `json:"idp_alias" binding:"required"`
	Protocol         string         `json:"protocol" binding:"required"`
	DisplayName      string         `json:"display_name" binding:"required"`
	Config           map[string]any `json:"config"`
	Secret           string         `json:"secret"`
	Acknowledgements []string       `json:"ack"`
}

// ConfigureIdp handles RP-6 (PUT /tenants/:id/idp).
func (h *InternalHandler) ConfigureIdp(c *gin.Context) {
	if h.Idp == nil {
		h.fallback.ConfigureIdp(c)
		return
	}
	tenantID, err := parseTenantID(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	var req configureIdpRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrInvalidRequest, err.Error()))
		return
	}
	protocol := domain.IdpProtocol(req.Protocol)
	if protocol != domain.IdpProtocolSAML && protocol != domain.IdpProtocolOIDC {
		HandleError(c, domain.NewError(domain.ErrInvalidIdpProtocol, "protocol must be saml or oidc"))
		return
	}
	result, err := h.Idp.ConfigureIdp(c.Request.Context(), idempotencyKey(c), tenantID, service.ConfigureIdpRequest{
		IdpAlias: req.IdpAlias, Protocol: protocol, DisplayName: req.DisplayName,
		Config: req.Config, Secret: req.Secret, Acknowledgements: req.Acknowledgements,
	})
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// ── RP-7 ─────────────────────────────────────────────────────────────────

// RemoveIdp handles RP-7 (DELETE /tenants/:id/idp/:alias).
func (h *InternalHandler) RemoveIdp(c *gin.Context) {
	if h.Idp == nil {
		h.fallback.RemoveIdp(c)
		return
	}
	tenantID, err := parseTenantID(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	alias := c.Param("alias")
	if err := h.Idp.RemoveIdp(c.Request.Context(), tenantID, alias); err != nil {
		HandleError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ── RP-13 (operator-only, whole-estate, not tenant-scoped) ───────────────

// PortabilityAudit handles RP-13 (POST /audit/idp-region-portability).
func (h *InternalHandler) PortabilityAudit(c *gin.Context) {
	if h.Idp == nil {
		h.fallback.PortabilityAudit(c)
		return
	}
	result, err := h.Idp.PortabilityAudit(c.Request.Context())
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// ── RP-8 ─────────────────────────────────────────────────────────────────

type applyRealmConfigRequest struct {
	SettingKey   string `json:"setting_key" binding:"required"`
	DesiredValue any    `json:"desired_value"`
}

// ApplyRealmConfig handles RP-8 (PATCH /tenants/:id/realm-config).
func (h *InternalHandler) ApplyRealmConfig(c *gin.Context) {
	if h.RealmConfig == nil {
		h.fallback.ApplyRealmConfig(c)
		return
	}
	tenantID, err := parseTenantID(c)
	if err != nil {
		HandleError(c, err)
		return
	}
	var req applyRealmConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrInvalidRequest, err.Error()))
		return
	}
	result, err := h.RealmConfig.ApplyRealmConfig(c.Request.Context(), tenantID, req.SettingKey, req.DesiredValue)
	if err != nil {
		HandleError(c, err)
		return
	}
	status := http.StatusOK
	if !result.Applied {
		status = http.StatusAccepted // RP-INV-6: transient Keycloak failure -> 202, RP-C4 reconciles
	}
	c.JSON(status, result)
}
