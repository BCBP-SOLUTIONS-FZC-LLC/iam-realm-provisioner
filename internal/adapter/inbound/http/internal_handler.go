package http

import (
	"net/http"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"
)

// InternalHandler implements Handlers (RP-1..RP-17) by delegating to the
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

// termsAcceptanceRequest is RP-1/RP-2's required terms_acceptance object:
// the tenant owner's terms-of-service acceptance, which this service
// records in Audit Log as legal.terms.accepted (compliance_7y). The BFF
// stamps accepted_at when it validates the signup form. Every rule is
// checked here, not by binding tags (validate tags only document the
// contract in Swagger), so each failure is a 400 INVALID_REQUEST
// (realmInvalidRequest): parseTermsAcceptance checks presence, version,
// accepted_at format, IP address and user agent; the service then checks
// the accepted_at window [now-24h, now+10m] after the idempotent-replay
// check, so a completed key still replays.
type termsAcceptanceRequest struct {
	Version    string `json:"version" validate:"required" example:"2026-09" minLength:"1" maxLength:"32"`
	AcceptedAt string `json:"accepted_at" validate:"required" example:"2026-10-22T09:12:04Z" format:"date-time"`
	IPAddress  string `json:"ip_address" validate:"required" example:"203.0.113.7"`
	UserAgent  string `json:"user_agent" validate:"required" example:"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_6)" maxLength:"512"`
}

// parseTermsAcceptance converts and shape-checks terms_acceptance; a nil
// result is always paired with an INVALID_REQUEST problem.
func parseTermsAcceptance(t *termsAcceptanceRequest) (*domain.TermsAcceptance, error) {
	if t == nil {
		return nil, problem.NewError(realmInvalidRequest, "terms_acceptance is required")
	}
	acceptedAt, err := time.Parse(time.RFC3339Nano, t.AcceptedAt)
	if err != nil {
		return nil, problem.NewError(realmInvalidRequest, "terms_acceptance.accepted_at must be an RFC 3339 timestamp")
	}
	out := &domain.TermsAcceptance{Version: t.Version, AcceptedAt: acceptedAt, IPAddress: t.IPAddress, UserAgent: t.UserAgent}
	if err := out.ValidateShape(); err != nil {
		return nil, domainErrToProblem(err)
	}
	return out, nil
}

type provisionTrialRequest struct {
	TenantID           uuid.UUID `json:"tenant_id" binding:"required"`
	TenantName         string    `json:"tenant_name" binding:"required,max=256"`
	TenantSlug         string    `json:"tenant_slug" binding:"required,max=63"`
	AdminEmail         string    `json:"admin_email" binding:"required,email,max=254"`
	AdminName          string    `json:"admin_name" binding:"required,max=256"`
	HashedPasswordSeed string    `json:"hashed_password_seed" binding:"max=512"`
	Plan               string    `json:"plan" binding:"required,max=32"`
	// TermsAcceptance is required: the owner's terms acceptance (A-4).
	TermsAcceptance *termsAcceptanceRequest `json:"terms_acceptance" validate:"required"`
}

// ProvisionTrial handles RP-1 (POST /tenants/provision-trial).
//
// @Summary      RP-1 — Provision trial realm
// @Description  Creates a shared trial realm for a new tenant and seeds the admin user. Keycloak-effect-first: realm is created in Keycloak before the local row is committed. Idempotent on Idempotency-Key reuse: replays the stored result without re-provisioning. Requires terms_acceptance (the owner's terms-of-service acceptance): recorded for Audit Log as legal.terms.accepted in the same transaction (one record per tenant, owner and version), never put on an event. accepted_at must lie within [now-24h, now+10m].
// @Tags         Provisioning
// @Accept       json
// @Produce      json
// @Param        request  body      provisionTrialRequest        true  "Trial provisioning payload"
// @Success      201      {object}  service.ProvisionTrialResult
// @Failure      400      {object}  ProblemResponse                "invalid_request OR missing_idempotency_key"
// @Failure      409      {object}  ProblemResponse                "operation_in_progress OR realm_already_dedicated OR tenant_offboarded"
// @Failure      500      {object}  ProblemResponse
// @Failure      503      {object}  ProblemResponse                "db_unavailable OR keycloak_unavailable OR token_service_unavailable"
// @Security     SystemRole
// @Security     IdempotencyKey
// @Router       /tenants/provision-trial [post]
func (h *InternalHandler) ProvisionTrial(c *gin.Context) {
	if h.Provisioning == nil {
		h.fallback.ProvisionTrial(c)
		return
	}
	var req provisionTrialRequest
	if err := gincommon.ShouldBindJSON(c, &req); err != nil {
		gincommon.AbortWithProblem(c, err)
		return
	}
	if !validTenantSlug(req.TenantSlug) {
		gincommon.AbortWithProblem(c, problem.NewError(realmInvalidRequest, "tenant_slug must not contain '/', whitespace, or control characters"))
		return
	}
	if !tenantMatchesCaller(c, req.TenantID) {
		return
	}
	if !validPlan(req.Plan) {
		gincommon.AbortWithProblem(c, problem.NewError(realmInvalidRequest, "plan must be one of: starter, pro, enterprise"))
		return
	}
	terms, err := parseTermsAcceptance(req.TermsAcceptance)
	if err != nil {
		gincommon.AbortWithProblem(c, err)
		return
	}
	result, err := h.Provisioning.ProvisionTrial(c.Request.Context(), idempotencyKey(c), service.ProvisionTrialRequest{
		TenantID: req.TenantID, TenantName: req.TenantName, TenantSlug: req.TenantSlug,
		AdminEmail: req.AdminEmail, AdminName: req.AdminName,
		HashedPasswordSeed: req.HashedPasswordSeed, Plan: req.Plan,
		Terms: terms,
	})
	if err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
		return
	}
	c.JSON(http.StatusCreated, result)
}

// ── RP-2 ─────────────────────────────────────────────────────────────────

type provisionDirectRequest struct {
	TenantID              uuid.UUID `json:"tenant_id" binding:"required"`
	TenantName            string    `json:"tenant_name" binding:"required,max=256"`
	TenantSlug            string    `json:"tenant_slug" binding:"required,max=63"`
	AdminEmail            string    `json:"admin_email" binding:"required,email,max=254"`
	AdminName             string    `json:"admin_name" binding:"required,max=256"`
	HashedPasswordSeed    string    `json:"hashed_password_seed" binding:"max=512"`
	Plan                  string    `json:"plan" binding:"required,max=32"`
	BillingSubscriptionID string    `json:"billing_subscription_id" binding:"max=128"`
	// TermsAcceptance is required: the owner's terms acceptance (A-4).
	TermsAcceptance *termsAcceptanceRequest `json:"terms_acceptance" validate:"required"`
}

// ProvisionDirect handles RP-2 (POST /tenants/provision-direct).
//
// @Summary      RP-2 — Provision dedicated paid realm
// @Description  Creates a dedicated realm for a direct-paid tenant using least-loaded shard placement. Keycloak-effect-first. Idempotent on Idempotency-Key reuse. Requires terms_acceptance, recorded for Audit Log as legal.terms.accepted exactly as in RP-1.
// @Tags         Provisioning
// @Accept       json
// @Produce      json
// @Param        request  body      provisionDirectRequest        true  "Direct provisioning payload"
// @Success      201      {object}  service.ProvisionDirectResult
// @Failure      400      {object}  ProblemResponse                 "invalid_request OR missing_idempotency_key"
// @Failure      409      {object}  ProblemResponse                 "operation_in_progress OR realm_already_dedicated OR tenant_offboarded"
// @Failure      500      {object}  ProblemResponse
// @Failure      503      {object}  ProblemResponse                "db_unavailable OR keycloak_unavailable OR token_service_unavailable"
// @Security     SystemRole
// @Security     IdempotencyKey
// @Router       /tenants/provision-direct [post]
func (h *InternalHandler) ProvisionDirect(c *gin.Context) {
	if h.Provisioning == nil {
		h.fallback.ProvisionDirect(c)
		return
	}
	var req provisionDirectRequest
	if err := gincommon.ShouldBindJSON(c, &req); err != nil {
		gincommon.AbortWithProblem(c, err)
		return
	}
	if !validTenantSlug(req.TenantSlug) {
		gincommon.AbortWithProblem(c, problem.NewError(realmInvalidRequest, "tenant_slug must not contain '/', whitespace, or control characters"))
		return
	}
	if !tenantMatchesCaller(c, req.TenantID) {
		return
	}
	if !validPlan(req.Plan) {
		gincommon.AbortWithProblem(c, problem.NewError(realmInvalidRequest, "plan must be one of: starter, pro, enterprise"))
		return
	}
	terms, err := parseTermsAcceptance(req.TermsAcceptance)
	if err != nil {
		gincommon.AbortWithProblem(c, err)
		return
	}
	result, err := h.Provisioning.ProvisionDirect(c.Request.Context(), idempotencyKey(c), service.ProvisionDirectRequest{
		TenantID: req.TenantID, TenantName: req.TenantName, TenantSlug: req.TenantSlug,
		AdminEmail: req.AdminEmail, AdminName: req.AdminName,
		HashedPasswordSeed: req.HashedPasswordSeed, Plan: req.Plan,
		BillingSubscriptionID: req.BillingSubscriptionID,
		Terms:                 terms,
	})
	if err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
		return
	}
	c.JSON(http.StatusCreated, result)
}

// ── RP-3 ─────────────────────────────────────────────────────────────────

type convertRequest struct {
	Plan                  string `json:"plan" binding:"required,max=32"`
	BillingSubscriptionID string `json:"billing_subscription_id" binding:"max=128"`
	PaymentMethodID       string `json:"payment_method_id" binding:"max=128"` // ignored — Billing/PCI domain, §5.2.1
}

// Convert handles RP-3 (POST /tenants/:id/convert).
//
// @Summary      RP-3 — Convert trial tenant to dedicated realm
// @Description  Migrates users from the shared trial realm to a new dedicated realm, preserving Keycloak sub UUIDs. TOTP credentials are copied; WebAuthn requires re-enrolment. Idempotent on Idempotency-Key reuse. Named callers: Billing (commercial-system principal, CD-19 allow-list — drives the conversion saga after Stripe confirms payment), Signup BFF (iam-system). Audit actor: the caller's principal is logged (commercial-system or iam-system).
// @Tags         Provisioning
// @Accept       json
// @Produce      json
// @Param        id       path      string          true  "Tenant UUID"  format(uuid)
// @Param        request  body      convertRequest  true  "Conversion payload"
// @Success      200      {object}  service.ConvertResult
// @Failure      400      {object}  ProblemResponse  "invalid_request OR missing_idempotency_key"
// @Failure      403      {object}  ProblemResponse  "insufficient_role (caller not in CD-19 allow-list)"
// @Failure      404      {object}  ProblemResponse  "tenant_realm_not_found"
// @Failure      409      {object}  ProblemResponse  "operation_in_progress OR realm_already_dedicated OR tenant_offboarded OR optimistic_lock_conflict OR credential_replay_expired"
// @Failure      502      {object}  ProblemResponse  "conversion_failed"
// @Failure      503      {object}  ProblemResponse                "db_unavailable OR keycloak_unavailable OR token_service_unavailable"
// @Failure      500      {object}  ProblemResponse
// @Security     SystemRole
// @Security     TenantID
// @Security     IdempotencyKey
// @Router       /tenants/{id}/convert [post]
func (h *InternalHandler) Convert(c *gin.Context) {
	if h.Provisioning == nil {
		h.fallback.Convert(c)
		return
	}
	tenantID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		gincommon.AbortWithProblem(c, problem.NewError(realmInvalidRequest, "invalid tenant id"))
		return
	}
	var req convertRequest
	if err := gincommon.ShouldBindJSON(c, &req); err != nil {
		gincommon.AbortWithProblem(c, err)
		return
	}
	result, err := h.Provisioning.Convert(c.Request.Context(), idempotencyKey(c), tenantID, service.ConvertRequest{
		Plan: req.Plan, BillingSubscriptionID: req.BillingSubscriptionID,
	})
	if err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
		return
	}
	c.JSON(http.StatusOK, result)
}

// ── RP-4 ─────────────────────────────────────────────────────────────────

type revertConversionRequest struct {
	Reason string `json:"reason"`
}

// RevertConversion handles RP-4 (POST /tenants/:id/revert-conversion).
//
// @Summary      RP-4 — Revert trial-to-dedicated conversion
// @Description  Rolls back a conversion within the 7-day revert window. Migrates users back to the shared trial realm and deletes the dedicated realm. Named callers: Billing (commercial-system principal, CD-19 allow-list — on fraud/chargeback dispute; which Stripe signal triggers the call is a Billing-side decision, CD-10). 7-day window is enforced for ALL callers including commercial-system (409 revert_window_expired). Audit actor: the caller's principal is logged. Body is optional; reason is informational only.
// @Tags         Provisioning
// @Accept       json
// @Produce      json
// @Param        id       path      string                  false  "Tenant UUID"  format(uuid)
// @Param        request  body      revertConversionRequest false  "Optional revert reason"
// @Success      200      {object}  service.RevertConversionResult
// @Failure      400      {object}  ProblemResponse  "invalid_request OR missing_idempotency_key"
// @Failure      404      {object}  ProblemResponse  "tenant_realm_not_found"
// @Failure      409      {object}  ProblemResponse  "operation_in_progress OR revert_window_expired"
// @Failure      500      {object}  ProblemResponse
// @Failure      503      {object}  ProblemResponse                "db_unavailable OR keycloak_unavailable OR token_service_unavailable"
// @Security     SystemRole
// @Security     TenantID
// @Security     IdempotencyKey
// @Router       /tenants/{id}/revert-conversion [post]
func (h *InternalHandler) RevertConversion(c *gin.Context) {
	if h.Provisioning == nil {
		h.fallback.RevertConversion(c)
		return
	}
	tenantID, perr := parseTenantID(c)
	if perr != nil {
		gincommon.AbortWithProblem(c, perr)
		return
	}
	var req revertConversionRequest
	_ = c.ShouldBindJSON(&req) //nolint:errcheck // body is optional (reason only) — a missing/empty body just leaves req zero-valued
	result, err := h.Provisioning.RevertConversion(c.Request.Context(), idempotencyKey(c), tenantID, truncateReason(req.Reason, 500), 7, 14)
	if err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
		return
	}
	c.JSON(http.StatusOK, result)
}

// ── RP-5 ─────────────────────────────────────────────────────────────────

type createInvitedUserRequest struct {
	Email           string   `json:"email" binding:"required,email,max=254"`
	FullName        string   `json:"full_name" binding:"required,max=256"`
	RequiredActions []string `json:"required_actions" binding:"omitempty,max=10,dive,max=64"`
}

// CreateInvitedUser handles RP-5 (POST /tenants/:id/users).
//
// @Summary      RP-5 — Create invited user in tenant realm
// @Description  Creates a Keycloak user in the tenant's realm for an accepted invitation. Sets required_actions (e.g. VERIFY_EMAIL, UPDATE_PASSWORD). Idempotent on Idempotency-Key reuse.
// @Tags         Users
// @Accept       json
// @Produce      json
// @Param        id       path      string                    true  "Tenant UUID"  format(uuid)
// @Param        request  body      createInvitedUserRequest  true  "Invited user payload"
// @Success      201      {object}  service.CreateInvitedUserResult
// @Failure      400      {object}  ProblemResponse  "invalid_request OR missing_idempotency_key"
// @Failure      404      {object}  ProblemResponse  "tenant_realm_not_found"
// @Failure      409      {object}  ProblemResponse  "operation_in_progress OR user_email_conflict"
// @Failure      500      {object}  ProblemResponse
// @Failure      503      {object}  ProblemResponse                "db_unavailable OR keycloak_unavailable OR token_service_unavailable"
// @Security     SystemRole
// @Security     TenantID
// @Security     IdempotencyKey
// @Router       /tenants/{id}/users [post]
func (h *InternalHandler) CreateInvitedUser(c *gin.Context) {
	if h.Provisioning == nil {
		h.fallback.CreateInvitedUser(c)
		return
	}
	tenantID, perr := parseTenantID(c)
	if perr != nil {
		gincommon.AbortWithProblem(c, perr)
		return
	}
	var req createInvitedUserRequest
	if err := gincommon.ShouldBindJSON(c, &req); err != nil {
		gincommon.AbortWithProblem(c, err)
		return
	}
	result, err := h.Provisioning.CreateInvitedUser(c.Request.Context(), idempotencyKey(c), tenantID, service.CreateInvitedUserRequest{
		Email: req.Email, FullName: req.FullName, RequiredActions: req.RequiredActions,
	})
	if err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
		return
	}
	c.JSON(http.StatusCreated, result)
}

// ── RP-9: MFA reset (fail-closed, idempotent) ────────────────────────────

// ResetMFA handles RP-9 (POST /tenants/:id/users/:keycloak_user_id/mfa-reset).
//
// @Summary      RP-9 — Reset user MFA credentials
// @Description  Deletes all TOTP and WebAuthn credentials for the specified Keycloak user and sets UPDATE_PASSWORD as a required action. Fail-closed: returns error if Keycloak is unreachable (no reconciler). Idempotent — re-deleting already-absent credentials succeeds.
// @Tags         MFA
// @Produce      json
// @Param        id                path  string  true  "Tenant UUID"        format(uuid)
// @Param        keycloak_user_id  path  string  true  "Keycloak user UUID"  format(uuid)
// @Success      204               "No Content"
// @Failure      400               {object}  ProblemResponse  "invalid_request"
// @Failure      404               {object}  ProblemResponse  "tenant_realm_not_found OR keycloak_user_not_found"
// @Failure      500               {object}  ProblemResponse
// @Failure      503               {object}  ProblemResponse  "db_unavailable OR keycloak_unavailable"
// @Security     SystemRole
// @Security     TenantID
// @Router       /tenants/{id}/users/{keycloak_user_id}/mfa-reset [post]
func (h *InternalHandler) ResetMFA(c *gin.Context) {
	if h.Mfa == nil {
		h.fallback.ResetMFA(c)
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
	if err := h.Mfa.ResetMFA(c.Request.Context(), tenantID, keycloakUserID); err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
		return
	}
	c.Status(http.StatusNoContent)
}

// ── RP-17: standalone rotate/revoke/break-glass key-cache refresh (§2.5) ──

// RefreshServiceAccountKeys handles RP-17
// (POST /tenants/:id/service-account/keys/refresh), empty body. The caller
// has already issued, rotated, or revoked a credential directly against
// Token Service's own TS-1/TS-2 and calls this so the tenant's realm
// actually notices — Keycloak does not self-refresh a jwks.url client's
// cached keys (EXT-6, verified empirically), so neither a newly-rotated key
// nor a revoked one takes effect at Keycloak without this call.
//
// @Summary      RP-17 — Refresh a service-account's Keycloak key cache
// @Description  Clears the tenant's realm-scoped Keycloak keys cache so a just-issued/rotated credential is recognized and a just-revoked one stops authenticating (EXT-6, §2.5) — Keycloak's jwks.url client-jwt authenticator does not self-refresh. No Keycloak-side apply happens at mint time (RP-1/RP-2/RP-3): Keycloak fetches the JWKS lazily on first use. No Idempotency-Key: clearing an already-fresh cache is a harmless no-op. Fail-closed on Keycloak-unreachable.
// @Tags         ServiceAccount
// @Produce      json
// @Param        id  path  string  true  "Tenant UUID"  format(uuid)
// @Success      204 "No Content"
// @Failure      404 {object}  ProblemResponse  "tenant_realm_not_found"
// @Failure      422 {object}  ProblemResponse  "service_account_not_provisioned"
// @Failure      502 {object}  ProblemResponse  "keycloak_unavailable"
// @Security     SystemRole
// @Security     TenantID
// @Router       /tenants/{id}/service-account/keys/refresh [post]
func (h *InternalHandler) RefreshServiceAccountKeys(c *gin.Context) {
	if h.Provisioning == nil {
		h.fallback.RefreshServiceAccountKeys(c)
		return
	}
	tenantID, perr := parseTenantID(c)
	if perr != nil {
		gincommon.AbortWithProblem(c, perr)
		return
	}
	if err := h.Provisioning.RefreshServiceAccountKeys(c.Request.Context(), tenantID); err != nil {
		gincommon.AbortWithProblem(c, domainErrToProblem(err))
		return
	}
	c.Status(http.StatusNoContent)
}

// ── helpers ────────────────────────────────────────────────────────────

func idempotencyKey(c *gin.Context) string { return c.GetHeader("Idempotency-Key") }

// truncateReason bounds an optional, informational-only free-text field
// (e.g. "reason" on RP-4/RP-10, whose handlers deliberately discard
// ShouldBindJSON's error so a missing/malformed body still succeeds with a
// zero-valued struct) to maxLen runes. A `binding:"max=N"` struct tag would
// be silently ineffective on those two handlers specifically, since the
// JSON decode step that populates the struct runs before validation and
// its result is never inspected — this runs after binding instead, so an
// oversized value is capped rather than flowing into the audit trail
// unbounded.
func truncateReason(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen])
}

// validTenantSlug rejects a slug that would corrupt the realm name this
// service derives from it ("tenant-"+slug, RP-2's dedicatedRealmName) once
// it becomes a path segment in Keycloak Admin API URLs
// (/admin/realms/<name>/...). Keycloak itself imposes no documented
// charset/length limit on a realm name — this is deliberately a safety
// check (no '/', no whitespace, no control characters), not an invented
// business-format rule.
func validTenantSlug(slug string) bool {
	for _, r := range slug {
		if r == '/' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

var validPlans = map[string]bool{"starter": true, "pro": true, "enterprise": true}

func validPlan(plan string) bool { return validPlans[plan] }

func parseTenantID(c *gin.Context) (id uuid.UUID, perr *problem.Error) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return uuid.Nil, problem.NewError(realmInvalidRequest, "invalid tenant id")
	}
	return id, nil
}
