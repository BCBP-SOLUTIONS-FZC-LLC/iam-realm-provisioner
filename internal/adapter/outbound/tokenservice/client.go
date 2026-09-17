// Package tokenservice implements port.TokenServiceClient — this
// service's outbound HTTP dependency on the Token Service's
// credential-lifecycle API (TS-1/TS-3/TS-4, §2.5, RP-17/RP-18). RP mints
// the Keycloak client and applies the secret Token Service returns;
// Token Service generates and custodies the secret material and never
// touches Keycloak (RP-INV-1).
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

// systemPrincipalUserID is the reserved iam-system caller identity Token
// Service's GUCBridgeMiddleware requires as x-user-id on every
// /api/v1/internal/* route (mirrors orgmembership's identical constant).
const systemPrincipalUserID = "00000000-0000-0000-0000-0000000000a1"

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
	req.Header.Set("x-user-id", systemPrincipalUserID)
	req.Header.Set("x-tenant-id", tenantID.String())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, domain.NewError(domain.ErrTokenServiceUnavailable, "token service unavailable").WithDetails(map[string]any{"error": err.Error()})
	}
	return resp, nil
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

	resp, err := c.do(httpReq, tenantID)
	if err != nil {
		return nil, err
	}
	defer closeBody(c.log, resp.Body)

	if resp.StatusCode >= 500 {
		return nil, domain.NewError(domain.ErrTokenServiceUnavailable, "token service unavailable").WithDetails(map[string]any{"status": resp.StatusCode})
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tokenservice: unexpected status %d from TS-4", resp.StatusCode)
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

type issueOrRotateResponseBody struct {
	Version        int     `json:"version"`
	Secret         string  `json:"secret"`
	OpenBaoPath    string  `json:"openbao_path"`
	ExpiresPriorAt *string `json:"expires_prior_at,omitempty"`
	RecordVersion  int     `json:"record_version"`
}

// IssueOrRotateCredential calls TS-1
// POST /api/v1/internal/tenants/:id/service-accounts/:principal_id/credentials.
// A 409 is surfaced as domain.ErrServiceAccountRotationInFlight or
// domain.ErrOptimisticLockConflict (TS-1's own two 409 causes) rather than
// a generic error, so callers can decide retry-vs-fail.
func (c *Client) IssueOrRotateCredential(ctx context.Context, tenantID, principalID uuid.UUID, req port.IssueCredentialRequest) (*port.IssueCredentialResult, error) {
	url := fmt.Sprintf("%s/api/v1/internal/tenants/%s/service-accounts/%s/credentials", c.baseURL, tenantID, principalID)
	httpReq, err := c.newRequest(ctx, http.MethodPost, url, issueOrRotateRequestBody{
		RotationID: req.RotationID.String(), OverlapSeconds: req.OverlapSeconds,
	})
	if err != nil {
		return nil, err
	}

	resp, err := c.do(httpReq, tenantID)
	if err != nil {
		return nil, err
	}
	defer closeBody(c.log, resp.Body)

	if resp.StatusCode == http.StatusConflict {
		return nil, domain.NewError(domain.ErrServiceAccountRotationInFlight, "token service credential issue/rotate conflict").
			WithDetails(map[string]any{"tenant_id": tenantID, "principal_id": principalID})
	}
	if resp.StatusCode >= 500 {
		return nil, domain.NewError(domain.ErrTokenServiceUnavailable, "token service unavailable").WithDetails(map[string]any{"status": resp.StatusCode})
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tokenservice: unexpected status %d from TS-1", resp.StatusCode)
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
	Version     int     `json:"version"`
	Status      string  `json:"status"`
	OpenBaoPath string  `json:"openbao_path"`
	IssuedAt    string  `json:"issued_at"`
	ExpiresAt   *string `json:"expires_at,omitempty"`
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

	resp, err := c.do(httpReq, tenantID)
	if err != nil {
		return nil, err
	}
	defer closeBody(c.log, resp.Body)

	if resp.StatusCode >= 500 {
		return nil, domain.NewError(domain.ErrTokenServiceUnavailable, "token service unavailable").WithDetails(map[string]any{"status": resp.StatusCode})
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tokenservice: unexpected status %d from TS-3", resp.StatusCode)
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
		creds = append(creds, port.CredentialSummary{
			Version: cr.Version, Status: cr.Status, OpenBaoPath: cr.OpenBaoPath, IssuedAt: issuedAt, ExpiresAt: expiresAt,
		})
	}
	return &port.ReadPrincipalResult{
		PrincipalID: body.PrincipalID, TenantID: body.TenantID, KeycloakClientID: body.KeycloakClientID,
		PrincipalType: body.PrincipalType, Status: body.Status, RecordVersion: body.RecordVersion, Credentials: creds,
	}, nil
}

// closeBody closes body, logging (not propagating) a failure — mirrors
// orgmembership's identical helper.
func closeBody(log port.Logger, body io.Closer) {
	if err := body.Close(); err != nil {
		log.Warn("tokenservice: close response body failed", map[string]any{"error": err.Error()})
	}
}
