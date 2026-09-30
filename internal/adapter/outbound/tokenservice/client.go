// Package tokenservice implements port.TokenServiceClient — this
// service's outbound HTTP dependency on the Token Service's
// credential-lifecycle API (TS-1/TS-3/TS-4, §2.5, RP-17/RP-18). RP mints
// the Keycloak client (client-jwt/JWKS auth, EXT-6) and clears its cached
// JWKS keys on rotate/revoke; Token Service generates and custodies the
// credential material and serves its public half directly to Keycloak as
// a JWKS — it never touches Keycloak itself, and RP never applies a secret
// to it (RP-INV-1).
package tokenservice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// Client implements port.TokenServiceClient.
type Client struct {
	baseURL string
	http    *http.Client
	log     port.Logger
}

var _ port.TokenServiceClient = (*Client)(nil)

// New constructs a Client calling the Token Service's internal API at
// baseURL.
func New(baseURL string, log port.Logger) *Client {
	return &Client{baseURL: baseURL, http: httpx.NewClient(5 * time.Second), log: log}
}

// JWKSURLFor returns the JWKS URL for tenantID's platform-automation
// principal (EXT-6, §2.5) — pure string construction from this client's own
// baseURL, no network call.
func (c *Client) JWKSURLFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("%s/api/v1/internal/tenants/%s/service-accounts/platform-automation/jwks.json", c.baseURL, tenantID)
}

func (c *Client) newRequest(ctx context.Context, method, url string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("tokenservice: encode request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) do(req *http.Request, tenantID uuid.UUID) (*http.Response, error) {
	req.Header.Set("x-user-id", domain.SystemActorID)
	req.Header.Set("x-tenant-id", tenantID.String())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, domain.NewError(domain.ErrTokenServiceUnavailable, "token service unavailable").WithDetails(map[string]any{"error": err.Error()})
	}
	return resp, nil
}

// classifyStatus maps a non-2xx TS response status to the shared error
// every RegisterPrincipal/IssueOrRotateCredential/ReadPrincipal apply
// identically: >=500 as the retryable ErrTokenServiceUnavailable, 404 as
// ErrServiceAccountNotProvisioned (principal not yet registered), 400 as a
// typed invalid-request domain error surfacing the TS error code, any other
// >=300 as a plain unexpected-status error. Returns nil for a 2xx status.
// Callers that need to special-case a specific status first
// (IssueOrRotateCredential's 409) check that before falling through here.
func classifyStatus(status int, endpoint string, body io.Reader) error {
	if status >= 500 {
		return domain.NewError(domain.ErrTokenServiceUnavailable, "token service unavailable").WithDetails(map[string]any{"status": status})
	}
	if status == http.StatusNotFound {
		return domain.NewError(domain.ErrServiceAccountNotProvisioned, "token service: principal not found").
			WithDetails(map[string]any{"endpoint": endpoint})
	}
	if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
		var errBody errorResponseBody
		if decErr := json.NewDecoder(io.LimitReader(body, 4096)).Decode(&errBody); decErr != nil {
			errBody = errorResponseBody{}
		}
		msg := fmt.Sprintf("tokenservice: %s rejected request (status %d)", endpoint, status)
		if errBody.Error != "" {
			msg = fmt.Sprintf("tokenservice: %s rejected request — %s", endpoint, errBody.Error)
		}
		return fmt.Errorf("%s", msg)
	}
	if status >= 300 {
		return fmt.Errorf("tokenservice: unexpected status %d from %s", status, endpoint)
	}
	return nil
}

type registerPrincipalRequestBody struct {
	PrincipalSub     string `json:"principal_sub"`
	KeycloakClientID string `json:"keycloak_client_id"`
}

type principalResponseBody struct {
	PrincipalID   uuid.UUID `json:"principal_id"`
	TenantID      uuid.UUID `json:"tenant_id"`
	PrincipalType string    `json:"principal_type"`
	Status        string    `json:"status"`
	RecordVersion int       `json:"record_version"`
}

// RegisterPrincipal calls TS-4
// POST /api/v1/internal/tenants/:id/service-accounts.
func (c *Client) RegisterPrincipal(ctx context.Context, tenantID uuid.UUID, req port.RegisterPrincipalRequest) (*port.RegisterPrincipalResult, error) {
	url := fmt.Sprintf("%s/api/v1/internal/tenants/%s/service-accounts", c.baseURL, tenantID)
	httpReq, err := c.newRequest(ctx, http.MethodPost, url, registerPrincipalRequestBody{
		PrincipalSub: req.PrincipalSub.String(), KeycloakClientID: req.KeycloakClientID,
	})
	if err != nil {
		return nil, err
	}

	resp, err := c.do(httpReq, tenantID) //nolint:bodyclose // closed via the shared httpx.CloseBody helper below, which the linter's cross-package analysis doesn't trace
	if err != nil {
		return nil, err
	}
	defer httpx.CloseBody(c.log, resp.Body)

	if err := classifyStatus(resp.StatusCode, "TS-4", resp.Body); err != nil {
		return nil, err
	}

	var body principalResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("tokenservice: decode TS-4 response: %w", err)
	}
	return &port.RegisterPrincipalResult{
		PrincipalID: body.PrincipalID, Created: resp.StatusCode == http.StatusCreated, RecordVersion: body.RecordVersion,
	}, nil
}

type issueOrRotateRequestBody struct {
	RotationID     string `json:"rotation_id"`
	OverlapSeconds *int   `json:"overlap_seconds,omitempty"`
}

// errorResponseBody is the subset of platform-gincommon's flat
// ErrorResponse wire shape this client needs to disambiguate a 409 — see
// the Token Service LLD §17 (`rotation_in_flight` vs `optimistic_lock_conflict`
// vs the additive `credential_replay_revoked`, all 409).
type errorResponseBody struct {
	Error string `json:"error"`
}

type issueOrRotateResponseBody struct {
	Version        int     `json:"version"`
	Secret         string  `json:"secret"`
	OpenBaoPath    string  `json:"openbao_path"`
	ExpiresPriorAt *string `json:"expires_prior_at,omitempty"`
	RecordVersion  int     `json:"record_version"`
}

// IssueOrRotateCredential calls TS-1
// POST /api/v1/internal/tenants/:id/service-accounts/:principal_id/credentials.
// A 409 is surfaced as one of the three §17 causes when TS-1's response body
// names it explicitly — domain.ErrOptimisticLockConflict (record_version
// mismatch), domain.ErrCredentialReplayRevoked (TS-D13: a replayed
// rotation_id whose credential has since been revoked), or
// domain.ErrServiceAccountRotationInFlight (concurrent rotation on the same
// principal). An empty/unparseable 409 body falls back to
// ErrServiceAccountRotationInFlight as the safe retryable default.
func (c *Client) IssueOrRotateCredential(ctx context.Context, tenantID, principalID uuid.UUID, req port.IssueCredentialRequest) (*port.IssueCredentialResult, error) {
	url := fmt.Sprintf("%s/api/v1/internal/tenants/%s/service-accounts/%s/credentials", c.baseURL, tenantID, principalID)
	httpReq, err := c.newRequest(ctx, http.MethodPost, url, issueOrRotateRequestBody{
		RotationID: req.RotationID.String(), OverlapSeconds: req.OverlapSeconds,
	})
	if err != nil {
		return nil, err
	}

	resp, err := c.do(httpReq, tenantID) //nolint:bodyclose // closed via the shared httpx.CloseBody helper below, which the linter's cross-package analysis doesn't trace
	if err != nil {
		return nil, err
	}
	defer httpx.CloseBody(c.log, resp.Body)

	if resp.StatusCode == http.StatusConflict {
		var errBody errorResponseBody
		_ = json.NewDecoder(resp.Body).Decode(&errBody) //nolint:errcheck // best-effort; falls back to the safe default below on a malformed/empty body
		code := domain.ErrServiceAccountRotationInFlight
		switch errBody.Error {
		case string(domain.ErrOptimisticLockConflict):
			code = domain.ErrOptimisticLockConflict
		case string(domain.ErrCredentialReplayRevoked):
			code = domain.ErrCredentialReplayRevoked
		}
		return nil, domain.NewError(code, "token service credential issue/rotate conflict").
			WithDetails(map[string]any{"tenant_id": tenantID, "principal_id": principalID, "upstream_error": errBody.Error})
	}
	if err := classifyStatus(resp.StatusCode, "TS-1", resp.Body); err != nil {
		return nil, err
	}

	var body issueOrRotateResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("tokenservice: decode TS-1 response: %w", err)
	}
	var expiresPriorAt *time.Time
	if body.ExpiresPriorAt != nil {
		t, err := time.Parse(time.RFC3339, *body.ExpiresPriorAt)
		if err != nil {
			return nil, fmt.Errorf("tokenservice: decode TS-1 expires_prior_at: %w", err)
		}
		expiresPriorAt = &t
	}
	return &port.IssueCredentialResult{
		Version: body.Version, Secret: body.Secret, OpenBaoPath: body.OpenBaoPath,
		ExpiresPriorAt: expiresPriorAt, RecordVersion: body.RecordVersion, Replayed: resp.StatusCode == http.StatusOK,
	}, nil
}

type credentialSummaryBody struct {
	Version             int     `json:"version"`
	Status              string  `json:"status"`
	OpenBaoPath         string  `json:"openbao_path"`
	IssuedAt            string  `json:"issued_at"`
	ExpiresAt           *string `json:"expires_at,omitempty"`
	RotationCadenceDays *int    `json:"rotation_cadence_days,omitempty"`
	NextRotationAt      *string `json:"next_rotation_at,omitempty"`
}

type readPrincipalResponseBody struct {
	PrincipalID      uuid.UUID               `json:"principal_id"`
	TenantID         uuid.UUID               `json:"tenant_id"`
	KeycloakClientID string                  `json:"keycloak_client_id"`
	PrincipalType    string                  `json:"principal_type"`
	Status           string                  `json:"status"`
	RecordVersion    int                     `json:"record_version"`
	Credentials      []credentialSummaryBody `json:"credentials"`
}

// ReadPrincipal calls TS-3
// GET /api/v1/internal/tenants/:id/service-accounts/:principal_id (RP-18)
// — metadata only, never a secret.
func (c *Client) ReadPrincipal(ctx context.Context, tenantID, principalID uuid.UUID) (*port.ReadPrincipalResult, error) {
	url := fmt.Sprintf("%s/api/v1/internal/tenants/%s/service-accounts/%s", c.baseURL, tenantID, principalID)
	httpReq, err := c.newRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.do(httpReq, tenantID) //nolint:bodyclose // closed via the shared httpx.CloseBody helper below, which the linter's cross-package analysis doesn't trace
	if err != nil {
		return nil, err
	}
	defer httpx.CloseBody(c.log, resp.Body)

	if err := classifyStatus(resp.StatusCode, "TS-3", resp.Body); err != nil {
		return nil, err
	}

	var body readPrincipalResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("tokenservice: decode TS-3 response: %w", err)
	}
	creds := make([]port.CredentialSummary, 0, len(body.Credentials))
	for _, cr := range body.Credentials {
		issuedAt, err := time.Parse(time.RFC3339, cr.IssuedAt)
		if err != nil {
			return nil, fmt.Errorf("tokenservice: decode TS-3 issued_at: %w", err)
		}
		var expiresAt *time.Time
		if cr.ExpiresAt != nil {
			t, err := time.Parse(time.RFC3339, *cr.ExpiresAt)
			if err != nil {
				return nil, fmt.Errorf("tokenservice: decode TS-3 expires_at: %w", err)
			}
			expiresAt = &t
		}
		var nextRotationAt *time.Time
		if cr.NextRotationAt != nil {
			t, err := time.Parse(time.RFC3339, *cr.NextRotationAt)
			if err != nil {
				return nil, fmt.Errorf("tokenservice: decode TS-3 next_rotation_at: %w", err)
			}
			nextRotationAt = &t
		}
		creds = append(creds, port.CredentialSummary{
			Version: cr.Version, Status: cr.Status, OpenBaoPath: cr.OpenBaoPath,
			IssuedAt: issuedAt, ExpiresAt: expiresAt,
			RotationCadenceDays: cr.RotationCadenceDays, NextRotationAt: nextRotationAt,
		})
	}
	return &port.ReadPrincipalResult{
		PrincipalID: body.PrincipalID, TenantID: body.TenantID, KeycloakClientID: body.KeycloakClientID,
		PrincipalType: body.PrincipalType, Status: body.Status, RecordVersion: body.RecordVersion, Credentials: creds,
	}, nil
}
