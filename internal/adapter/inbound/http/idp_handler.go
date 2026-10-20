package http

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"
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
// @Failure      400      {object}  ProblemResponse  "INVALID_REQUEST OR MISSING_IDEMPOTENCY_KEY OR INVALID_IDP_PROTOCOL OR IDP_REGION_PORTABILITY_FAILED"
// @Failure      404      {object}  ProblemResponse  "TENANT_REALM_NOT_FOUND"
// @Failure      409      {object}  ProblemResponse  "OPERATION_IN_PROGRESS OR IDP_REQUIRES_DEDICATED_REALM OR OPTIMISTIC_LOCK_CONFLICT OR TENANT_OFFBOARDED"
// @Failure      422      {object}  ProblemResponse  "IDP_WARNINGS_UNACKNOWLEDGED"
// @Failure      500      {object}  ProblemResponse
// @Security     SystemRole
// @Security     TenantID
// @Security     IdempotencyKey
// @Router       /tenants/{id}/idp [put]
func (h *InternalHandler) ConfigureIdp(c *gin.Context) {
	if h.Idp == nil {
		h.fallback.ConfigureIdp(c)
		return
	}
	tenantID, perr := parseTenantID(c)
	if perr != nil {
		gincommon.AbortWithProblem(c, perr)
		return
	}
	var req configureIdpRequest
	if err := gincommon.ShouldBindJSON(c, &req); err != nil {
		gincommon.AbortWithProblem(c, err)
		return
	}
	protocol := domain.IdpProtocol(req.Protocol)
	if protocol != domain.IdpProtocolSAML && protocol != domain.IdpProtocolOIDC {
		gincommon.AbortWithProblem(c, problem.NewError(realmInvalidIdpProtocol, "protocol must be saml or oidc"))
		return
	}
	result, err := h.Idp.ConfigureIdp(c.Request.Context(), idempotencyKey(c), tenantID, service.ConfigureIdpRequest{
		IdpAlias: req.IdpAlias, Protocol: protocol, DisplayName: req.DisplayName,
		Config: req.Config, Secret: req.Secret, Acknowledgements: req.Acknowledgements,
	})
	if err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
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
// @Failure      400    {object}  ProblemResponse  "INVALID_REQUEST"
// @Failure      404    {object}  ProblemResponse  "TENANT_REALM_NOT_FOUND OR IDP_CONFIG_NOT_FOUND"
// @Failure      500    {object}  ProblemResponse
// @Security     SystemRole
// @Security     TenantID
// @Router       /tenants/{id}/idp/{alias} [delete]
func (h *InternalHandler) RemoveIdp(c *gin.Context) {
	if h.Idp == nil {
		h.fallback.RemoveIdp(c)
		return
	}
	tenantID, perr := parseTenantID(c)
	if perr != nil {
		gincommon.AbortWithProblem(c, perr)
		return
	}
	alias := c.Param("alias")
	if err := h.Idp.RemoveIdp(c.Request.Context(), tenantID, alias); err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
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
// @Failure      403  {object}  ProblemResponse  "INSUFFICIENT_ROLE (operator role required)"
// @Failure      500  {object}  ProblemResponse
// @Security     SystemRole
// @Router       /audit/idp-region-portability [post]
func (h *InternalHandler) PortabilityAudit(c *gin.Context) {
	if h.Idp == nil {
		h.fallback.PortabilityAudit(c)
		return
	}
	result, err := h.Idp.PortabilityAudit(c.Request.Context())
	if err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
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
// @Failure      400      {object}  ProblemResponse  "INVALID_REQUEST OR UNSUPPORTED_REALM_SETTING"
// @Failure      404      {object}  ProblemResponse  "TENANT_REALM_NOT_FOUND"
// @Failure      500      {object}  ProblemResponse
// @Security     SystemRole
// @Security     TenantID
// @Router       /tenants/{id}/realm-config [patch]
func (h *InternalHandler) ApplyRealmConfig(c *gin.Context) {
	if h.RealmConfig == nil {
		h.fallback.ApplyRealmConfig(c)
		return
	}
	tenantID, perr := parseTenantID(c)
	if perr != nil {
		gincommon.AbortWithProblem(c, perr)
		return
	}
	var req applyRealmConfigRequest
	if err := gincommon.ShouldBindJSON(c, &req); err != nil {
		gincommon.AbortWithProblem(c, err)
		return
	}
	result, err := h.RealmConfig.ApplyRealmConfig(c.Request.Context(), tenantID, req.SettingKey, req.DesiredValue)
	if err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
		return
	}
	status := http.StatusOK
	if !result.Applied {
		status = http.StatusAccepted // RP-INV-6: transient Keycloak failure -> 202, RP-C4 reconciles
	}
	c.JSON(status, result)
}
