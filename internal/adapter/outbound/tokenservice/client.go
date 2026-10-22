// Package tokenservice implements port.TokenServiceClient — this
// service's outbound HTTP dependency on the Token Service's
// credential-lifecycle API (TS-1..TS-4 and TS-7, §2.5, RP-17/RP-18). RP mints
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"

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
// ErrServiceAccountNotProvisioned (principal not yet registered), 400/422 as
// a plain error surfacing the RFC 9457 problem's errorCode (decoded with
// platform-gincommon's problem.FromResponse), any other >=300 as a plain
// unexpected-status error. Returns nil for a 2xx status. Callers that need
// to special-case a specific status first (IssueOrRotateCredential's 409)
// check that before falling through here.
func classifyStatus(resp *http.Response, endpoint string) error {
	status := resp.StatusCode
	if status >= 500 {
		return domain.NewError(domain.ErrTokenServiceUnavailable, "token service unavailable").WithDetails(map[string]any{"status": status})
	}
	if status == http.StatusNotFound {
		return domain.NewError(domain.ErrServiceAccountNotProvisioned, "token service: principal not found").
			WithDetails(map[string]any{"endpoint": endpoint})
	}
	if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
		if p, err := problem.FromResponse(resp); err == nil && p.ErrorCode != "" {
			return fmt.Errorf("tokenservice: %s rejected request — %s", endpoint, p.ErrorCode)
		}
		return fmt.Errorf("tokenservice: %s rejected request (status %d)", endpoint, status)
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

	if resp.StatusCode == http.StatusConflict {
		return nil, domain.NewError(domain.ErrServiceAccountRotationInFlight,
			"token service: principal registration conflict — concurrent write in progress").
			WithDetails(map[string]any{"tenant_id": tenantID})
	}
	if err := classifyStatus(resp, "TS-4"); err != nil {
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

// ts1ConflictCodes maps the RFC 9457 errorCode values Token Service's TS-1
// sends on a 409 (its internal/adapter/inbound/http/errors.go problem
// types, Token Service LLD §17) to this service's domain codes. A 409 whose
// errorCode is absent from this map — or whose body is not a problem at
// all — falls back to ErrServiceAccountRotationInFlight.
var ts1ConflictCodes = map[string]domain.ErrorCode{
	"ROTATION_IN_FLIGHT":        domain.ErrServiceAccountRotationInFlight,
	"OPTIMISTIC_LOCK_CONFLICT":  domain.ErrOptimisticLockConflict,
	"CREDENTIAL_REPLAY_REVOKED": domain.ErrCredentialReplayRevoked,
	"CREDENTIAL_REPLAY_EXPIRED": domain.ErrCredentialReplayExpired,
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
// A 409 is surfaced as one of the four §17 causes when TS-1's RFC 9457
// problem body names it explicitly in errorCode (ts1ConflictCodes) — domain.ErrOptimisticLockConflict (record_version
// mismatch), domain.ErrCredentialReplayRevoked (TS-D13: a replayed
// rotation_id whose credential has since been revoked),
// domain.ErrCredentialReplayExpired (TS-D22: a replayed rotation_id past
// ROTATION_REPLAY_WINDOW), or domain.ErrServiceAccountRotationInFlight
// (concurrent rotation on the same principal). An empty/unparseable/
// non-problem 409 body, or an unknown errorCode, falls back to ErrServiceAccountRotationInFlight as the safe retryable default.
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
		var upstream string
		if p, err := problem.FromResponse(resp); err == nil { // best-effort; a malformed/empty/non-problem body falls back to the safe default below
			upstream = p.ErrorCode
		}
		code, ok := ts1ConflictCodes[upstream]
		if !ok {
			code = domain.ErrServiceAccountRotationInFlight
		}
		return nil, domain.NewError(code, "token service credential issue/rotate conflict").
			WithDetails(map[string]any{"tenant_id": tenantID, "principal_id": principalID, "upstream_error": upstream})
	}
	if err := classifyStatus(resp, "TS-1"); err != nil {
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

// RevokeCredential calls TS-2 to retire the rotating credential for principalID —
// used during RP-3 conversion after TS-1 has issued the new dedicated-realm
// credential, so the old shared-realm credential stops authenticating immediately
// rather than waiting for the overlap sweep. TS-2 requires an explicit version,
// so this first reads the principal via TS-3 to locate the rotating credential,
// then calls POST .../credentials/:version/revoke for it. Returns nil if no
// rotating credential exists (idempotent: the overlap sweep may have beaten us).
func (c *Client) RevokeCredential(ctx context.Context, tenantID, principalID uuid.UUID) error {
	principal, err := c.ReadPrincipal(ctx, tenantID, principalID)
	if err != nil {
		return fmt.Errorf("tokenservice: RevokeCredential TS-3 lookup: %w", err)
	}
	for _, cred := range principal.Credentials {
		if cred.Status != "rotating" {
			continue
		}
		if err := c.revokeVersion(ctx, tenantID, principalID, cred.Version); err != nil {
			return err
		}
	}
	return nil
}

// principalNotFoundCode is the RFC 9457 errorCode Token Service sends with a
// 404 when the tenant has no automation principal (its §17
// PRINCIPAL_NOT_FOUND).
const principalNotFoundCode = "PRINCIPAL_NOT_FOUND"

// ErrTS7Unavailable marks a TS-7 404 that is not Token Service's
// PRINCIPAL_NOT_FOUND problem — the route itself is missing (a Token Service
// deployed before TS-7) or something in front of it answered. Treating it as
// "already gone" would leave the principal active, so it is an error and the
// reap rolls back.
var ErrTS7Unavailable = errors.New("tokenservice: TS-7 returned 404 without PRINCIPAL_NOT_FOUND — the route is missing (Token Service predates TS-7?)")

// RevokePrincipal calls TS-7
// POST /api/v1/internal/tenants/:id/service-accounts/platform-automation/revoke
// (no body): Token Service revokes every credential version of the tenant's
// automation principal, deletes its OpenBao material, unschedules rotation and
// sets the principal `revoked`. 2xx (including an already-revoked principal)
// is success. A 404 is success only when its problem errorCode is
// PRINCIPAL_NOT_FOUND (no principal, e.g. RP-1 never registered one or
// offboarding purged it); any other 404 is ErrTS7Unavailable. A 409 maps to
// ErrServiceAccountRotationInFlight or ErrOptimisticLockConflict (both
// retryable); everything else goes through classifyStatus.
func (c *Client) RevokePrincipal(ctx context.Context, tenantID uuid.UUID) error {
	url := fmt.Sprintf("%s/api/v1/internal/tenants/%s/service-accounts/platform-automation/revoke", c.baseURL, tenantID)
	httpReq, err := c.newRequest(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(httpReq, tenantID) //nolint:bodyclose // closed via the shared httpx.CloseBody helper below
	if err != nil {
		return err
	}
	defer httpx.CloseBody(c.log, resp.Body)

	switch resp.StatusCode {
	case http.StatusNotFound:
		if p, err := problem.FromResponse(resp); err == nil && p.ErrorCode == principalNotFoundCode {
			return nil
		}
		return ErrTS7Unavailable
	case http.StatusConflict:
		var upstream string
		if p, err := problem.FromResponse(resp); err == nil {
			upstream = p.ErrorCode
		}
		code := domain.ErrServiceAccountRotationInFlight
		if upstream == "OPTIMISTIC_LOCK_CONFLICT" {
			code = domain.ErrOptimisticLockConflict
		}
		return domain.NewError(code, "token service principal revoke conflict").
			WithDetails(map[string]any{"tenant_id": tenantID, "upstream_error": upstream})
	}
	return classifyStatus(resp, "TS-7")
}

// revokeVersion calls TS-2 POST .../credentials/:version/revoke for a single
// known version — the inner step of RevokeCredential once the rotating version
// has been resolved via TS-3.
func (c *Client) revokeVersion(ctx context.Context, tenantID, principalID uuid.UUID, version int) error {
	url := fmt.Sprintf("%s/api/v1/internal/tenants/%s/service-accounts/%s/credentials/%d/revoke",
		c.baseURL, tenantID, principalID, version)
	httpReq, err := c.newRequest(ctx, http.MethodPost, url, struct{}{})
	if err != nil {
		return err
	}
	resp, err := c.do(httpReq, tenantID) //nolint:bodyclose // closed via the shared httpx.CloseBody helper below
	if err != nil {
		return err
	}
	defer httpx.CloseBody(c.log, resp.Body)
	return classifyStatus(resp, "TS-2")
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

	if err := classifyStatus(resp, "TS-3"); err != nil {
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
