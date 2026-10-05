// Package auditlog implements port.AuditLogClient — this service's
// outbound HTTP dependency on the Audit Log Service's AL-5 direct-write
// ingest (POST /api/v1/internal/audit-entries). It carries the non-bus
// TenantIdpConfigChanged entry (HLD §9.4, §10.6), which is deliberately
// not one of the 9 iam-tenant-events bus events.
package auditlog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// maxErrorBody bounds how much of a rejection body is read into the error.
const maxErrorBody = 512

// maxRetryAfter caps how far a Retry-After hint may push a row's next
// attempt — a hostile or misconfigured upstream can't stall delivery
// beyond ClaimDue's own 5-minute exponential cap.
const maxRetryAfter = 5 * time.Minute

// parseRetryAfter interprets a Retry-After header value per RFC 7231
// §7.1.3 — either delta-seconds or an HTTP-date. Returns 0 for an empty,
// malformed, or non-positive value. now is injected for testability.
func parseRetryAfter(header string, now time.Time) time.Duration {
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(header); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// Client implements port.AuditLogClient.
type Client struct {
	baseURL string
	http    *http.Client
	log     port.Logger
}

var _ port.AuditLogClient = (*Client)(nil)

// New constructs a Client calling Audit Log's internal API at baseURL.
func New(baseURL string, log port.Logger) *Client {
	return &Client{baseURL: baseURL, http: httpx.NewClient(5 * time.Second), log: log}
}

// WriteEntry calls AL-5. 201 (created) and 200 (idempotent replay of
// idempotencyKey) are success; 429 and 5xx are transient; any other
// non-2xx is a permanent rejection wrapping port.ErrAuditEntryRejected.
func (c *Client) WriteEntry(ctx context.Context, idempotencyKey string, entry domain.AuditEntry) error {
	if entry.TenantID == (uuid.UUID{}) {
		return fmt.Errorf("%w: nil tenant_id in audit entry", port.ErrAuditEntryRejected)
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("%w: encode entry: %v", port.ErrAuditEntryRejected, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/internal/audit-entries", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	req.Header.Set("x-user-id", domain.SystemActorID)
	req.Header.Set("x-tenant-id", entry.TenantID.String())
	req.Header.Set("x-tenant-roles", "iam-system")

	resp, err := c.http.Do(req) //nolint:bodyclose // closed via the shared httpx.CloseBody helper below, which the linter's cross-package analysis doesn't trace
	if err != nil {
		return fmt.Errorf("auditlog: AL-5 request: %w", err)
	}
	defer httpx.CloseBody(c.log, resp.Body)

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		base := fmt.Errorf("auditlog: AL-5 unavailable (status %d)", resp.StatusCode)
		if d := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); d > 0 {
			if d > maxRetryAfter {
				d = maxRetryAfter
			}
			return &port.RetryAfterError{Underlying: base, RetryAfter: d}
		}
		return base
	case resp.StatusCode >= 500:
		return fmt.Errorf("auditlog: AL-5 unavailable (status %d)", resp.StatusCode)
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody)) //nolint:errcheck // best-effort diagnostic only
		return fmt.Errorf("%w: AL-5 status %d: %s", port.ErrAuditEntryRejected, resp.StatusCode, bytes.TrimSpace(snippet))
	}
}
