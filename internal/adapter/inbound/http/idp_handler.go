package http

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

// ── RP-6 ─────────────────────────────────────────────────────────────────

type configureIdpRequest struct {
	IdpAlias    string         `json:"idp_alias" binding:"required,max=128"`
	Protocol    string         `json:"protocol" binding:"required,max=16"`
	DisplayName string         `json:"display_name" binding:"required,max=256"`
	Config      map[string]any `json:"config"`
	// Secret can be a SAML signing certificate or RSA private key in PEM
	// form (legitimately several KB) — bounded generously, well below the
	// global request-body cap, not to the size of a typical OIDC client secret.
	Secret           string   `json:"secret" binding:"max=8192"`
	Acknowledgements []string `json:"ack" binding:"omitempty,max=20,dive,max=64"`
}

// ConfigureIdp handles RP-6 (PUT /tenants/:id/idp).
//
// @Summary      RP-6 — Configure tenant IdP (SAML/OIDC)
// @Description  Creates or replaces the tenant's identity provider in Keycloak. The IdP client secret is stored exclusively in OpenBao — never in Postgres. Runs the §11.6 region-portability validation gate before writing: CRITICAL failures return 409; unacknowledged WARN checks return 422 until the caller supplies the required ack codes in the `ack` field. Idempotent on Idempotency-Key reuse.
// @Tags         IdP
// @Accept       json
// @Produce      json
// @Param        id       path      string               true  "Tenant UUID"  format(uuid)
// @Param        request  body      configureIdpRequest  true  "IdP configuration payload"
// @Success      200      {object}  service.ConfigureIdpResult
// @Failure      400      {object}  ErrorResponse  "invalid_request OR missing_idempotency_key OR invalid_idp_protocol"
// @Failure      404      {object}  ErrorResponse  "realm_not_found"
// @Failure      409      {object}  ErrorResponse  "operation_in_progress OR idp_region_portability_failed"
// @Failure      422      {object}  ErrorResponse  "idp_warnings_unacknowledged"
// @Failure      500      {object}  ErrorResponse
// @Security     SystemRole
// @Security     TenantID
// @Security     IdempotencyKey
// @Router       /tenants/{id}/idp [put]
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
//
// @Summary      RP-7 — Remove tenant IdP
// @Description  Deletes the specified identity provider from the tenant's Keycloak realm and removes the client secret from OpenBao. Idempotent: deleting an already-absent IdP returns 204.
// @Tags         IdP
// @Produce      json
// @Param        id     path  string  true  "Tenant UUID"  format(uuid)
// @Param        alias  path  string  true  "IdP alias"
// @Success      204    "No Content"
// @Failure      400    {object}  ErrorResponse  "invalid_request"
// @Failure      404    {object}  ErrorResponse  "realm_not_found"
// @Failure      500    {object}  ErrorResponse
// @Security     SystemRole
// @Security     TenantID
// @Router       /tenants/{id}/idp/{alias} [delete]
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
//
// @Summary      RP-13 — Whole-estate IdP region-portability audit (operator-only)
// @Description  Operator-only. Runs the §11.6 Region-Agnostic IdP Configuration Checklist across every tenant with an active IdP configuration and returns a per-tenant validation report. Not tenant-scoped — scans the full estate. Used for pre-migration and compliance audits.
// @Tags         IdP
// @Produce      json
// @Success      200  {object}  service.PortabilityAuditResult
// @Failure      403  {object}  ErrorResponse  "insufficient_role (operator role required)"
// @Failure      500  {object}  ErrorResponse
// @Security     SystemRole
// @Router       /audit/idp-region-portability [post]
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
	SettingKey   string `json:"setting_key" binding:"required,max=128"`
	DesiredValue any    `json:"desired_value"`
}

// ApplyRealmConfig handles RP-8 (PATCH /tenants/:id/realm-config).
//
// @Summary      RP-8 — Apply realm configuration setting
// @Description  Applies a single realm configuration key/value to the tenant's Keycloak realm. Local-intent-first (RP-INV-6): if Keycloak is transiently unreachable the desired value is persisted as `pending` in realm_config_reconciliations and the caller receives 202 Accepted with `applied:false, reconciling:true`. RP-C4 converges it later. A 200 with `applied:true` means the change is live in Keycloak.
// @Tags         RealmConfig
// @Accept       json
// @Produce      json
// @Param        id       path      string                   true  "Tenant UUID"  format(uuid)
// @Param        request  body      applyRealmConfigRequest  true  "Realm config setting"
// @Success      200      {object}  service.ApplyRealmConfigResult  "Applied immediately"
// @Success      202      {object}  service.ApplyRealmConfigResult  "Queued for reconciliation (applied:false)"
// @Failure      400      {object}  ErrorResponse  "invalid_request"
// @Failure      404      {object}  ErrorResponse  "realm_not_found"
// @Failure      500      {object}  ErrorResponse
// @Security     SystemRole
// @Security     TenantID
// @Router       /tenants/{id}/realm-config [patch]
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
