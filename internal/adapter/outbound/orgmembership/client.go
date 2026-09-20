// Package orgmembership implements port.OrgMembershipClient and
// port.SubscriptionLapseReader — this service's two outbound HTTP
// dependencies on Org & Membership: the I-8 residual-MFA-requirement check
// (consulted only on the TenantRoleRevoked drop path, §8.6, RP-1) and I-16's
// subscription-lapse poll (RP-C3's suspend-phase trigger, §16 RP-C3 of the
// RP↔O&M alignment review).
package orgmembership

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// mfaRequiringRoles mirrors O&M's tenant-role vocabulary (§8.6) — a user
// holding either still requires MFA regardless of department level.
var mfaRequiringRoles = map[string]bool{"tenant_owner": true, "tenant_admin": true}

// mfaRequiringLevels mirrors O&M's department-level vocabulary (§6.5).
var mfaRequiringLevels = map[string]bool{"reviewer": true, "approver": true}

// membershipProjection is the subset of O&M's I-8 MembershipProjection this
// client reads (§18.3 cross-check against iam-org-membership's
// AuthZService.MembershipProjection).
type membershipProjection struct {
	Roles       []string `json:"roles"`
	Departments []struct {
		Level string `json:"role_level"`
	} `json:"departments"`
}

// Client implements port.OrgMembershipClient.
type Client struct {
	baseURL string
	http    *http.Client
	log     port.Logger
}

var _ port.OrgMembershipClient = (*Client)(nil)
var _ port.SubscriptionLapseReader = (*Client)(nil)

// New constructs a Client calling O&M's internal API at baseURL.
func New(baseURL string, log port.Logger) *Client {
	return &Client{baseURL: baseURL, http: httpx.NewClient(5 * time.Second), log: log}
}

// HasResidualMFARequirement calls O&M's I-8
// GET /api/v1/internal/users/:id/memberships?tenant_id=<uuid> and reports
// whether userID still holds tenant_owner/tenant_admin or a
// reviewer/approver department mapping.
func (c *Client) HasResidualMFARequirement(ctx context.Context, tenantID, userID uuid.UUID) (bool, error) {
	url := fmt.Sprintf("%s/api/v1/internal/users/%s/memberships?tenant_id=%s", c.baseURL, userID, tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return false, err
	}
	req.Header.Set("x-user-id", domain.SystemActorID)
	req.Header.Set("x-tenant-id", tenantID.String())
	req.Header.Set("x-tenant-roles", "iam-system")

	resp, err := c.http.Do(req) //nolint:bodyclose // closed via the shared httpx.CloseBody helper below, which the linter's cross-package analysis doesn't trace
	if err != nil {
		return false, domain.NewError(domain.ErrOrgMembershipUnavailable, "org membership unavailable").WithDetails(map[string]any{"error": err.Error()})
	}
	defer httpx.CloseBody(c.log, resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		// No active membership at all — no residual requirement.
		return false, nil
	}
	if resp.StatusCode >= 500 {
		return false, domain.NewError(domain.ErrOrgMembershipUnavailable, "org membership unavailable").WithDetails(map[string]any{"status": resp.StatusCode})
	}
	if resp.StatusCode >= 300 {
		return false, fmt.Errorf("orgmembership: unexpected status %d from I-8", resp.StatusCode)
	}

	var proj membershipProjection
	if err := json.NewDecoder(resp.Body).Decode(&proj); err != nil {
		return false, fmt.Errorf("orgmembership: decode I-8 response: %w", err)
	}
	for _, r := range proj.Roles {
		if mfaRequiringRoles[r] {
			return true, nil
		}
	}
	for _, d := range proj.Departments {
		if mfaRequiringLevels[d.Level] {
			return true, nil
		}
	}
	return false, nil
}

// subscriptionLapseItem mirrors O&M's I-16 SubscriptionLapseItem DTO
// (its dto.go): a dedicated- or shared-realm tenant already cancelled past
// O&M's own SUBSCRIPTION_GRACE_DAYS.
type subscriptionLapseItem struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	RealmID     string    `json:"realm_id"`
	RealmType   string    `json:"realm_type"`
	CancelledAt time.Time `json:"cancelled_at"`
}

type subscriptionLapseListResponse struct {
	Tenants []subscriptionLapseItem `json:"tenants"`
}

// ListLapsedSubscriptions calls O&M's I-16
// GET /api/v1/internal/subscription-lapses (§16 RP-C3) — deliberately
// cross-tenant (no :id, no x-tenant-id header), so unlike
// HasResidualMFARequirement it carries only the system-principal identity
// headers. Self-idempotent on O&M's side: a tenant this service suspends
// drops off the next poll.
func (c *Client) ListLapsedSubscriptions(ctx context.Context) ([]port.LapsedSubscription, error) {
	url := c.baseURL + "/api/v1/internal/subscription-lapses"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-user-id", domain.SystemActorID)
	req.Header.Set("x-tenant-roles", "iam-system")
	// O&M's shared gincommon auth middleware (RequireAuth) requires
	// x-tenant-id to be present on every /api/v1 route, including this one
	// — there is no way to opt a single route out of that platform-wide
	// contract. This call is genuinely cross-tenant (O&M's I-16 handler
	// queries its own BYPASSRLS sysPool unconditionally and never reads
	// this value), so a sentinel nil UUID satisfies the header-presence
	// check without claiming any real tenant scope.
	req.Header.Set("x-tenant-id", uuid.Nil.String())

	resp, err := c.http.Do(req) //nolint:bodyclose // closed via the shared httpx.CloseBody helper below, which the linter's cross-package analysis doesn't trace
	if err != nil {
		return nil, domain.NewError(domain.ErrOrgMembershipUnavailable, "org membership unavailable").WithDetails(map[string]any{"error": err.Error()})
	}
	defer httpx.CloseBody(c.log, resp.Body)

	if resp.StatusCode >= 500 {
		return nil, domain.NewError(domain.ErrOrgMembershipUnavailable, "org membership unavailable").WithDetails(map[string]any{"status": resp.StatusCode})
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("orgmembership: unexpected status %d from I-16", resp.StatusCode)
	}

	var body subscriptionLapseListResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("orgmembership: decode I-16 response: %w", err)
	}
	out := make([]port.LapsedSubscription, len(body.Tenants))
	for i, t := range body.Tenants {
		out[i] = port.LapsedSubscription{
			TenantID:    t.TenantID,
			RealmID:     t.RealmID,
			RealmType:   t.RealmType,
			CancelledAt: t.CancelledAt,
		}
	}
	return out, nil
}
