// Package orgmembership implements port.OrgMembershipClient — the single
// outbound HTTP dependency this service has on Org & Membership: the I-8
// residual-MFA-requirement check, consulted only on the TenantRoleRevoked
// drop path (§8.6, RP-1).
package orgmembership

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// systemPrincipalUserID is the reserved iam-system caller identity RP→O&M
// calls use (§18.3: x-user-id=…00a1).
const systemPrincipalUserID = "00000000-0000-0000-0000-0000000000a1"

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
		Level string `json:"level"`
	} `json:"departments"`
}

// Client implements port.OrgMembershipClient.
type Client struct {
	baseURL string
	http    *http.Client
	log     port.Logger
}

var _ port.OrgMembershipClient = (*Client)(nil)

// New constructs a Client calling O&M's internal API at baseURL.
func New(baseURL string, log port.Logger) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: 5 * time.Second}, log: log}
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
	req.Header.Set("x-user-id", systemPrincipalUserID)
	req.Header.Set("x-tenant-id", tenantID.String())
	req.Header.Set("x-tenant-roles", "iam-system")

	resp, err := c.http.Do(req)
	if err != nil {
		return false, domain.NewError(domain.ErrOrgMembershipUnavailable, "org membership unavailable").WithDetails(map[string]any{"error": err.Error()})
	}
	defer closeBody(c.log, resp.Body)

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

// closeBody closes body, logging (not propagating) a failure — by the time
// this runs the response has already been fully handled, so there is no
// remaining action a Close error could change.
func closeBody(log port.Logger, body io.Closer) {
	if err := body.Close(); err != nil {
		log.Warn("orgmembership: close response body failed", map[string]any{"error": err.Error()})
	}
}
